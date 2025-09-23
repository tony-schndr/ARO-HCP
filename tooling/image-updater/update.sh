#!/bin/bash
set -euo pipefail

# Configuration
CONFIG_FILE="config.yaml"
MAIN_BRANCH="main"

# Logging helper
log() {
    echo "[$(date +'%H:%M:%S')] $1"
}

# Extract component names from the images section of config file
get_components() {
    awk '/^images:/{flag=1;next} /^[^ ]/{flag=0} flag && /^  [a-zA-Z]/{print $1}' "$CONFIG_FILE" | sed 's/://'
}

# Check if PR exists for component and get its number
get_existing_pr() {
    local component="$1"
    local branch="auto-update-${component}"
    gh pr list --head "$branch" --state open --json number --jq '.[0].number' 2>/dev/null || echo ""
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
    local branch="auto-update-${component}"

    log "Processing component: $component"

    # Check for existing PR
    local existing_pr
    existing_pr=$(get_existing_pr "$component")

    # Create/switch to branch (force recreate if exists)
    git checkout "$MAIN_BRANCH"
    git branch -D "$branch" 2>/dev/null || true
    git checkout -b "$branch"

    # Run image updater for this component only
    if ! ./image-updater update --config "$CONFIG_FILE" --component "$component"; then
        log "❌ Image updater failed for $component"
        git checkout "$MAIN_BRANCH"
        return 1
    fi

    # Format configs using project's yamlfmt
    log "Formatting configs with yamlfmt"
    (cd ../.. && make yamlfmt) || {
        log "❌ yamlfmt failed for $component"
        git checkout "$MAIN_BRANCH"
        return 1
    }

    # Check if there are any changes to commit
    if git diff --quiet; then
        log "No changes detected for $component, skipping"
        git checkout "$MAIN_BRANCH"
        return 0
    fi

    # Get current diff for comparison
    local current_diff
    current_diff=$(git diff)

    # If existing PR exists, compare changes
    if [ -n "$existing_pr" ]; then
        log "Found existing PR #$existing_pr for $component"

        local existing_diff
        existing_diff=$(get_pr_diff "$existing_pr")

        # Compare diffs (normalize whitespace)
        if [ "$(echo "$current_diff" | tr -d ' \t\n')" = "$(echo "$existing_diff" | tr -d ' \t\n')" ]; then
            log "Changes are identical to existing PR #$existing_pr, skipping"
            git checkout "$MAIN_BRANCH"
            return 0
        fi

        log "Changes differ from existing PR #$existing_pr, will replace it"
    fi

    # Show what changed
    log "Changes detected:"
    git diff --stat

    # Commit changes
    git add -A
    git commit -m "Update $component image digest

Automatically updated $component to latest image digest.

🤖 Generated with [Claude Code](https://claude.ai/code)

Co-Authored-By: Claude <noreply@anthropic.com>"

    # Push branch
    git push origin "$branch" --force-with-lease

    # Create new PR
    local new_pr_url
    if new_pr_url=$(gh pr create \
        --title "Update $component image digest" \
        --body "Automatically updated \`$component\` to the latest image digest from registry.

**Changes:**
- Updated image digest for $component component

🤖 Generated with [Claude Code](https://claude.ai/code)" \
        --base "$MAIN_BRANCH" \
        --head "$branch" 2>/dev/null); then

        local new_pr_number
        new_pr_number=$(echo "$new_pr_url" | grep -o '[0-9]\+$')
        log "✅ Created PR #$new_pr_number for $component"

        # Close existing PR if it existed
        if [ -n "$existing_pr" ]; then
            close_pr "$existing_pr" "$new_pr_number" "$component"
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

    # Check git status is clean
    if ! git diff --quiet || ! git diff --cached --quiet; then
        log "❌ Working directory is not clean. Please commit or stash changes first."
        exit 1
    fi

    # Extract and process components
    components=$(get_components)
    if [ -z "$components" ]; then
        log "❌ No components found in $CONFIG_FILE"
        exit 1
    fi

    log "Found components: $(echo $components | tr '\n' ' ')"

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