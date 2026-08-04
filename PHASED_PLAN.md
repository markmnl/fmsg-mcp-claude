# PHASED_PLAN.md

## MVP definition

The smallest build where **Share and Resume are both trivial for one pair of users
on the same fmsg host**: Alice shares from Claude Code in 2 actions; Bob resumes on
any Claude surface in ≤2 actions (3 via the claude.ai link, flagged); either can
reply from a plain fmsg client and the thread stays coherent.

In: Go stdio server; verbatim share on Claude Code + model-serialized fallback
elsewhere; snapshot semantics; ancestor-walk resume with deepest-base assembly;
two-phase confirm + redaction; sub-account identity + dual addressing; installer
script; MCP prompts.
Out (deliberately — see v2): live-thread pull, cross-host, claude.ai verbatim
share, deep-link resolver, context-file bundling, compaction.

## Phases

### P0 — spike: prove the fragile seams *(exit: green e2e script)*
- Stand up the local stack (workspace `start-local-stack.sh` +
  `seed-local-accounts.sh`; two seeded accounts on one host), with fmsg-cli built
  from the `json-output` branch (or the release containing it).
- Script (throwaway) the full share sequence via raw fmsg-cli `--json`:
  `draft create → update --type → attach → draft send → add-to`, then the resume
  walk (`get --json` pid loop, `get-attach`, `get-data`).
- Capture **golden JSON fixtures** for every command shape the server decodes
  (incl. a multi-batch add-to message and delivery-state fields) against the
  pinned CLI version; verify against the live stack, not just unit fixtures.
- Verify assumptions **A1/A2/A3** (INTERFACES.md §8) — each has a one-sitting test.
- Measure: real transcript JSONL → envelope size, gzip ratio, walk latency at
  depth 20.

### P1 — happy-path Share + Resume *(exit: two-user demo on one host)*
- Go server skeleton (`modelcontextprotocol/go-sdk`, stdio); cli-runner with pinned
  env + timeouts; output-parser module with the P0 goldens as its test suite.
- `share_session` (verbatim path: hook pointer + fallback locator; serializer;
  no confirm/redaction yet behind a dev flag), `continue_thread` (full walk +
  deepest-base + tail assembly), `whoami`.
- SessionStart hook + pointer file.
- Demo: Alice shares from Claude Code → Bob `continue_thread -1` in Claude Code
  and in claude.ai → Bob replies from the stack's web client → Alice
  `continue_thread` sees Bob's reply in context.

### P2 — safety + identity *(exit: UX_FLOWS step counts hold with confirm on)*
- Two-phase confirm (preview payload, `confirm_token`), redaction pass +
  preview reporting, draft cleanup on failure, structured error codes.
- Resolution chain (`FMSG_DIRECTORY`, convention + `needs_confirmation`),
  `resolve_address`, dual addressing add-to with 400 tolerance.
- `reply_to_thread` incl. participant fan-out parsing (golden-tested).

### P3 — polish + distribution *(exit: non-technical install in 2 steps)*
- MCP prompts (`/mcp__fmsg__share_session`, `/mcp__fmsg__continue_thread`),
  `list_shared_threads` (cached), model-serialized share fallback path.
- Installer script (binaries for linux/darwin × amd64/arm64, env file, hook,
  `claude mcp add`), org-config template (`FMSG_API_URL`, `FMSG_DEFAULT_DOMAIN`,
  `FMSG_DIRECTORY`), admin provisioning runbook.
- README with the UX_FLOWS walks; e2e suite against the docker stack in CI.

### P4 — upstream engagement
- Land the fmsg-cli `--json` branch (#1 — already implemented) via its normal PR
  workflow and cut the release the MCP server pins.
- File the remaining issue stubs from OPEN_QUESTIONS.md §6 against fmsg-cli /
  fmsg-webapi / fmsgd, each linking back to the section of this repo it would
  simplify. Priority order: `thread` (#2), federation size default (#12), hash
  exposure (#3).

## v2 — and why each item was cut, not forgotten

| v2 item | Why cut from MVP |
|---|---|
| **Live-thread pull/watch** | Needs polling or an upstream CLI `watch` (WebSocket exists server-side, unwrapped). The data model already converges via pid-linking, so deferring adds no migration cost — it's purely additive notification UX. (User-confirmed fast-follow.) |
| **Cross-host sharing** | Blocked twice upstream: fmsgd's 10 KiB federation default and no exposed message hash for stable cross-host references. Building around either would violate the routes-through-fmsg-cli constraint. |
| **Verbatim share on claude.ai/Desktop** | MCP servers have no access to those surfaces' conversation stores; requires platform-side export. Model-serialized fallback ships in MVP with fidelity disclosure. |
| **Deep-link resolver (`fmsg-claude://` companion app)** | A per-OS installed handler + security surface to save one click over the `claude.ai/new?q=` link. Revisit if the 3-step claude.ai resume flow proves to be real friction. |
| **Context-file bundling (manifest v1.1)** | Schema slot reserved; needs sanitization/limits design and inflates the confirm surface. Transcript-only sharing already delivers the core value. |
| **Compaction/summarization for >20 MB** | Genuine-overflow-only path (UX principle: don't default to truncation). Until P0 measurements show real sessions approaching the budget, building it would be speculative. |
| **Paranoid redaction mode / turn-range sharing** | Security-posture options deferred until the default posture (redact + mandatory preview) is validated with real users. |

## Test strategy

- **Golden tests** (P0 onward): every `--json` shape the server decodes, pinned to
  the CLI version; CI fails loudly on drift with "re-pin and re-golden"
  instructions.
- **E2E against fmsg-docker**: two seeded accounts + their `_claude` sub-accounts;
  scenarios — root share, re-share (pid chain), plain-client reply, resume from
  mid-thread (branch exclusion asserted), resume of a plain thread (no snapshot),
  oversize refusal, expired-key error.
- **Serializer property tests**: JSONL fixtures from real Claude Code sessions
  (A3-pinned version), round-trip envelope → assembled context.
- **Redaction corpus**: known-secret fixtures (fake keys of each family) must all
  be caught; report asserts counts.
