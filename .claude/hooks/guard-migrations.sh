#!/usr/bin/env bash
# PreToolUse(Edit|Write|MultiEdit) hook: enforce forward-only migrations.
#
# Migrations in internal/store/migrations/*.sql are applied in lexical order and
# tracked in schema_migrations; editing an *already-existing* one would diverge
# from what's been applied. This hook blocks edits to an existing migration file
# while still allowing a brand-new file (Write to a path that doesn't exist yet)
# — that's how /new-migration scaffolds the next NNNN_ file.
#
# Exit 0 (allow) for everything else. A non-zero exit with stderr blocks the call.
set -uo pipefail

input="$(cat)"

if command -v jq >/dev/null 2>&1; then
  file="$(printf '%s' "$input" | jq -r '.tool_input.file_path // empty')"
else
  file="$(printf '%s' "$input" | python3 -c 'import json,sys; print(json.load(sys.stdin).get("tool_input",{}).get("file_path",""))' 2>/dev/null)"
fi

[ -z "${file:-}" ] && exit 0

case "$file" in
  *internal/store/migrations/*.sql)
    # Only block if the migration already exists (i.e. an edit to applied SQL).
    # Creating a new migration file is allowed.
    if [ -f "$file" ]; then
      echo "Forward-only migrations: '$file' already exists and must not be edited." >&2
      echo "Add a NEW migration instead (use the /new-migration skill)." >&2
      exit 2
    fi
    ;;
esac
exit 0
