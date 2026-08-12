#!/bin/sh
# Claude Code SessionStart hook for fmsg-mcp (ARCHITECTURE.md §4).
# Receives the hook payload on stdin and records {session_id, transcript_path}
# in a per-project pointer file the MCP server reads to locate the caller's
# transcript. Register in ~/.claude/settings.json:
#   {"hooks": {"SessionStart": [{"hooks": [{"type": "command",
#     "command": "~/.claude/fmsg-mcp/session-start.sh"}]}]}}
set -eu

payload=$(cat)

get() {
    printf '%s' "$payload" | sed -n 's/.*"'"$1"'"[[:space:]]*:[[:space:]]*"\([^"]*\)".*/\1/p' | head -n1
}

session_id=$(get session_id)
transcript_path=$(get transcript_path)
cwd=$(get cwd)
# The payload is JSON, so Windows path separators arrive as \\ — unescape them
# or the slug below gets two dashes per separator and never matches the one
# locator.Slug computes from the real path (readPointer would silently miss
# and Locate would fall back to mtime, picking the wrong transcript when
# several sessions share a project dir). transcript_path must STAY escaped:
# it is re-emitted into JSON below, where a lone \U is an invalid escape.
cwd=$(printf '%s' "$cwd" | sed 's/\\\\/\\/g')
[ -n "$cwd" ] || cwd=$(pwd)
[ -n "$transcript_path" ] || exit 0

# Slug: every non-alphanumeric byte becomes '-', matching Claude Code's
# project-directory naming (internal/locator).
slug=$(printf '%s' "$cwd" | tr -c '[:alnum:]' '-')

dir="$HOME/.claude/fmsg-mcp"
mkdir -p "$dir"
umask 077
printf '{"session_id":"%s","transcript_path":"%s","ts":%s}\n' \
    "$session_id" "$transcript_path" "$(date +%s)" \
    > "$dir/current-session-$slug.json"
