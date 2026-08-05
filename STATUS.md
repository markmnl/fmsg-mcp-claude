# STATUS — where this project is and how it got here

*Last updated: 2026-08-05. This is the orientation document: read it first.*

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
   tool results keep head+tail (750-char budget, middle cut).
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
8. **Incremental re-share (2026-08-05, user-requested):** sharing a session
   again sends only the exchanges added since the last share, pid-chained
   onto the already-shared thread. `internal/sharestate` persists per-session
   progress (`~/.claude/fmsg-mcp/shares/<session-id>.json`: thread root, last
   fmsg id, per-exchange hashes, recipients); the preview verifies the
   current session still hash-extends what was sent. Continuation requires
   the **exact same recipient set** — a widened audience can't read (and
   their host doesn't hold) the earlier messages, a narrowed one forks the
   bookkeeping — and a diverged/edited session likewise falls back to a full
   new thread. State saves after *every* sent message, so a mid-chain
   failure resumes from the last delivered message on the next share.
   Explicit `reply_to_fmsg_id` bypasses the state entirely.

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
- **Cross-host replies to own sent messages bounced code 6** (2026-08-05).
  First theory (delivery ordering race) was wrong — pacing didn't fix it.
  Real root cause: fmsgd's sender hashed the pre-deflate header form while
  receivers hash the transmitted (deflated) form, so the recorded parent
  hash never matched and every chained reply missed. Fixed on fmsgd branch
  `fix-shared-hash-deflated-form` + new fmsg-docker integration test
  `007-reply-to-own-sent.sh` (the suite only ever replied to *received*
  messages, hiding this). Messages sent pre-fix have the wrong hash
  recorded forever — start new threads after deploying fmsgd. Related,
  still useful: webapi's synchronous reject of replies to domains the
  parent never reached (branch `verify-reply-deliverable`); fmsgd
  parent-before-child dispatch sequencing remains open (#18a); the MCP's
  60s chain pacing can relax once that lands. **Both fixes deployed to
  fmsg.io and fmsg.live 2026-08-05** — cross-host chains unverified since;
  remember replies to pre-fix messages still bounce, test on new threads.

## Remaining work, in priority order

*(Done 2026-08-05: repo pushed to github.com/markmnl/fmsg-mcp-claude,
releases v0.2.0–v0.3.1 cut with full artifacts; fmsgd + webapi fix branches
merged and deployed to both live stacks.)*

1. ~~**Live-validate resume**~~ **DONE (2026-08-05, user-confirmed):** the
   full loop validated live — cross-host chain share (hash fix holds),
   incremental re-share, resume via `continue_thread`, reply.
2. **Merge + deploy the #15–17 resolution (2026-08-05, user decision:
   drafts are a free-form workspace; *send* is the validation gate).**
   Implemented on branches: fmsg-webapi `validate-at-send` (400 on send for
   no recipients / bad address / no type / bad version; #17 fixed, #16
   reclassified by-design) and fmsg-cli `update-merge-semantics` (fetch-and-
   merge before the full-replacement PUT; #15 fixed). fmsg-mcp keeps
   `UpdateFull` — correct under either contract.
3. **CI**: build+test workflow on PR; e2e suite against the workspace
   `fmsg-docker` stack (share→resume→reply, two seeded accounts) — the
   original P0 exit criterion, never run.
4. **Upstream by leverage**: raise fmsgd's 10 KiB federation default (#12);
   `fmsg watch` over the WebSocket (#8 — prerequisite for v2 live threads);
   thread/children endpoints (#2, #11); distinct exit codes (#5);
   `get-attach` URL-escape bug (#6); address lookup via fmsgid (#14);
   hash exposure (#3).
5. **v2 features**: live-thread pull/watch tool; hosted remote-MCP variant
   so claude.ai (Web) works — currently a README roadmap promise; the
   Go-package refactor replacing the subprocess + embed (#10); async
   delivery outcomes surfaced at the next prompt via a UserPromptSubmit
   hook (share writes pending ids, hook injects delivered/failed line) —
   for now share_session waits up to 60s on the chain's final message so
   most results carry terminal per-recipient state (MCP stdio cannot push
   into the conversation unprompted).
6. **Housekeeping**: ARCHITECTURE.md and TOOLS.md still describe v0.1 under
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
