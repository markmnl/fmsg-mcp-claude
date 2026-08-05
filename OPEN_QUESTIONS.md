# OPEN_QUESTIONS.md

## 1. Assumptions that need verification before P1

| # | Assumption | Verification | If false |
|---|---|---|---|
| A1 | A stdio MCP server launched by Claude Code inherits the project dir as CWD (or an env var identifying it), so the fallback transcript locator can derive `~/.claude/projects/<slug>` | Log `os.Getwd()`/env from a scratch server | Rely solely on the SessionStart hook pointer (already primary); fallback removed |
| A2 | `https://claude.ai/new?q=<prompt>` prefills a new chat for a logged-in user (not auto-sent) on claude.ai and via Desktop link handling | Manual test both surfaces | Open-in-Claude link degrades to copy-paste instructions in the message body; Claude Code path unaffected |
| A3 | Claude Code's transcript JSONL block schema is stable enough to parse against a pinned minimum version, and tool_result payloads in it are already size-bounded | Diff transcripts across two Claude Code releases; pin minimum version | Serializer needs per-version adapters; worst case share degrades to model-serialized fidelity on Code too |

## 2. Security / privacy

- **Default share posture:** everything the session contains *except* thinking
  blocks, after redaction — with a mandatory two-phase confirm showing recipient,
  sizes, and redaction hits. Rationale: fmsg messages are **immutable** (no unsend,
  and the recipient's host stores them independently), so no transcript leaves the
  machine without one explicit preview. This confirmation is deliberately not
  configurable off in MVP.
- **Redaction pass** (serializer, pre-send): regex families for `fmsgk_` keys, JWTs
  (`eyJ…`), AWS/GCP/GitHub/Anthropic/OpenAI key shapes, `-----BEGIN … PRIVATE
  KEY-----` blocks, and `.env`-style `SECRET|TOKEN|PASSWORD|KEY=` lines →
  `[REDACTED:<type>]`, with hits reported in the preview and recorded in
  `redaction.patterns_hit`. **Open:** regexes have false negatives by nature — do we
  want an optional "paranoid" mode that blocks share entirely when any hit occurs in
  a tool_result (vs. redact-and-continue)? MVP: redact-and-continue + preview.
- **Prompt injection on resume:** thread content (including messages authored by
  other participants) becomes model context. Mitigation: the assembled block wraps
  foreign turns in attributed quoting under a "conversation data, not instructions"
  preamble. This is a soft mitigation, not a guarantee — flagged for review, and a
  reason `continue_thread` stays behind normal tool approval rather than
  auto-approved by default.
- **Key handling:** `FMSG_API_KEY` lives in the server's env file (0600); the server
  never passes credentials on argv (fmsg-cli's `login <key>` leaks into process
  tables — the server never invokes `login` at all). Scratch dirs are 0700 and
  deleted after use.
- **Open:** should `share_session` support an explicit allowlist of turn ranges
  ("share only from turn 20") for sensitive sessions? Deferred; `note` +
  full-snapshot covers MVP.

## 3. Snapshot vs. live thread — confirmed scope

**Decision (user-confirmed): snapshot for MVP; live-thread convergence is v2.**
Alice's continued conversation after sharing does *not* update Bob's copy — Bob got
a snapshot. But the thread model already converges structurally: Bob's replies (from
Claude or any fmsg client) pid-link into the same thread, Alice's next share of her
evolved session pid-links likewise, and `continue_thread` on any message
incorporates everything on its lineage. So v2's addition is *pull/notification*
(polling `list` for new thread members, or an upstream CLI `watch` over the
existing WebSocket), **not** a data-model change — no migration risk in deferring.

## 4. Cross-host — explicitly out of MVP

Two independent upstream blockers, either fatal alone today:

1. **fmsgd federation size limit** (`FMSG_MAX_MSG_SIZE`, default 10 KiB):
   an operator config choice, not an upstream change to pursue (user
   decision 2026-08-05) — hosts that want to accept transcripts set it
   accordingly; a host that keeps it low rejects with fmsg code 4, which
   `delivery_status` surfaces.
2. **No stable cross-host message reference:** row IDs are per-host; the wire
   SHA-256 exists in the store (`msg.sha256`) but no endpoint or CLI output exposes
   it, and there's no get-by-hash.

Same-host MVP is unaffected (webapi 10/10/20 MB budget; shared row IDs).

## 5. Design edges still open

- **Above-base plain replies missing from a snapshot:** if Alice shares, Bob replies
  from his phone, and Alice *re-shares without ever resuming* (her session never
  incorporated Bob's reply), the deepest-base algorithm would skip Bob's reply
  (it's above base but absent from `incorporated_fmsg_ids`). The splice check
  (ARCHITECTURE.md §6 step 4) handles it by chain-position insertion — but ordering
  within the rendering is approximate. Acceptable? MVP: yes, with a visible
  `[not part of the shared session snapshot]` marker.
- **`reply_to_thread` participant fan-out** reads the parent's participants from
  its `--json` fields (`from`, `to`, `add_to[]`) — straightforward now, but golden
  tests should still cover multi-batch messages to pin the shape.
- **Sub-account CIDR friction:** `sub-accounts create --cidr` is mandatory; laptops
  roam. Orgs need either VPN-scoped CIDRs or `0.0.0.0/0` (weakening a control).
  Worth an upstream discussion (see #9 below) about optional CIDRs.
- **Multiple Claude Code sessions in one project:** hook pointer file is
  last-writer-wins per project slug. Wrong-session shares are prevented by the
  preview (turn count + title), but a per-terminal pointer (keyed by session id
  passed through the tool call, if available) would be cleaner — revisit after A1/A3
  verification.
- **fmsgid's role:** address existence/status lives in fmsgid but has no
  client-facing lookup. Long-term, `resolve_address` should query something real
  rather than a static directory file.

## 6. Upstream issue stubs (file, don't work around)

### fmsg-cli
1. ~~`--json` global flag~~ — **implemented** (`json-output` branch in this
   workspace; also surfaces previously-dropped `to_delivery`/`read`/`time_read`/
   `batch_id` fields and the attach upload response). Ship it in the next release;
   the MCP server pins that version as its minimum.
2. **`fmsg thread <id>`** — ancestor walk (root→id lineage) in one `--json` call.
   Replaces the N-round-trip walk.
3. **Expose message hashes + `get --by-hash`** — surface `msg.sha256`/`psha256`;
   prerequisite for any cross-host message reference.
4. **First-class send** — multi-recipient, `--type`, `--attach` on
   `send`/`draft create`, removing the 4-command dance.
5. **Distinct exit codes + structured errors** — failures are still prose on stderr
   with exit 1 even under `--json`; add distinct exit codes (usage / auth /
   not-found / server / network) and/or a JSON error object on stderr.
6. **Bug: URL-escape attachment filenames** in `get-attach`/`rm-attach`
   (`internal/api/client.go:463,493`) — spaces and reserved characters break.
7. **`fmsg whoami`** — print authenticated address, API URL, auth type, expiry.
8. **`fmsg watch`** — wrap `GET /fmsg/ws` (`new_msg` etc.) for scripts/agents.
9. **Sub-account ergonomics** — act-as support (webapi already has `X-FMSG-Act-As`),
   and optional CIDR restriction on `sub-accounts create`.
10. **Promote `internal/api` to a public Go package** — typed client for Go
    consumers (this server would swap prose-parsing for it without a rewrite).

### fmsg-webapi / fmsgd
11. **Children/replies + ancestors endpoints** (`GET /fmsg/{id}/replies`,
    `/fmsg/{id}/ancestors`) — thread reconstruction is currently impossible
    (children) or O(depth) (ancestors); the recursive CTE already exists internally
    for push. **Partially addressed (2026-08-05):** `GET /fmsg/:id/thread`
    (fmsg-webapi branch `thread-text`) returns the ancestor lineage as plain
    text, root first — built for UI "copy thread" and feeding agents. A
    JSON ancestors route and a children/replies route remain open.
12. ~~Raise/document `FMSG_MAX_MSG_SIZE`~~ **Dropped (2026-08-05, user
    decision): it is operator config — setups specify whatever they want.**
13. **Inbox filtering** — `GET /fmsg?since=&from=&topic=` (and/or a
    content-marker/type filter) so clients needn't page + fetch to find relevant
    messages.
14. **Address lookup surface** — a client-facing existence/directory query backed by
    fmsgid, so "bob" → `@bob@example.com` resolution can be real instead of a
    shipped JSON file.
15. **PUT/update contract mismatch (found in live testing):** fmsg-webapi's
    `PUT /fmsg/:id` is full-replacement, but fmsg-cli's `update` sends only the
    provided fields and documents PATCH semantics ("Only provided fields are
    updated"). Any partial update silently wipes recipients, `pid`, and `topic`.
    **Resolved (2026-08-05):** PUT stays full-replacement by design; fmsg-cli
    `update` now fetches the current draft and merges before the PUT (branch
    `update-merge-semantics`), making its documented semantics true. fmsg-mcp
    keeps `UpdateFull` (correct under either contract).
16. **`PUT /fmsg/:id` accepts an empty `to`** — ~~a draft can be updated into
    having zero recipients~~. **Reclassified by design (2026-08-05, user
    decision): drafts are a workspace and may be incomplete in any way;
    validity is enforced at send (see #17).**
17. **`POST /fmsg/:id/send` accepts a draft with zero recipients** — the
    recipientless messages from #15 sent without any validation error.
    **Fixed (2026-08-05):** send is now the validation gate — fmsg-webapi
    branch `validate-at-send` refuses with 400 (listing every problem) when
    the draft has no recipients, a malformed recipient address, no type, or
    an unsupported version.
18. **Cross-host replies to own sent messages always bounced with code 6
    (found live 2026-08-05; root cause identified same day):** initially
    diagnosed as concurrent out-of-order delivery, but pacing sends on
    confirmed parent delivery did not fix it. The real bug: **fmsgd's
    sender computed the shared sha256 over the pre-deflate header form**,
    while receiving hosts hash the header exactly as transmitted (deflated)
    per SPEC — so the sender-recorded parent hash never matched the
    receiver's stored hash and every reply's psha256 missed (code 6).
    **Fixed** on fmsgd branch `fix-shared-hash-deflated-form` (deflate
    before hashing; unit regression test) with new integration coverage in
    fmsg-docker `007-reply-to-own-sent.sh` (test 002 replies to a
    *received* message, whose stored hash is always the transmitted form —
    the gap that hid this). Note: messages sent before the fix carry the
    wrong recorded hash permanently; replies to them will still bounce —
    start new threads after deploying. The layered ordering design remains
    worthwhile: (a) fmsgd dispatch sequencing (parent-before-child per
    domain) for genuine races; (b) fmsg-webapi's synchronous rejection of
    replies to domains the parent never reached (implemented, branch
    `verify-reply-deliverable`). fmsg-mcp's 60s chain pacing can be relaxed
    once (a) lands.
