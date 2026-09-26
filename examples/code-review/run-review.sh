#!/usr/bin/env bash
set -euo pipefail

PROVIDER="${BRIDGECTL_REVIEW_PROVIDER:-codex}"
IMAGE="${BRIDGECTL_IMAGE:-ghcr.io/orchael/bridgectl:latest}"
REPO="${1:-$PWD}"
PROMPT_FILE="${BRIDGECTL_REVIEW_PROMPT:-$(dirname "$0")/review-prompt.md}"

REPO="$(cd "$REPO" && pwd)"
git -C "$REPO" rev-parse --is-inside-work-tree >/dev/null
BRANCH="$(git -C "$REPO" branch --show-current)"
[ -n "$BRANCH" ] || { echo "code review requires a checked-out branch" >&2; exit 2; }
[ -f "$PROMPT_FILE" ] || { echo "prompt not found: $PROMPT_FILE" >&2; exit 2; }

case "$PROVIDER" in
  codex|opencode)
    [ -n "${OPENAI_API_KEY:-}" ] || { echo "OPENAI_API_KEY is required for $PROVIDER" >&2; exit 2; }
    AUTH_ARGS=(-e OPENAI_API_KEY)
    ;;
  claude)
    [ -n "${CLAUDE_CODE_OAUTH_TOKEN:-}" ] || { echo "CLAUDE_CODE_OAUTH_TOKEN is required for claude" >&2; exit 2; }
    AUTH_ARGS=(-e CLAUDE_CODE_OAUTH_TOKEN)
    ;;
  *)
    echo "unsupported review provider: $PROVIDER" >&2; exit 2 ;;
esac

echo "Reviewing $BRANCH with $PROVIDER" >&2
docker run --rm -i   --name "bridgectl-review-${BRANCH//[^a-zA-Z0-9_.-]/-}-$$"   -v "$REPO:/repos/workspace"   -w /repos/workspace   "${AUTH_ARGS[@]}"   -e BRIDGECTL_REVIEW_BRANCH="$BRANCH"   "$IMAGE"   bridgectl run --no-tty --provider "$PROVIDER" --project code-review /repos/workspace   < "$PROMPT_FILE"
