#!/usr/bin/env bash
# PreToolUse(Edit|Write|MultiEdit) hook: keep secrets and local runtime state out
# of the tree. Blocks writing the control token, SQLite DBs, and .env files —
# these are gitignored and arrive via RABBOT_-prefixed env vars, never the repo.
#
# Exit 0 (allow) otherwise. A non-zero exit with stderr blocks the call.
set -uo pipefail

input="$(cat)"

if command -v jq >/dev/null 2>&1; then
  file="$(printf '%s' "$input" | jq -r '.tool_input.file_path // empty')"
else
  file="$(printf '%s' "$input" | python3 -c 'import json,sys; print(json.load(sys.stdin).get("tool_input",{}).get("file_path",""))' 2>/dev/null)"
fi

[ -z "${file:-}" ] && exit 0

base="$(basename "$file")"
case "$base" in
  control.token|*.db|*.db-wal|*.db-shm|.env|.env.*)
    echo "Refusing to write '$file': secrets / local runtime state must not enter the repo." >&2
    echo "Secrets arrive via RABBOT_-prefixed env vars; these paths are gitignored." >&2
    exit 2
    ;;
esac
exit 0
