# TOOLS.md — fmsg-mcp tool surface

> **Design revision (v0.2):** this document describes the original design. The
> implementation was deliberately simplified after live testing: **no
> `claude-session.json` attachment** (the Markdown body is the sole carrier —
> readable by any fmsg client and loadable by any agent, not just Claude),
> **no dual addressing and no automatic `add-to`** (shares go to exactly the
> recipients the user lists; multi-recipient uses `update --to`), and **no
> `_claude` naming convention in the server** (the from address is whatever
> the configured API key grants). Resume is a plain pid-walk that concatenates
> the bodies on the lineage. Sections below about the envelope schema, dual
> addressing, and deepest-base assembly are superseded; the two-phase confirm,
> redaction, transcript locator, and CLI-only constraint are unchanged.
> Current truth lives in [STATUS.md](./STATUS.md).

Conventions common to every tool:

- All CLI invocations run through the cli-runner: **always `--json`** (flags placed
  before any negative message index), pinned env (`FMSG_API_URL`, `FMSG_API_KEY`),
  controlled CWD (never the project dir — avoids the CLI's `.env` auto-load), 60 s
  timeout per invocation, one JSON value decoded from stdout, stderr captured.
- Bodies and attachments are always passed as scratch-dir **file paths** (the CLI's
  file-vs-text heuristic makes inline text unsafe).
- Message IDs are int64 webapi row IDs; `-1` is passed through to the CLI's native
  "latest inbox message" index.
- Errors are structured: `{ "code": "<see table>", "message": "<human>",
  "detail": "<CLI stderr, e.g. 'API error 404: message not found'>" }`

| Error code | Meaning |
|---|---|
| `cli_error` | fmsg-cli missing/wrong version, non-zero exit, unparseable output |
| `not_logged_in` | No `FMSG_API_KEY` and no usable `auth.json`; or expired key |
| `not_a_shared_thread` | Ancestor chain has no `claude-session.json` anywhere (resume still proceeds — see `continue_thread` — this code is only used by `list_shared_threads` filtering and `also_share` validation) |
| `too_large` | Serialized share exceeds the 20 MB message budget after gzip |
| `needs_confirmation` | Recipient was resolved heuristically, or preview not yet confirmed (share is two-phase) |

## 1. `share_session`

Share the current Claude session as an fmsg message (Markdown body + transcript
attachment). **Two-phase:** the first call returns a preview and a `confirm_token`;
the actual send happens only when re-invoked with that token. This is the
one-action share from the user's point of view — the confirmation is a single
approval of the previewed send, required because fmsg messages are immutable
(no unsend).

```json
{
  "name": "share_session",
  "inputSchema": {
    "type": "object",
    "properties": {
      "recipient":         { "type": "string", "description": "Teammate: full fmsg address (@bob@example.com) or a short name to resolve" },
      "title":             { "type": "string", "description": "Thread topic; on a root share becomes the immutable fmsg topic. Defaults to a model-suggested title." },
      "note":              { "type": "string", "description": "Optional Markdown intro rendered above the transcript in the message body" },
      "reply_to_fmsg_id":  { "type": "integer", "description": "Share as a reply into an existing thread (pid-linked) instead of starting a new one" },
      "transcript":        { "type": "object", "description": "Model-serialized transcript (claude.ai/Desktop fallback only; omit on Claude Code where the server reads the session JSONL verbatim)" },
      "confirmed_address": { "type": "string", "description": "Echo of the resolved address from a needs_confirmation response" },
      "confirm_token":     { "type": "string", "description": "Token from the phase-1 preview; presence triggers the actual send" }
    },
    "required": ["recipient"]
  }
}
```

**Phase 1 (no `confirm_token`):** locate transcript (ARCHITECTURE.md §4) →
serialize → redact → resolve recipient (ARCHITECTURE.md §5). Returns:

```json
{
  "status": "needs_confirmation",
  "resolved_recipient": "@bob@example.com",
  "resolution": "directory | literal | convention",
  "turns": 42, "body_bytes": 18734, "attachment_bytes_gz": 210441,
  "redactions": [{"type": "fmsgk_key", "count": 1}],
  "fidelity": "verbatim",
  "warning": "fmsg messages are immutable and cannot be unsent.",
  "confirm_token": "st_9f2c…"
}
```

**Phase 2 (`confirm_token` present):** exact CLI sequence —

| Step | Invocation | Decoded |
|---|---|---|
| 1 | `fmsg --json draft create <resolved_recipient> <scratch>/body.md` + (`--topic "<title>"` if root, else `--pid <reply_to_fmsg_id \| previous share id>`) | `{"id": n}` |
| 2 | `fmsg --json update <n> --type "text/markdown"` | `{"id": n}` |
| 3 | `fmsg --json attach <n> <scratch>/claude-session.json[.gz]` | `{"filename", "size"}` — use returned filename (server collision-renames) |
| 4 | `fmsg --json draft send <n>` | `{"id": n, "time": t}` |
| 5 | `fmsg --json add-to <n> <recipient_claude> <sharer_human>` | `{"id": n, "added": k}`; per-address 400 "already added" tolerated |
| on failure after 1 | `fmsg --json del <n>` | cleanup |

Returns `{ "status": "sent", "fmsg_id": n, "recipients": [...], "open_in_claude":
"https://claude.ai/new?q=..." }`. The model relays the fmsg id and link to Alice.

## 2. `continue_thread`

Resume: walk the ancestor chain of an fmsg message and return the reconstructed
session as the tool result (which seeds the model's context on every surface).

```json
{
  "name": "continue_thread",
  "inputSchema": {
    "type": "object",
    "properties": {
      "fmsg_id": { "type": "integer", "description": "Target fmsg message id; -1 = most recent inbox message" }
    },
    "required": ["fmsg_id"]
  }
}
```

CLI mapping (ARCHITECTURE.md §6): `fmsg --json get <id>` per hop following the
`pid` field (cap 100) → deepest ancestor with a `claude-session.json[.gz]`
attachment → `fmsg --json get-attach <base> claude-session.json
<scratch>/base.json` (gunzip if needed) → `fmsg get-data <id>` (raw stdout) per
post-base plain reply. Returns one text block: provenance header,
data-not-instructions preamble, role-tagged seed transcript, appended replies
attributed `[@sender@example.com via fmsg]`, reply-target footer.

If **no** ancestor carries a transcript, the thread is still assembled from plain
message bodies (`get-data` walk) — resuming an ordinary fmsg thread in Claude is
useful too; the provenance header just says "no Claude session snapshot; plain
fmsg thread".

## 3. `reply_to_thread`

Send a plain reply into the thread (visible to all participants in any fmsg
client), or a full pid-linked re-share.

```json
{
  "name": "reply_to_thread",
  "inputSchema": {
    "type": "object",
    "properties": {
      "fmsg_id":    { "type": "integer", "description": "Message being replied to (pid target)" },
      "body":       { "type": "string",  "description": "Markdown reply body" },
      "also_share": { "type": "boolean", "description": "true = attach a full session snapshot (share_session semantics, incl. two-phase confirm) instead of a plain reply" }
    },
    "required": ["fmsg_id", "body"]
  }
}
```

Plain-reply CLI mapping: `fmsg --json draft create <parent-from-addr>
<scratch>/reply.md --pid <fmsg_id>` → `fmsg --json update <n> --type
"text/markdown"` → `fmsg --json draft send <n>` → `fmsg --json add-to <n> <other
participants of parent…>` (participants read from the parent's `--json` fields:
`from` ∪ `to` ∪ each `add_to[].add_to_from` ∪ `add_to[].to`, minus the replier's
own addresses; 400s tolerated). `also_share: true` delegates to `share_session`
with `reply_to_fmsg_id` set.

## 4. `list_shared_threads` *(optional tier)*

```json
{
  "name": "list_shared_threads",
  "inputSchema": {
    "type": "object",
    "properties": { "limit": { "type": "integer", "maximum": 20, "default": 10 } }
  }
}
```

CLI mapping: **one call** — `fmsg --json list --limit <limit>` → keep items whose
`attachments[].filename` is `claude-session.json`/`.json.gz`. Returns
`[{fmsg_id, from, title (topic), time, is_root (pid == null)}]`, cached 60 s. A
server-side filter is still the better answer (flagged upstream), and
`continue_thread -1` already covers the common "open the one Bob just received"
case without this tool.

## 5. `whoami` and `resolve_address`

- `whoami()` — no CLI call. Reads `FMSG_API_KEY` presence + `~/.config/fmsg/
  auth.json` (read-only): returns `{address, human_address (suffix-stripped),
  api_url, auth_type, key_expires_at?}`, or `not_logged_in` with provisioning
  guidance.
- `resolve_address(name)` — no CLI call; dry-run of the resolution chain
  (literal → `FMSG_DIRECTORY` → `@<name>@$FMSG_DEFAULT_DOMAIN`): returns
  `{address, resolution}` without sending anything.

## 6. MCP prompts (Claude Code slash commands)

| Prompt | Surfaces as | Body |
|---|---|---|
| `share_session` | `/fmsg:share_session [recipient]` | "Share this session with {recipient} via fmsg: call share_session, present the preview (recipients, size, redactions, immutability warning), and on my approval re-invoke with the confirm_token." |
| `continue_thread` | `/fmsg:continue_thread [id]` | "Call continue_thread with fmsg_id {id \| -1} and continue the conversation from the returned context." |

Tools remain the universal path on surfaces without slash commands.

## 7. `claude-session.json` v1 (normative)

```json
{
  "format": "fmsg-claude-session",
  "format_version": 1,
  "provenance": {
    "surface": "claude-code | claude-desktop | claude-ai",
    "fidelity": "verbatim | model-serialized",
    "model": "claude-…",
    "session_id": "uuid | null",
    "shared_at": 1754280000.0,
    "sharer_address": "@alice_claude@example.com",
    "sharer_human_address": "@alice@example.com",
    "cwd": "/home/alice/project | null",
    "git": { "remote": "…", "branch": "…", "commit": "…" },
    "base_fmsg_id": 123,
    "incorporated_fmsg_ids": [101, 105]
  },
  "title": "Fixing the auth refresh bug",
  "turns": [
    { "i": 0, "role": "user",
      "blocks": [ { "type": "text", "text": "…" } ] },
    { "i": 1, "role": "assistant",
      "blocks": [
        { "type": "text", "text": "…" },
        { "type": "tool_use", "id": "toolu_x", "name": "Bash", "input": {} },
        { "type": "tool_result", "tool_use_id": "toolu_x", "text": "…",
          "truncated": false }
      ] }
  ],
  "attachments_manifest": [
    { "fmsg_filename": "ctx-1-auth.go.txt", "kind": "context-file",
      "original_path": "internal/auth.go", "sha256": "…", "bytes": 4123 }
  ],
  "truncation": { "truncated": false, "strategy": null,
                  "original_turn_count": 42 },
  "redaction": { "applied": true, "patterns_hit": ["fmsgk_key"] }
}
```

Rules:

- `format_version` bumps on any breaking change; readers reject unknown majors
  with a clear "update fmsg-mcp" error.
- `base_fmsg_id` = the fmsg message this session was resumed from (null on a fresh
  session); `incorporated_fmsg_ids` = every fmsg message whose content is already
  inside `turns` (the resume splice-check depends on this).
- Thinking blocks are never serialized. `tool_result` text is taken as stored in
  the Claude Code JSONL (already bounded); `model-serialized` fidelity carries
  whatever the model provided.
- `attachments_manifest` may be present-but-empty in v1; populated manifests are
  v1.1 (context-file bundling).
- Serialized JSON > 1 MiB is gzipped (`claude-session.json.gz`); readers sniff
  `1f 8b` magic bytes rather than trusting the extension.
- fmsg attachment filenames generated by this server always match
  `^[A-Za-z0-9._-]+$` and are unique per message case-insensitively (spec rule +
  CLI escaping bug).
