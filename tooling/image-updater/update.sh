#!/bin/bash
set -euo pipefail

CREATE_PR=true

if [ -n "${GITHUB_SERVER_URL:-}" ] && [ -n "${GITHUB_REPOSITORY:-}" ] && [ -n "${GITHUB_RUN_ID:-}" ]; then
    WORKFLOW_URL="${GITHUB_SERVER_URL}/${GITHUB_REPOSITORY}/actions/runs/${GITHUB_RUN_ID}"
    AUTOMATION_CREDIT="Automatically updated with [Image Digest Updater](${WORKFLOW_URL})"
else
    # Fallback for local runs or when GitHub env vars are not available
    AUTOMATION_CREDIT="Automatically updated with Image Digest Updater"
fi

log() {
    echo "[$(date +'%H:%M:%S')] $1"
}

usage() {
    cat << EOF
Usage: $0 [OPTIONS]

Bulk update all component image digests and create a single PR.

OPTIONS:
    --no-pr     Skip PR creation (useful for local testing)
    -h, --help  Show this help message

EXAMPLES:
    $0                  # Update all components and create PR
    $0 --no-pr         # Update all components but don't create PR
EOF
}

parse_args() {
    while [[ $# -gt 0 ]]; do
        case $1 in
            --no-pr)
                CREATE_PR=false
                shift
                ;;
            -h|--help)
                usage
                exit 0
                ;;
            *)
                log "❌ Unknown option: $1"
                usage
                exit 1
                ;;
        esac
    done
}

get_components() {
    awk '/^images:/{flag=1;next} /^[^ ]/{flag=0} flag && /^  [a-zA-Z]/{print $1}' "$CONFIG_FILE" | sed 's/://'
}

get_auto_update_prs() {
    gh pr list --state open --json number,headRefName --jq '.[] | select(.headRefName | startswith("auto-update")) | {number: .number, branch: .headRefName}' 2>/dev/null || echo ""
}

close_existing_prs() {
    local new_pr_number="$1"
    local existing_prs
    existing_prs=$(get_auto_update_prs)

    if [ -n "$existing_prs" ]; then
        echo "$existing_prs" | jq -r '.number' 2>/dev/null | while read -r pr_number; do
            if [ "$pr_number" != "$new_pr_number" ]; then
                log "Closing existing auto-update PR #$pr_number"
                gh pr close "$pr_number" --comment "Superseded by PR #$new_pr_number"
            fi
        done
    fi
}

get_component_digest_changes() {
    local changes=""
    local current_component=""

    # Parse git diff to extract component names and their new digests
    while IFS= read -r line; do
        # Look for file paths that indicate component (like config/config.yaml changes)
        if [[ $line =~ ^diff.*config\.yaml$ ]] || [[ $line =~ ^diff.*config\..*\.yaml$ ]]; then
            continue
        fi

        # Look for component sections in the diff (e.g., "  arohcpfrontend:")
        if [[ $line =~ ^[[:space:]]*[a-zA-Z][a-zA-Z0-9_-]*:[[:space:]]*$ ]]; then
            current_component=$(echo "$line" | sed 's/^[[:space:]]*\([a-zA-Z][a-zA-Z0-9_-]*\):[[:space:]]*$/\1/')
        fi

        # Look for digest changes and associate with current component
        if [[ $line =~ ^\+.*digest:.*$ ]] && [[ -n "$current_component" ]]; then
            local digest=$(echo "$line" | sed 's/^+.*digest: *//g' | tr -d '"')
            if [[ -n "$digest" ]]; then
                changes="${changes}- ${current_component}: ${digest}\n"
            fi
            current_component=""  # Reset after finding digest
        fi
    done < <(git diff --cached)

    echo -e "$changes"
}

bulk_update() {
    log "Starting bulk image digest update process"

    cd "$(dirname "$0")"

    log "Running image updater for all components"
    if ! make -C . update; then
        log "❌ Image updater failed"
        return 1
    fi

    log "Formatting configs with yamlfmt"
    if ! make -C ../.. yamlfmt; then
        log "❌ yamlfmt failed"
        return 1
    fi

    log "Materializing configs"
    if ! make -C ../../config materialize; then
        log "❌ Config materialization failed"
        return 1
    fi

    if git diff --quiet; then
        log "No changes detected, nothing to update"
        return 0
    fi

    log "Changes detected:"
    git diff --stat

    git add --all

    if [ "$CREATE_PR" = true ]; then
        local branch="auto-update-all-components-$(date +%Y%m%d)"

        log "Creating branch: $branch"
        git checkout -b "$branch"

        # Get component digest changes for commit message
        local component_changes
        component_changes=$(get_component_digest_changes)

        local commit_msg="Updated image digest for dev and int

$component_changes
$AUTOMATION_CREDIT"

        git commit -m "$commit_msg"

        git push origin "$branch" --force-with-lease

        local pr_body="${AUTOMATION_CREDIT}

**Component Image Digest Update**

This PR updates image digests for dev and int environments.

**Changes:**
${component_changes}"

        local new_pr_url
        if new_pr_url=$(gh pr create \
            --title "Auto bump component image digests ($(date +'%Y-%m-%d %H:%M'))" \
            --body "$pr_body" \
            --base main \
            --head "$branch" 2>/dev/null); then

            local new_pr_number
            new_pr_number=$(echo "$new_pr_url" | grep -o '[0-9]\+$')
            log "✅ Created PR #$new_pr_number"

            close_existing_prs "$new_pr_number"
        else
            log "❌ Failed to create PR"
            git checkout main
            return 1
        fi

        git checkout main
    else
        log "✅ Changes staged successfully (--no-pr flag used, skipping PR creation)"
        log "To commit these changes manually:"
        log "  git commit -m 'Update component image digests'"
        git checkout main
    fi
}

main() {
    parse_args "$@"

    if ! command -v gh >/dev/null 2>&1; then
        log "❌ GitHub CLI (gh) is required but not installed"
        exit 1
    fi

    if [ ! -f "./image-updater" ]; then
        log "Building image-updater..."
        go build -o image-updater
    fi

    if bulk_update; then
        log "🎉 Bulk image update completed successfully"
    else
        log "❌ Bulk image update failed"
        exit 1
    fi
}

if [[ "${BASH_SOURCE[0]}" == "${0}" ]]; then
    main "$@"
fi