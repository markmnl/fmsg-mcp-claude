# fmsg-mcp-claude

Share your Claude session with anyone as an [fmsg](https://github.com/markmnl/fmsg)
thread — and open any fmsg thread in Claude with the full conversation as
context. Shares are plain Markdown messages: readable in any fmsg client,
usable by any agent.

## 1. Get an fmsg address + API key

You send as an fmsg address, authenticated by an API key (`fmsgk_…`):

- **No fmsg host?** Create a free account at **[fmsg.io](https://fmsg.io)**
  and generate an API key under your account.
- **Self-hosting?** Run your own host with
  [fmsg-docker](https://github.com/markmnl/fmsg-docker).

## 2. Install

### Claude Desktop (macOS / Windows)

1. Download the bundle for your platform from the
   [latest release](https://github.com/markmnl/fmsg-mcp-claude/releases/latest):
   `fmsg-darwin-arm64.mcpb` · `fmsg-darwin-x64.mcpb` · `fmsg-windows-x64.mcpb`
2. Double-click it (or Claude Desktop → Settings → Extensions → Install
   Extension…) and paste your fmsg Web API URL and API key when prompted.

That's it — everything needed is inside the bundle.

### Claude Code (Linux / macOS / Windows)

1. From the [latest release](https://github.com/markmnl/fmsg-mcp-claude/releases/latest),
   download `fmsg-mcp_<os>_<arch>` for your platform, put it somewhere like
   `~/bin/fmsg-mcp`, and `chmod +x` it. That's the only file — the fmsg CLI it
   uses is built in.
2. Register the server:

   ```sh
   claude mcp add fmsg \
     --env FMSG_API_URL=<your fmsg Web API URL> \
     --env FMSG_API_KEY=fmsgk_... \
     -- ~/bin/fmsg-mcp
   ```

3. Recommended — install the session hook so shares capture your session
   verbatim (without it, sharing still works from what Claude retells):

   ```sh
   mkdir -p ~/.claude/fmsg-mcp
   cp hooks/session-start.sh ~/.claude/fmsg-mcp/ && chmod +x ~/.claude/fmsg-mcp/session-start.sh
   ```

   and add to `~/.claude/settings.json`:

   ```json
   {"hooks": {"SessionStart": [{"hooks": [{"type": "command", "command": "~/.claude/fmsg-mcp/session-start.sh"}]}]}}
   ```

### Claude Web (claude.ai)

Not yet — claude.ai only connects to *remote* MCP servers, and fmsg-mcp
currently runs locally. Use Claude Desktop or Claude Code for now; a hosted
connector is on the roadmap. (Resuming works everywhere the server runs,
because context is returned by the tools themselves.)

## 3. Use it

Just ask Claude, in any session:

- *"Share this session with @kebbie@fmsg.io"* — you'll get a preview
  (recipients, size, redactions) and nothing is sent until you approve.
- *"Continue my latest fmsg thread"* — Claude loads the whole thread as
  context and picks up where it left off.
- *"Reply to that thread: we shipped the fix"* — sends a Markdown reply to
  all participants.
- *"Did my share reach everyone?"* — per-recipient delivery status.

In Claude Code these are also slash commands: `/fmsg:share_session`
and `/fmsg:continue_thread`.

**How shares work:** one Markdown fmsg message per prompt, chained with
fmsg's reply links — the thread mirrors your conversation, so recipients can
read it in any fmsg client, branch from any point, or load it into Claude or
any other agent. Secrets (API keys, tokens, private keys) are redacted before
sending. **Sending is permanent** — fmsg messages cannot be recalled, edited,
or deleted — which is why every share shows a preview first.

## Configuration

| Variable | Default | Purpose |
|---|---|---|
| `FMSG_API_URL` | `http://127.0.0.1:8000` | Your host's fmsg Web API base URL |
| `FMSG_API_KEY` | — | `fmsgk_…` API key; its granted address is who you send as |
| `FMSG_CLI` | *(built in)* | Override the bundled [fmsg-cli](https://github.com/markmnl/fmsg-cli) with your own binary |
| `FMSG_DEFAULT_DOMAIN` | — | Lets short names resolve: `bob` → `@bob@<domain>` |
| `FMSG_DIRECTORY` | — | JSON file mapping short names to full addresses |

## Development

```sh
go build -o fmsg-mcp .   # dev build: uses fmsg-cli from PATH (needs --json/whoami)
go test ./...
# release-style build with the CLI embedded:
#   cp <fmsg binary> internal/cli/embedded/fmsg-bin && go build -tags embedcli .
```

Design docs: [ARCHITECTURE.md](./ARCHITECTURE.md) · [TOOLS.md](./TOOLS.md) ·
[UX_FLOWS.md](./UX_FLOWS.md) · [INTERFACES.md](./INTERFACES.md) ·
[OPEN_QUESTIONS.md](./OPEN_QUESTIONS.md) · [PHASED_PLAN.md](./PHASED_PLAN.md)
