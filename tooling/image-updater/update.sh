#!/bin/bash
set -euo pipefail

# Configuration
CONFIG_FILE="config.yaml"
MAIN_BRANCH="main"

# GitHub Actions workflow URL for PR description
if [ -n "${GITHUB_SERVER_URL:-}" ] && [ -n "${GITHUB_REPOSITORY:-}" ] && [ -n "${GITHUB_RUN_ID:-}" ]; then
    WORKFLOW_URL="${GITHUB_SERVER_URL}/${GITHUB_REPOSITORY}/actions/runs/${GITHUB_RUN_ID}"
    AUTOMATION_CREDIT="Automatically updated with [Image Digest Updater](${WORKFLOW_URL})"
else
    # Fallback for local runs or when GitHub env vars are not available
    AUTOMATION_CREDIT="Automatically updated with Image Digest Updater"
fi

# Logging helper
log() {
    echo "[$(date +'%H:%M:%S')] $1"
}

# Extract component names from the images section of config file
get_components() {
    awk '/^images:/{flag=1;next} /^[^ ]/{flag=0} flag && /^  [a-zA-Z]/{print $1}' "$CONFIG_FILE" | sed 's/://'
}

# Get all open PRs for a component (any digest suffix)
get_component_prs() {
    local component="$1"
    gh pr list --state open --json number,headRefName --jq '.[] | select(.headRefName | startswith("auto-update-'$component'-")) | {number: .number, branch: .headRefName}' 2>/dev/null || echo ""
}

# Extract digest hash from latest image
get_digest_hash() {
    local digest="$1"
    echo "$digest" | grep -o 'sha256:[a-f0-9]\{64\}' | cut -d: -f2 | cut -c1-7 || echo ""
}

# Get the diff from an existing PR
get_pr_diff() {
    local pr_number="$1"
    gh pr diff "$pr_number" 2>/dev/null || echo ""
}

# Close an existing PR in favor of a new one
close_pr() {
    local old_pr_number="$1"
    local new_pr_number="$2"
    local component="$3"
    log "Closing existing PR #$old_pr_number for $component in favor of #$new_pr_number"
    gh pr close "$old_pr_number" --comment "Closing in favor of #$new_pr_number"
}

# Update a single component
update_component() {
    local component="$1"

    log "Processing component: $component"

    # Start from main branch
    git checkout "$MAIN_BRANCH"

    # Run image updater for this component only
    if ! ./image-updater update --config "$CONFIG_FILE" --component "$component"; then
        log "❌ Image updater failed for $component"
        return 1
    fi

    # Format configs using project's yamlfmt
    log "Formatting configs with yamlfmt"
    (cd ../.. && make yamlfmt) || {
        log "❌ yamlfmt failed for $component"
        return 1
    }

    # Check if there are any changes to commit
    if git diff --quiet; then
        log "No changes detected for $component, skipping"
        return 0
    fi
    make -C ../../config materialize
    # Extract the new digest from the changes (before committing)
    local new_digest
    new_digest=$(git diff | grep "^+.*digest:" | head -1 | sed 's/^+.*digest: *//g' | tr -d '"' || echo "")

    if [ -z "$new_digest" ]; then
        log "❌ Could not extract digest from changes for $component"
        git checkout .  # Reset changes
        return 1
    fi

    # Get digest hash for branch naming
    local digest_hash
    digest_hash=$(get_digest_hash "$new_digest")

    if [ -z "$digest_hash" ]; then
        log "❌ Could not extract hash from digest for $component"
        git checkout .  # Reset changes
        return 1
    fi

    # Create the branch with digest suffix
    local branch="auto-update-${component}-${digest_hash}"
    log "Creating branch: $branch"

    # Clean up any existing branch with same name
    git branch -D "$branch" 2>/dev/null || true

    # Create the new branch (this will include our current changes)
    git checkout -b "$branch"

    # Check if a PR with this exact digest already exists
    local existing_prs
    existing_prs=$(get_component_prs "$component")

    local existing_branch_for_digest=""
    if [ -n "$existing_prs" ]; then
        existing_branch_for_digest=$(echo "$existing_prs" | jq -r --arg branch "$branch" 'select(.branch == $branch) | .number' 2>/dev/null | head -1 || echo "")
    fi

    if [ -n "$existing_branch_for_digest" ]; then
        log "PR already exists for this exact digest (#$existing_branch_for_digest), skipping"
        git checkout "$MAIN_BRANCH"
        git branch -D "$branch" 2>/dev/null || true
        return 0
    fi

    # Show what changed
    log "Changes detected:"
    git diff --stat

    git add config/

    # Commit changes with digest hash
    git commit -m "Update $component image digest to ${digest_hash}

Automatically updated $component to latest image digest: ${new_digest}"

    # Push branch
    git push origin "$branch" --force-with-lease

    # Create new PR with digest hash in title
    local new_pr_url
    if new_pr_url=$(gh pr create \
        --title "Update $component image digest to ${digest_hash}" \
        --body "${AUTOMATION_CREDIT}

**Changes:**
- Updated image digest for $component component
- New digest: \`${new_digest}\`" \
        --base "$MAIN_BRANCH" \
        --head "$branch" 2>/dev/null); then

        local new_pr_number
        new_pr_number=$(echo "$new_pr_url" | grep -o '[0-9]\+$')
        log "✅ Created PR #$new_pr_number for $component (digest: ${digest_hash})"

        # Close all other open PRs for this component
        if [ -n "$existing_prs" ]; then
            echo "$existing_prs" | jq -r '.number' 2>/dev/null | while read -r pr_number; do
                if [ "$pr_number" != "$new_pr_number" ]; then
                    log "Closing older PR #$pr_number for $component"
                    close_pr "$pr_number" "$new_pr_number" "$component"
                fi
            done
        fi
    else
        log "❌ Failed to create PR for $component"
        git checkout "$MAIN_BRANCH"
        return 1
    fi

    # Return to main branch
    git checkout "$MAIN_BRANCH"
}

# Main function
main() {
    # Change to script directory
    cd "$(dirname "$0")"

    log "Starting automated image update process"

    # Check prerequisites
    if ! command -v gh >/dev/null 2>&1; then
        log "❌ GitHub CLI (gh) is required but not installed"
        exit 1
    fi

    # Build image-updater if needed
    if [ ! -f "./image-updater" ]; then
        log "Building image-updater..."
        go build -o image-updater
    fi

    # Ensure we're on main and up to date
    log "Syncing with $MAIN_BRANCH branch"
    git checkout "$MAIN_BRANCH"
    git pull origin "$MAIN_BRANCH"

    # Extract and process components (filter to quay.io only for fork testing)
    all_components=$(get_components)
    components="maestro hypershift"  # Only process quay.io images for fork testing

    if [ -z "$components" ]; then
        log "❌ No components found in $CONFIG_FILE"
        exit 1
    fi

    log "All available components: $(echo $all_components | tr '\n' ' ')"
    log "Processing components (quay.io only): $components"

    # Process each component
    success_count=0
    total_count=0

    for component in $components; do
        total_count=$((total_count + 1))
        if update_component "$component"; then
            success_count=$((success_count + 1))
        fi
    done

    log "🎉 Completed: $success_count/$total_count components processed successfully"

    # Return to main branch
    git checkout "$MAIN_BRANCH"
}

# Run main function if script is executed directly
if [[ "${BASH_SOURCE[0]}" == "${0}" ]]; then
    main "$@"
fi