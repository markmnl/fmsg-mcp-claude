# UX_FLOWS.md — walked flows with step counts

"Step" = a user action (typing a message, clicking, approving a tool). Model/tool
activity is shown indented and doesn't count. The bar is **trivial**: ≤2 user
actions per flow; anything over is flagged with why and what would cut it.

## Surface capability matrix

| | Share | Resume |
|---|---|---|
| **Claude Code** | ✅ verbatim (session JSONL) | ✅ tool result / slash command |
| **Claude Desktop** | ⚠️ model-serialized only | ✅ tool result |
| **claude.ai / Cowork** | ⚠️ model-serialized only | ✅ tool result (entry via prefill link) |
| **Any fmsg client (no Claude)** | n/a | reads the Markdown body; replies join the thread as plain messages |

Verbatim share needs filesystem access to the conversation store, which only
Claude Code gives an MCP server. On other surfaces the transcript is the model's
own serialization, disclosed as such in the message (`fidelity:
model-serialized`). Resume is surface-independent because context arrives as a
tool *result*.

## 1. Share — Claude Code — **2 steps**

1. Alice: *"Share this session with Bob on fmsg"* (or
   `/fmsg:share_session bob`).
   - `share_session` phase 1 runs: locates the session JSONL, serializes,
     redacts, resolves `bob` → `@bob@example.com`; Claude presents the preview —
     recipient(s), turn count, sizes, redaction hits, "cannot be unsent".
2. Alice approves → phase 2 sends (draft/update/attach/send/add-to).
   - Claude reports the fmsg id and the Open-in-Claude link Bob will see.

The two steps are irreducible by design: fmsg messages are immutable, so a
transcript that may contain secrets never leaves the machine without one explicit
look at what's going and to whom.

## 2. Share — claude.ai / Desktop — **2 steps**

Same two actions; the tool call carries `transcript` (model-serialized) because
the server can't read the conversation store. The preview additionally notes the
fidelity downgrade. (Where MCP App UI buttons are available, step 1 becomes a
click on a "Share via fmsg" button; the confirm remains.)

## 3. Resume — Claude Code — **2 steps** (1 with pre-approval)

Bob was told (or sees in his fmsg client) that Alice shared a session.

1. Bob, in his project: `/fmsg:continue_thread` (no id needed —
   defaults to `-1`, the latest inbox message) or *"continue the fmsg thread from
   Alice"*.
2. Bob approves the tool call.
   - The walk/assembly runs; Claude now has the full lineage as context and says
     what the session was about and where it left off. Bob just keeps talking.

With the fmsg tools pre-approved in Claude Code settings this is **1 step**. Bob
never explains context — that's the point.

## 4. Resume — from an fmsg client into claude.ai — **3 steps** ⚠️

Bob is reading the thread on his phone/web fmsg client.

1. Bob clicks **Open in Claude** (the `https://claude.ai/new?q=…` link in the
   message body).
2. claude.ai opens a new chat with the instruction prefilled ("Use the fmsg tool
   continue_thread with message id 42"); Bob hits send.
3. Bob approves the tool call → seeded session.

**Over the 2-step bar.** Why: the prefill link can't auto-send (and shouldn't —
the user must own tool execution in a fresh chat), and MCP tool calls need one
approval. What would cut it: an MCP App surface where the fmsg "connector" renders
the thread with its own **Continue in Claude** button (collapses steps 1–2), or an
fmsg client deep-link that auto-runs a pre-authorized prompt. Both are v2; neither
blocks the MVP because step 2 is literally pressing Enter on a prefilled prompt.

## 5. Reply from fmsg only (Bob has no Claude at hand) — **1 step**

1. Bob replies in his fmsg client like to any message.
   - His reply is a plain fmsg message pid-linked into the thread. The next
     `continue_thread` by anyone incorporates it as a `[@bob@example.com via
     fmsg]` turn. Nothing to configure — this is native fmsg behavior.

## 6. What Bob's phone shows

```
Topic: Fixing the auth refresh bug
From: @alice_claude@example.com          (added: @bob_claude, @alice)
────────────────────────────────────────
Claude session — shared by @alice@example.com · claude-code · verbatim
2026-08-04 16:20 UTC · 42 turns

Open in Claude: https://claude.ai/new?q=Use%20the%20fmsg%20tool%20…
Claude Code: /fmsg:continue_thread   (full transcript attached)

**Alice:** The token refresh loop is 401ing after rotation…
**Claude:** Looking at auth/manager.go, the refresh path re-uses…
> ran Bash: go test ./internal/auth/
**Claude:** Two failures point at expiry skew…
…
────────────────────────────────────────
📎 claude-session.json.gz (210 KB)
```

Readable as a plain message; machine-recoverable from the attachment.

## 7. Install — teammate (non-technical) — **2 steps**

Precondition (admin, once per teammate, out-of-band): `fmsg sub-accounts create
bob_claude --cidr <org-cidr> --expires <date>` on the org's fmsg host; send Bob
the one-time `fmsgk_` key and (already in the installer) the org's
`FMSG_API_URL`.

1. Bob runs the org installer one-liner (e.g. `curl -fsSL
   https://example.com/install-fmsg-mcp.sh | sh`). It: installs the pinned
   `fmsg-cli` and `fmsg-mcp` binaries (single static Go binaries, no runtime);
   writes the env file with the org's `FMSG_API_URL`, `FMSG_DEFAULT_DOMAIN`,
   `FMSG_DIRECTORY`; installs the SessionStart hook; runs `claude mcp add fmsg --
   fmsg-mcp` scoped to the user.
2. Bob pastes the `fmsgk_` key when the script prompts.

Done — next Claude Code start has `/fmsg:…` available. No fmsg knowledge, no
hand-edited config. The honest limit: step 2 (a per-user credential) cannot be
removed given fmsg-cli's auth model; org SSO auto-provisioning of sub-account keys
would require IdP-side automation that is deployment-specific and out of scope
here.

## 8. Step-count summary

| Flow | Steps | Bar (≤2) |
|---|---|---|
| Share, Claude Code | 2 | ✅ |
| Share, claude.ai/Desktop | 2 | ✅ |
| Resume, Claude Code | 2 (1 pre-approved) | ✅ |
| Resume, fmsg client → claude.ai | 3 | ⚠️ flagged, §4 |
| Reply from fmsg only | 1 | ✅ |
| Install, teammate | 2 | ✅ |
