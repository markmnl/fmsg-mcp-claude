# STATUS — where this project is and how it got here

*Last updated: 2026-08-04. This is the orientation document: read it first.*

## What exists and works

`fmsg-mcp` (Go, stdio MCP server, `modelcontextprotocol/go-sdk`) is **built,
unit-tested, and partially live-tested** against real fmsg hosts. Tools:
`share_session` (two-phase preview→confirm), `continue_thread`,
`reply_to_thread`, `list_threads`, `delivery_status`, `whoami`,
`resolve_address`; MCP prompts `share_session` and `continue_thread`
(Claude Code slash commands). Release automation
(`.github/workflows/release.yml`) builds platform binaries with the fmsg CLI
**embedded** (`-tags embedcli`, extracted to the user cache dir at runtime)
plus Claude Desktop `.mcpb` bundles (manifest_version 0.3, verified against
the MCPB spec; dry-run assembled locally, not yet installed on a real
Desktop).

Upstream fmsg-cli changes this project produced, **both merged to main**:
the global `--json` flag (also surfaces previously-dropped
`to_delivery`/`read`/`time_read`/`batch_id` and the attach response) and
`fmsg whoami`.

## Decisions made (chronological, with rationale)

1. **Everything goes through fmsg-cli, never fmsg-webapi directly** —
   original planning constraint. Honestly assessed afterwards: the right
   *first iteration* (it forced `--json`/`whoami` upstream and kept auth in
   one place) but not the right resting place. **Agreed endgame:** promote
   fmsg-cli's client to a public Go package and import it, deleting the
   subprocess runner and the embed machinery (upstream issue #10).
2. **v0.1 design (envelope attachment, dual addressing, `_claude` suffix
   conventions) was deliberately abandoned** after first live tests. v0.2:
   the **Markdown body is the sole carrier** (any fmsg client renders it,
   any agent can consume it — not Claude-specific); shares go to **exactly
   the recipients the user lists** (no auto add-to, no dual addressing —
   the sender picks which of the recipient's identities should participate);
   the from address is simply **whatever the configured API key grants**.
3. **One fmsg message per user prompt, pid-chained** — the fmsg thread
   mirrors the conversation, so receivers can branch from or resume up to
   any prompt, and later shares continue the chain via `reply_to_fmsg_id`.
   Compression is the host's job. Tool activity renders as `> 🔧 tool:` lines
   with ````tool-output` fences so any consumer can mechanically skip it;
   tool results keep head+tail (1500-char budget, middle cut).
4. **Default topic = Claude Code's own session summary** (the `summary`
   lines in the transcript JSONL), falling back to the first prompt's
   opening words.
5. **Two-phase confirm and secret redaction are non-negotiable**, but the
   user-facing framing is a simple "are you sure you want to send?" — the
   immutability fact is background, not a scare line. Precise phrasing when
   it comes up: messages are immutable, cannot be *edited or recalled*; do
   NOT say "cannot be deleted" (hosts may delete from their own stores —
   replies to deleted messages are then rejected, so the thread restarts or
   someone who still has them resends; resend tooling not built yet) and do
   NOT say "cannot be unsent" (models garble it into "unsendable").
6. **Snapshot semantics; live-thread convergence is v2** (user-confirmed).
   Replies already pid-thread naturally, so deferring costs no migration.
7. Harness meta content (`isMeta` lines, `<local-command-caveat>`,
   `<command-name>`, `<system-reminder>`) is **excluded from transcripts**;
   thinking blocks always excluded.

## Live-testing findings (fmsg.live / fmsg.io, 2026-08-04)

- **fmsg-webapi PUT is full-replacement while fmsg-cli's `update` sends only
  provided fields** (its help text promises PATCH). A partial update wiped
  recipients, pid, and topic → shares arrived as disconnected, topicless,
  recipientless messages. Server-side workaround: `UpdateFull` restates
  every field. **Real upstream bugs, unfixed:** OPEN_QUESTIONS #15 (contract
  mismatch), #16 (PUT accepts empty `to`), #17 (send accepts zero
  recipients).
- Cross-host share fmsg.live→fmsg.io **delivered** (small messages);
  per-recipient outcomes are visible via `delivery_status` /
  `fmsg --json get`. With per-prompt granularity and no embedded IDs,
  cross-host **resume may now work** (unverified — see next steps).
- `whoami` with env-key auth needed the CLI's new `whoami` (the address
  lives in the exchanged JWT).
- **Cross-host pid chains delivered out of order** (2026-08-05): fmsgd sends
  concurrently, receiving hosts reject children whose parent isn't stored
  yet (code 6, no retry) → remote recipients got only the chain's root.
  Upstream issue #18; the server now waits for each message's terminal
  delivery state before sending its child. Agreed host-side design: fmsgd's
  dispatch query holds a child for domain X until its parent is delivered
  there; webapi synchronously rejects replies to domains the parent never
  reached — the webapi half is implemented (fmsg-webapi branch
  `verify-reply-deliverable`, 409 on send with the add-to/new-thread
  remedy); the fmsgd sequencing half remains. The MCP pacing workaround
  dies when the fmsgd fix lands.

## Remaining work, in priority order

1. **Cut `v0.2.0`** (push, tag, release) and validate the artifacts for
   real: install a `.mcpb` on actual Claude Desktop; `claude mcp add` a
   downloaded binary.
2. **Live-validate resume** — the core promise is untested: second persona
   runs `continue_thread`, replies from the UI, first persona resumes again.
   Also test cross-host resume from the fmsg.io side.
3. **Fix webapi bugs #15–17** (PUT merge-or-validate; reject empty `to` on
   PUT; reject send with zero recipients) — they corrupt data for every
   client, not just this server.
4. **CI**: build+test workflow on PR; e2e suite against the workspace
   `fmsg-docker` stack (share→resume→reply, two seeded accounts) — the
   original P0 exit criterion, never run.
5. **Upstream by leverage**: raise fmsgd's 10 KiB federation default (#12);
   `fmsg watch` over the WebSocket (#8 — prerequisite for v2 live threads);
   thread/children endpoints (#2, #11); distinct exit codes (#5);
   `get-attach` URL-escape bug (#6); address lookup via fmsgid (#14);
   hash exposure (#3).
6. **v2 features**: live-thread pull/watch tool; hosted remote-MCP variant
   so claude.ai (Web) works — currently a README roadmap promise; the
   Go-package refactor replacing the subprocess + embed (#10).
7. **Housekeeping**: ARCHITECTURE.md and TOOLS.md still describe v0.1 under
   supersession banners — rewrite to the shipped design; prune
   OPEN_QUESTIONS; keep watching assumption A3 (Claude Code JSONL schema
   drift — the parser is deliberately tolerant).

## Gotchas for anyone touching the code

- Any `update` sent to the webapi must restate **all** fields
  (`cli.UpdateFull`) until #15 is fixed upstream.
- Always `--json` first on CLI argv; flags must precede negative message
  indices; bodies always via temp files (the CLI's file-vs-text heuristic).
- Dev builds use `fmsg` from PATH / `FMSG_CLI`; release builds embed it
  (`cp <fmsg> internal/cli/embedded/fmsg-bin && go build -tags embedcli`).
- The server refuses CLI builds without `--json` at first tool use.
