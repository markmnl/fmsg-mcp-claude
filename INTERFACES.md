# INTERFACES.md — Step 0 inventory of the real interfaces

Everything in this document was read from the sibling working copies at the time of
writing (fmsg-cli `v0.2.0` **plus the `json-output` branch — see below**, fmsg-webapi
`HEAD dc6977b`, fmsg-spec `SPEC.md`). The MCP server **only ever shells out to
fmsg-cli**; the webapi column exists so we know what the CLI is actually doing and
where capability gaps live (CLI vs. server), not so we can call the webapi ourselves.

> **Baseline: this plan requires fmsg-cli with the global `--json` flag** (implemented
> on the `json-output` branch in this workspace, headed for the next release). With
> `--json`, every command emits a single JSON value on stdout, faithful to the webapi
> response — including fields the human output omits (`to_delivery`, `read`,
> `time_read`, add-to `batch_id`). The MCP server pins a minimum CLI version that
> includes it and refuses to run against older builds.

## 1. fmsg-cli command inventory

Source of truth: `fmsg-cli/cmd/*.go` (one file per command), HTTP layer in
`fmsg-cli/internal/api/client.go`. Global flags: **`--json`** (machine-readable
output, see above) and Cobra's `--help`. There is still no `--api-url`, `--config`,
or `--verbose`.

| Command | Flags / args | fmsg-webapi call(s) | Notes |
|---|---|---|---|
| `login [api-key\|jwt]` | `--address` | API key: `POST /fmsg/token`. JWT: none (decoded locally) | Prompts interactively if no arg. Writes `~/.config/fmsg/auth.json` (0600). |
| `list` / `ls` | `--limit` (default 20, server caps at 100), `--offset` | `GET /fmsg` | Inbox. With `--json`: array of full list items (incl. `attachments`, `to_delivery`); see §3. |
| `sent` | `--limit`, `--offset` | `GET /fmsg/sent` | Messages you authored, **including unsent drafts**. |
| `get <id>` | — | `GET /fmsg/{id}` | With `--json`: full message object; see §3. |
| `send <recipient> <file\|text>` | `--pid`/`-p` (int64), `--topic`, `--important`, `--no-reply` | `POST /fmsg` then `POST /fmsg/{id}/send` | **Exactly one recipient. Type hardcoded `text/plain`. Cannot attach** (draft is sent immediately). |
| `draft create <recipient> <file\|text>` | same as `send` | `POST /fmsg` | With `--json`: `{"id": n}`. |
| `draft send <id>` | — | `POST /fmsg/{id}/send` | With `--json`: `{"id": n, "time": t}`. |
| `update <id> [file\|text]` | `--to` (slice, **replaces** recipients), `--topic`, `--type` (MIME), `--pid`, `--important`, `--no-reply` | `PUT /fmsg/{id}` | Only changed flags are sent. **The only way to set a non-text/plain type.** |
| `del <id>` | — | `DELETE /fmsg/{id}` | Drafts only. |
| `add-to <id> <recipient>...` | — | `POST /fmsg/{id}/add-to` | Works on **sent** messages; caller must be `from` or a `to` recipient. Re-adding an already-added address → HTTP 400. |
| `attach <id> <file>` | — | `POST /fmsg/{id}/attach` (multipart, field `file`) | Draft-only. One file per invocation; whole file buffered in memory. |
| `get-attach <id> <filename> <output-file>` | — | `GET /fmsg/{id}/attach/{filename}` | Output path **mandatory**. Filename is **not URL-escaped** by the CLI (bug — see §6). |
| `get-data <id> [output-file]` | — | `GET /fmsg/{id}/data` | **Raw body bytes to stdout when no output file** (unaffected by `--json`). |
| `rm-attach <id> <filename>` | — | `DELETE /fmsg/{id}/attach/{filename}` | Draft-only. |
| `sub-accounts list` | — | `GET /fmsg/sub-accounts` | |
| `sub-accounts get <agent>` | — | `GET /fmsg/sub-accounts/{agent}` | |
| `sub-accounts create <agent>` | `--cidr` (required), `--expires` (RFC3339, required) | `POST /fmsg/sub-accounts` | Prints `API key (save now, shown once): fmsgk_...`. Requires IdP-authenticated owner. |
| `sub-accounts update-cidrs <agent>` | `--cidr` (required) | `PATCH /fmsg/sub-accounts/{agent}` | |
| `sub-accounts rotate-key <agent>` | `--expires` (required) | `POST /fmsg/sub-accounts/{agent}/rotate-key` | |
| `sub-accounts delete <agent>` | — | `DELETE /fmsg/sub-accounts/{agent}` | |

**Message IDs** everywhere are the webapi's **int64 row IDs** (bigserial in the
Postgres store shared with fmsgd), *not* fmsg wire hashes. Negative IDs are accepted
by every `<id>` argument: `-1` = most recent **inbox** message, `-2` = second most
recent, etc. (costs an extra `GET /fmsg?limit=1&offset=N-1` round trip, is racy, and
resolves against the inbox only — wrong for drafts).

**Body argument heuristic:** for `send`/`draft create`/`update`, the second positional
is read with `os.ReadFile` first; if that succeeds it is treated as a **file path**,
otherwise as literal text (`-` = stdin). A literal message matching an existing
filename is silently replaced by the file's contents. The MCP server must therefore
**always pass a temp-file path**, never inline text.

## 2. Configuration and auth

- Env vars (the complete list, `fmsg-cli/internal/config/config.go`):
  - `FMSG_API_URL` — base URL of fmsg-webapi. Default `http://127.0.0.1:8000`.
    **No flag override exists.**
  - `FMSG_API_KEY` — optional `fmsgk_` key for non-interactive use; takes precedence
    over `auth.json`; its exchanged JWT is cached in memory only.
- `.env` is auto-loaded from the **current working directory** on every invocation
  (`godotenv.Load()` in `main.go`). The MCP server must pin its own environment and
  run the CLI from a controlled CWD so a project's stray `.env` can't redirect it.
- Credential store: `~/.config/fmsg/auth.json` (dir 0700, file 0600) — fields
  `auth_type` (`api_key`|`jwt`), `api_key`, `access_token`, `expires_at`, `user`
  (the fmsg address), `api_url`. Credentials are **bound to the API URL**: changing
  `FMSG_API_URL` invalidates them.
- Two credential types:
  1. **fmsg API key** `fmsgk_<key_id>_<secret>` → exchanged at `POST /fmsg/token`
     for a short-lived first-party JWT (server default TTL 12 h), cached and
     **auto-refreshed** ~5 min before expiry. Suitable for unattended use. API keys
     exist **only for sub-accounts** (owner provisions them via `sub-accounts
     create`), and are additionally bound to source-IP CIDRs and an expiry date.
  2. **User JWT from the configured identity provider** — used verbatim, **cannot be
     refreshed**; when it expires the user must re-run `fmsg login`. Unsuitable for
     a background server.
- On 401 the CLI force-refreshes once and replays the request where possible.

## 3. Exact output the MCP server consumes (`--json` mode)

The server invokes every command with the global `--json` flag and decodes one JSON
value from stdout. Shapes (fixed by `fmsg-cli/internal/api/client.go` structs, which
now mirror the webapi responses):

- `list` / `sent`: JSON **array** of message list items — `{id, version, has_pid,
  has_add_to, important, no_reply, deflate, pid, from, to, to_delivery[{addr,
  time_delivered, response_code}], add_to[{batch_id, add_to_from, to, to_delivery,
  time}], time, topic, type, size, read, time_read, attachments[{size, filename}]}`.
  Empty inbox → `[]`.
- `get <id>`: the same object without `id`, plus `short_text` (first ≤768 bytes of
  `text/*` bodies). **`pid` (int64 or null) is the basis of the ancestor walk;
  `attachments[].filename` is how transcript-bearing messages are detected.**
- `draft create`: `{"id": n}`. `send` / `draft send`: `{"id": n, "time": <unix
  float>}`. `update` / `del`: `{"id": n}`. `add-to`: `{"id": n, "added": k}`.
- `attach`: the server's `{"filename": "...", "size": n}` (collision-renamed
  filename included — important, the server may rename on conflict).
- `get-attach`: `{"filename": "...", "saved_to": "<path>"}`.
- `get-data` **with no output file: raw body bytes regardless of `--json`** — the
  channel for fetching plain-reply bodies.
- `sub-accounts …`: grant object(s); `api_key` present only on create/rotate.

Caveats that remain in `--json` mode:

- **Errors are still prose on stderr with a binary exit code** (0/1). Error
  classification parses the `API error <status>:` prefix; a `--json` error object +
  distinct exit codes remains an upstream ask (§6).
- **Flags must precede a negative message index** (`fmsg --json get -1`): the CLI
  inserts a `--` separator before the first `-N` argument, so anything after it is
  positional.
- Delivery state is now observable (`to_delivery`, `read`, `time_read`, `batch_id`
  are included in JSON output) — the earlier CLI limitation is resolved by the
  `json-output` change.

## 4. fmsg-webapi constraints that shape the design

(Read from `fmsg-webapi/cmd/fmsg-webapi/main.go` and `internal/handlers/*` — for
understanding only; the MCP server never calls these directly.)

- **No thread endpoint, no ancestors endpoint, no children/replies listing.** The
  only ancestor walk in the codebase is an internal recursive CTE used to compute a
  push notification's thread root; it is not exposed over HTTP. Reconstructing a
  lineage means one `GET /fmsg/{id}` per hop, client-side.
- **Same-host participants share one message row** — local-domain delivery is
  resolved by the webapi against the same `msg` row the sender created, and
  `GET /fmsg/{id}` authorizes any participant. So an integer message ID in a link is
  valid for **both Alice and Bob when they're on the same host**. Cross-host, each
  host assigns its own row IDs; the wire-level SHA-256 exists in the database
  (`msg.sha256`) but is **never exposed by any endpoint** → there is no stable
  cross-host message reference today.
- Inbox listing supports only `limit` (≤100) and `offset` — no filtering by sender,
  topic, date, unread, or thread.
- Sending is two-step (draft `POST /fmsg`, then `POST /fmsg/{id}/send`); the body is
  a JSON string field (binary content must go via attachments); the server ignores
  the client-supplied `size` and sniffs `deflate` itself.
- `POST /fmsg/{id}/add-to` requires the caller to be `from` or an existing `to`
  recipient, works after sending, and rejects already-added addresses with 400.
- A WebSocket (`GET /fmsg/ws`) pushes `new_msg` / `delivered` / `recipients_added`
  events — **fmsg-cli does not wrap it** (a `wait` command was removed). Live
  updates via the CLI mean polling `list` and diffing IDs.

## 5. Size budget — the real numbers

The often-quoted "~10 MB of data plus ~10 MB of attachments" is **not in the fmsg
spec**. Where the numbers actually live:

| Limit | Value | Where | Applies to |
|---|---|---|---|
| `FMSG_API_MAX_DATA_SIZE` | 10 MB default | fmsg-webapi | message body, same-host client API |
| `FMSG_API_MAX_ATTACH_SIZE` | 10 MB default | fmsg-webapi | each attachment |
| `FMSG_API_MAX_MSG_SIZE` | 20 MB default | fmsg-webapi | body + all attachments |
| `FMSG_MAX_MSG_SIZE` | **10 KiB default** | fmsgd (`cmd/fmsgd/host.go:166`) | host↔host federation, data + attachments combined |
| Spec `MAX_SIZE` | per-host config; spec's example value is 1 MiB | fmsg-spec | normative variable, no fixed value |

Neither `fmsg-docker` nor `fmsg-deploy-scripts` overrides the fmsgd default, so on a
default deployment a **cross-host** transcript larger than 10 KiB is rejected with
fmsg code 4 ("too big"). **Same-host delivery never traverses fmsgd's limit** — the
webapi resolves local recipients directly — so the 10/10/20 MB budget genuinely holds
for the MVP's same-host scope. A typical Claude Code transcript JSON (hundreds of KB
to a few MB, gzipping 5–10×) fits comfortably; compaction is an overflow path, not a
default (see PHASED_PLAN.md).

## 6. Gaps: what fmsg-cli/fmsg-webapi can't do that this design needs

Each of these is a **finding to flag upstream** (issue stubs in OPEN_QUESTIONS.md
§Upstream), not something the MCP server reimplements around:

1. ~~No structured output~~ — **resolved**: the global `--json` flag is implemented
   (`json-output` branch, this workspace) and this plan builds on it.
2. **No thread/ancestor fetch and no children listing** — the ancestor walk is N
   sequential `get --json` calls following `pid`; listing replies to a message is
   *impossible* through the CLI or the webapi.
3. **No message-hash exposure and no get-by-hash** — blocks any stable cross-host
   reference; MVP links are host-scoped integer IDs.
4. **`send` is single-recipient, untyped, attachment-less** — a markdown share with
   an attachment takes a 4-command dance (`draft create` → `update --type` →
   `attach` → `draft send`) plus `add-to` for extra recipients.
5. **Binary exit codes and prose-only errors** — even with `--json`, failures are
   plain text on stderr; classification requires parsing the `API error <status>:`
   prefix. Upstream ask: distinct exit codes and/or a JSON error object.
6. **`get-attach`/`rm-attach` don't URL-escape the filename** (`client.go:463,493`)
   — filenames with spaces or reserved characters break. We sidestep it by only ever
   generating `[A-Za-z0-9._-]` attachment names, but it's an upstream bug.
7. **No `whoami`** — the server reads `auth.json` (read-only) to learn the logged-in
   address.
8. **No WebSocket wrapper / watch command** — live-thread updates (v2) need polling
   or an upstream `watch`.
9. **No act-as support in the CLI** (webapi's `X-FMSG-Act-As` is IdP-owner-only
   anyway) — drives the dual-addressing scheme in ARCHITECTURE.md §identity.
10. **fmsgd federation size default (10 KiB)** — cross-host transcripts are
    undeliverable on default hosts.
11. **No address lookup/directory surface** — fmsgid holds address existence/status
    but neither the CLI nor the webapi exposes a lookup a client could use to
    resolve "bob" → `@bob@example.com`.

## 7. Claude-surface interfaces

- **Claude Code session transcripts** are JSONL files under
  `~/.claude/projects/<cwd-slug>/<session-uuid>.jsonl`, containing user/assistant
  turns including `tool_use`/`tool_result` blocks. A local stdio MCP server can read
  them; identifying *which* session invoked the tool is the design problem
  (ARCHITECTURE.md §transcript acquisition).
- **Claude Code hooks** (e.g. `SessionStart`) receive `session_id` and
  `transcript_path` and can persist a pointer for the MCP server.
- **MCP prompts** exposed by a server appear in Claude Code as slash commands
  (`/mcp__<server>__<prompt>`).
- **claude.ai / Claude Desktop / Cowork**: MCP tools receive only their arguments —
  there is no access to the client's conversation store, so verbatim transcript
  capture is not possible from an MCP server on these surfaces (share falls back to
  a model-serialized transcript; resume is unaffected because the tool *returns*
  context rather than reading it).
- **`https://claude.ai/new?q=<urlencoded prompt>`** prefills a new chat — the basis
  of the "Open in Claude" link (assumption A2 below).

## 8. Assumptions to verify (labelled; referenced from OPEN_QUESTIONS.md)

- **A1 — CWD inheritance:** a stdio MCP server launched by Claude Code inherits the
  project directory as CWD (or receives it via an env var), letting the server derive
  the `~/.claude/projects/<cwd-slug>` path for its fallback transcript locator.
  *Verify:* log `os.Getwd()` from a scratch MCP server under Claude Code.
- **A2 — claude.ai prefill:** `https://claude.ai/new?q=...` reliably lands a
  logged-in user in a new chat with the prompt prefilled (not auto-sent).
  *Verify:* manual test on claude.ai and Claude Desktop link handling.
- **A3 — JSONL stability:** the Claude Code transcript JSONL block schema is stable
  enough to parse against a pinned Claude Code version, and tool_result contents in
  it are already bounded in size. *Verify:* diff transcripts across two Claude Code
  releases; pin a minimum supported version.
