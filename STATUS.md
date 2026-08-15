# STATUS — where this project is and how it got here

*Last updated: 2026-08-15. This is the orientation document: read it first.*

## What exists and works

`fmsg-mcp` (Go, stdio MCP server, `modelcontextprotocol/go-sdk`) is **built,
unit-tested, and partially live-tested** against real fmsg hosts. Tools:
`share_session` (two-phase preview→confirm), `share_summary` (two-phase,
model-authored single-message summary), `send_message` (immediate standalone
send), `continue_thread`, `reply_to_thread`, `list_threads`,
`wait_for_message` (chat mode, decision 12), `delivery_status`, `whoami`,
`resolve_address`; MCP prompts `share_session`, `share_summary`,
`continue_thread` and `chat` (Claude Code slash commands). Release automation
(`.github/workflows/release.yml`) builds platform binaries with the fmsg CLI
**embedded** (`-tags embedcli`, extracted to the user cache dir at runtime)
plus Claude Desktop `.mcpb` bundles (manifest_version 0.3, verified against
the MCPB spec; dry-run assembled locally, not yet installed on a real
Desktop).

Upstream fmsg-cli changes this project produced: the global `--json` flag
(also surfaces previously-dropped `to_delivery`/`read`/`time_read`/`batch_id`
and the attach response) and `fmsg whoami`, **both merged to main**; and
`fmsg watch` (WebSocket event stream, fmsg-cli PR #14 + fmsg-docker test 010
PR #17, **open 2026-08-15**) — the MCP falls back to polling `list` until a
CLI with `watch` is embedded.

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
   Compression is the host's job. ~~Tool activity renders as `> 🔧 tool:`
   lines with ````tool-output` fences so any consumer can mechanically skip
   it; tool results keep head+tail (750-char budget, middle cut).~~
   *(Tool rendering superseded by decision 9: tool activity is now excluded
   entirely.)*
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
9. **Tool calls and tool output excluded entirely (2026-08-07,
   user-requested; supersedes the tool rendering in decision 3):** shares
   carry only user/assistant text. Stripping happens at the parse layer
   (`parseContent` drops `tool_use`/`tool_result` alongside thinking), so
   tool-only turns vanish before exchange splitting and no rendering or
   redaction path ever sees tool data. Consequence: pre-change sharestate
   hashes were computed over tool-inclusive renderings and can never
   prefix-match again — sharestate gained `format_version`
   (`CurrentFormat = 2`); a stale-format state previews as mode
   `new_thread_render_format_changed` and the re-share starts a fresh
   thread. One-time cost, accepted.
10. **`share_summary` (2026-08-07, user-requested):** share a session as ONE
    fmsg message instead of the whole transcript. The MCP server has no LLM —
    the model writes the summary and passes it as the `summary` argument; the
    server adds the provenance header (`RenderSummary`), redacts, and sends
    through the same two-phase preview→confirm. Re-summarising the same
    session threads the updated summary as a reply (user decision), tracked
    in `<session-id>.summary.json` (sharestate `Kind: "summary"`, no
    exchange hashes; continuation requires the same recipient set). The
    preview warns above ~9.5 KiB — the fmsg federation default max message
    size is 10 KiB (delivery code 4), while the webapi allows 10 MB.
11. **`send_message` (2026-08-07, user decision):** immediate standalone
    send — verbatim text or a model-composed answer — with **no
    preview/confirm**, a deliberate exception to decision 5: the user's own
    prompt ("send @x …") is the intent, and a preview would add a round-trip
    to every quick send. Redaction is NOT excepted — it runs on body and
    topic, with hits reported in the result since there is no preview.
    Replies stay `reply_to_thread`'s job; `send_message` always starts a
    new thread (topic defaults to the body's first line).

12. **Chat mode (2026-08-15, user-requested; design in CHAT_MODE.md):**
    "converse / chat / keep talking / auto-reply / respond to the next
    message" → the model decides *once* vs *keep* from the wording and drives
    a loop: `wait_for_message` (blocks until a qualifying inbound message,
    returns body + lineage context) → `reply_to_thread` → (keep) wait again.
    The server has no LLM and stdio can't push, so the loop lives in the
    model, steered by the tool description and the `chat` prompt. Arrival is
    **`fmsg watch` over the webapi WebSocket** (user decision — extend the
    CLI rather than talk to the webapi; decision 1 holds), with a `list`
    catch-up on connect/reconnect and a 2 s poll fallback for CLIs without
    `watch`. Per-call block capped at 230 s (Claude Desktop kills tool calls
    at ~4 min; Claude Code's default is ~28 h) — the model re-calls on
    `timeout`. **Replies are sent with no preview** — a second explicit
    exception to decision 5 with decision 11's rationale (the arming
    instruction is the intent) — but always redacted; guardrails: continuous
    mode only within one thread (`thread_of`), own/`no_reply` messages never
    qualify, caps `max_replies` 20 / `max_wait_minutes` 30 enforced by the
    model via the prompt, injection framing added to `thread.Assemble`.
    Found and fixed alongside: `reply_to_thread` had been sending bodies
    **unredacted** (invariant breach, now redacts + reports `redactions`).
    Verified live cross-host (fmsg.io↔fmsg.live) over stdio JSON-RPC:
    timeout path, WebSocket arrival in ~8 s, redacted reply, keep-mode wait
    catching the chained reply. Deferred (Track B in CHAT_MODE.md): Claude
    Code *channels* push (`notifications/claude/channel`, research preview
    behind `--dangerously-load-development-channels`) to remove idle
    round-trips.

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

## Field report (Windows 11, v0.4.1, 2026-08-13) — all four items fixed

An external user report from a real Windows install. The one real bug:
Claude Code's SessionStart payload JSON-escapes `cwd` (`C:\\Users\\…`), so
the hook slugged the escaped form (two dashes per separator) while
`locator.Slug` slugs the real path — `readPointer` always missed and
`Locate` silently fell back to mtime, picking the wrong transcript whenever
several sessions shared a project dir. Fix: the hook unescapes `\\` in `cwd`
only, before slugging; `transcript_path` must stay escaped because it is
re-emitted into the pointer JSON (unescaped `\U` is invalid JSON and
`readPointer` rejects it — the reporter hit that failure mode too). Also
from the report: README gained a Windows subsection for Claude Code (Git
Bash + explicit shell in the hook entry, `.exe` install, no chmod);
`--version`/`--help` flags added (previously any argv silently started the
stdio server); release workflow now publishes `SHA256SUMS` (generated after
mcpb signing, which rewrites bundles).

**Second field report (macOS arm64, v0.4.2 .mcpb on Claude Desktop,
2026-08-13):** clean install, full loopback share/resume verified. Friction
addressed: the `whoami` `expires_at` (~12h) read as an expiring credential —
it is the session token exchanged from the API key, rotated automatically by
fmsg-cli (by design, user-confirmed); `whoami` now says so in an
`expires_note` and the README documents it. Release asset names
(`fmsg-<platform>.mcpb` vs `fmsg-mcp_<os>_<arch>`) were ambiguous — the
release workflow now appends a "Which asset do I want?" mapping to every
release body (v0.4.2's edited by hand). Also reported, not actionable here:
GitHub's release Assets panel intermittently failed to render
(`/releases/expanded_assets/<tag>` worked).

**Dogfood update to v0.4.3 (own Linux install, 2026-08-13)** surfaced three
README gaps, all fixed: no Updating section (and the hook is easy to forget
— fixes sometimes land there, not in the binary); the hook install said
`cp hooks/…`, presuming a repo clone binary installers don't have (now a
raw-URL curl); `claude mcp add` without `--scope user` registers
per-project, so fmsg silently doesn't exist elsewhere (README now recommends
user scope). Incidental observation, not user-facing: the stdio server
exits without responding if a script writes all input and closes stdin
immediately — hold the pipe open when driving it programmatically.

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
3. ~~**CI**~~ **DONE (2026-08-05):** `.github/workflows/ci.yml` runs
   gofmt/vet/test plus the release-shaped `-tags embedcli` build on every
   PR/push. The e2e lives in **fmsg-docker** as integration test
   `008-claude-mcp-share-resume-reply.sh` (user decision: that harness
   already owns stack+DB+seeding): drives the real fmsg-mcp binary over
   stdio JSON-RPC — share (preview+confirm) → cross-instance resume →
   reply → incremental re-share asserting only the delta is sent; the
   runner builds fmsg-mcp from `FMSG_MCP_REF` (default main) like
   fmsg-cli. **Suite green 8/8 locally** (fmsg-docker 29bc006). Test 008
   already caught two real bugs: the incremental hash basis included the
   volatile provenance header (23cbea2), and the share_session schema
   rejected the documented confirm-only call (recipients lacked
   omitempty). Remaining nicety: a scheduled GitHub Actions run of the
   fmsg-docker suite (it's currently run manually).
4. **Upstream by leverage**: ~~`fmsg watch` over the WebSocket (#8)~~ built
   2026-08-15 (fmsg-cli PR #14) — merge, then re-embed in the release build
   and add an fmsg-docker chat e2e (011: two MCP instances / MCP + CLI over
   `wait_for_message` → `reply_to_thread`);
   thread/children endpoints (#2, #11); distinct exit codes (#5);
   `get-attach` URL-escape bug (#6); address lookup via fmsgid (#14);
   hash exposure (#3).
5. **v2 features**: ~~live-thread pull/watch tool~~ (chat mode, decision 12);
   Track B channel push (CHAT_MODE.md §3.5); hosted remote-MCP variant
   so claude.ai (Web) works — currently a README roadmap promise; the
   Go-package refactor replacing the subprocess + embed (#10); async
   delivery outcomes surfaced at the next prompt via a UserPromptSubmit
   hook (share writes pending ids, hook injects delivered/failed line) —
   for now share_session waits up to 60s on the chain's final message so
   most results carry terminal per-recipient state (MCP stdio cannot push
   into the conversation unprompted).
6. **Anthropic desktop-extension directory submission (2026-08-07, in
   prep):** goal is a warning-free one-click install from within Claude
   Desktop (file-based .mcpb installs always show "unverified" warnings;
   `mcpb sign` is not documented to remove them — directory listing is).
   Done: tool annotations (title + readOnly/destructive hints, required by
   review), manifest metadata (homepage/support/keywords/long_description/
   `privacy_policies`), PRIVACY.md, release workflow signs+verifies bundles
   (`MCPB_SIGNING_*` secrets; **no self-signed fallback** — `mcpb verify`
   checks the chain against the OS trust store, so self-signed always
   verifies as "not signed"; without cert secrets the step skips and
   bundles publish unsigned, which is fine: signing is not a submission
   requirement) and publishes only .mcpb bundles + fmsg-mcp binaries
   (standalone fmsg-cli artifacts dropped); LICENSE added (MIT, user
   decision 2026-08-07) and stamped in the manifest; user has a reviewer
   test account ready. Remaining: cut a
   release so signed bundles + PRIVACY.md URL are live, then submit at
   https://clau.de/desktop-extention-submission. Separately, OS-level:
   Apple Developer ID notarization of the embedded binary (Gatekeeper) and
   a Windows code-signing cert (SmartScreen). Escalation contact:
   mcp-review@anthropic.com.
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
