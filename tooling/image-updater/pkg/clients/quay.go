// Copyright 2025 Microsoft Corporation
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package clients

import (
	"encoding/json"
	"fmt"
	"net/http"
	"regexp"
	"strings"
	"time"
)

// QuayClient provides methods to interact with Quay.io API
type QuayClient struct {
	httpClient *http.Client
	baseURL    string
}

// NewQuayClient creates a new Quay.io API client
func NewQuayClient() *QuayClient {
	return &QuayClient{
		httpClient: &http.Client{
			Timeout: 30 * time.Second,
		},
		baseURL: "https://quay.io/api/v1",
	}
}

// QuayTag represents a tag from the Quay.io API response
type QuayTag struct {
	Name           string `json:"name"`
	ManifestDigest string `json:"manifest_digest"`
	LastModified   string `json:"last_modified"`
}

// QuayTagsResponse represents the response from Quay.io tags API
type QuayTagsResponse struct {
	Tags          []QuayTag `json:"tags"`
	Page          int       `json:"page"`
	HasAdditional bool      `json:"has_additional"`
}

func (c *QuayClient) GetLatestDigest(repository string, tagPattern string) (string, error) {
	fmt.Printf("  Using tag pattern: %s\n", tagPattern)
	return c.getDigestByTagPattern(repository, tagPattern)
}

// tryGetLatestTag efficiently checks for a "latest" tag without full pagination
func (c *QuayClient) tryGetLatestTag(repository string) (string, bool, error) {
	url := fmt.Sprintf("%s/repository/%s/tag?page=1", c.baseURL, repository)

	resp, err := c.httpClient.Get(url)
	if err != nil {
		return "", false, fmt.Errorf("failed to request Quay.io API: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return "", false, fmt.Errorf("Quay.io API returned status %d for repository %s", resp.StatusCode, repository)
	}

	var tagsResp QuayTagsResponse
	if err := json.NewDecoder(resp.Body).Decode(&tagsResp); err != nil {
		return "", false, fmt.Errorf("failed to decode Quay.io API response: %w", err)
	}

	// Look for the "latest" tag in the first page
	for _, tag := range tagsResp.Tags {
		if tag.Name == "latest" {
			if tag.ManifestDigest == "" {
				return "", false, fmt.Errorf("latest tag found but no manifest digest available for repository %s", repository)
			}
			return tag.ManifestDigest, true, nil
		}
	}

	return "", false, nil // "latest" tag not found
}

// getDigestByTagPattern fetches the latest digest for tags matching the given regex pattern
func (c *QuayClient) getDigestByTagPattern(repository string, tagPattern string) (string, error) {
	// Compile the regex pattern
	regex, err := regexp.Compile(tagPattern)
	if err != nil {
		return "", fmt.Errorf("invalid tag pattern %s: %w", tagPattern, err)
	}

	// Get all tags and filter by pattern
	tags, err := c.getTags(repository)
	if err != nil {
		return "", fmt.Errorf("failed to fetch all tags: %w", err)
	}

	// Find the most recent tag matching the pattern
	var mostRecent *QuayTag
	matchCount := 0
	for _, tag := range tags {
		// Check if tag matches the pattern
		if !regex.MatchString(tag.Name) {
			continue
		}

		matchCount++

		// Skip signature and attestation tags
		if isMetadataTag(tag.Name) {
			continue
		}

		if tag.ManifestDigest == "" {
			continue
		}

		if mostRecent == nil || tag.LastModified > mostRecent.LastModified {
			mostRecent = &tag
		}
	}

	fmt.Printf("  Found %d tags matching pattern\n", matchCount)

	if mostRecent == nil {
		return "", fmt.Errorf("no tags matching pattern %s found for repository %s", tagPattern, repository)
	}

	fmt.Printf("  Selected tag: %s\n", mostRecent.Name)
	return mostRecent.ManifestDigest, nil
}

// getTags fetches all tags from all pages for the specified repository
func (c *QuayClient) getTags(repository string) ([]QuayTag, error) {
	var allTags []QuayTag
	maxPages := 5

	for page := range maxPages {
		url := fmt.Sprintf("%s/repository/%s/tag?page=%d", c.baseURL, repository, page)

		resp, err := c.httpClient.Get(url)
		if err != nil {
			return nil, fmt.Errorf("failed to request Quay.io API (page %d): %w", page, err)
		}

		if resp.StatusCode != http.StatusOK {
			resp.Body.Close()
			return nil, fmt.Errorf("quay.io API returned status %d for repository %s (page %d)", resp.StatusCode, repository, page)
		}

		var tagsResp QuayTagsResponse
		if err := json.NewDecoder(resp.Body).Decode(&tagsResp); err != nil {
			resp.Body.Close()
			return nil, fmt.Errorf("failed to decode Quay.io API response (page %d): %w", page, err)
		}
		resp.Body.Close()

		// Add tags from this page
		allTags = append(allTags, tagsResp.Tags...)

		// Check if there are more pages
		if !tagsResp.HasAdditional {
			break
		}
	}

	fmt.Printf("  Fetched %d tags across %d pages\n", len(allTags))
	return allTags, nil
}

// isTemporaryTag checks if a tag name looks temporary or ephemeral
func isTemporaryTag(name string) bool {
	// Skip PR-related tags and build container tags
	if strings.Contains(name, "on-pr-") || strings.Contains(name, "build-container") {
		return true
	}
	return false
}

// hasExpiration checks if a tag has an expiration time
func hasExpiration(_ QuayTag) bool {
	// In the JSON response, expired tags or tags with expiration might have specific fields
	// For now, we'll consider tags temporary if they have very recent timestamps and might expire
	return false // We'd need to parse the actual expiration field if it exists
}

// isMetadataTag checks if a tag is for signatures, attestations, or SBOMs
func isMetadataTag(name string) bool {
	return strings.HasSuffix(name, ".sig") ||
		strings.HasSuffix(name, ".att") ||
		strings.HasSuffix(name, ".sbom")
}
