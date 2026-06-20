#!/usr/bin/env bash
# PostToolUse hook: keep the tree gofmt-clean by formatting any edited .go file.
# Reads the tool-call JSON on stdin, extracts the edited path, and runs gofmt -w.
# Non-Go edits and missing files are no-ops. Never fails the tool call.
set -uo pipefail

input="$(cat)"

# Prefer jq; fall back to a tiny python parser so the hook works without jq.
if command -v jq >/dev/null 2>&1; then
  file="$(printf '%s' "$input" | jq -r '.tool_input.file_path // empty')"
else
  file="$(printf '%s' "$input" | python3 -c 'import json,sys; print(json.load(sys.stdin).get("tool_input",{}).get("file_path",""))' 2>/dev/null)"
fi

[ -z "${file:-}" ] && exit 0

case "$file" in
  *.go)
    [ -f "$file" ] || exit 0
    gofmt="$(command -v gofmt || echo "$HOME/.local/bin/gofmt")"
    "$gofmt" -w "$file" 2>/dev/null || true
    ;;
esac
exit 0
