# fmsg-mcp-claude

An MCP server that bridges Claude sessions and [fmsg](https://github.com/markmnl/fmsg)
threads: share a working Claude session with a teammate as an fmsg message
(readable in any fmsg client), and resume a Claude session seeded with the
ancestor history of any fmsg thread message.

## Quick start

**You need an fmsg address and an API key** (`fmsgk_…`) on an fmsg host. No
host? Create a free account at **[fmsg.io](https://fmsg.io)** and generate an
API key under your account.

**Claude Desktop (macOS / Windows)**

1. Download the `.mcpb` bundle for your platform from the
   [latest release](https://github.com/markmnl/fmsg-mcp-claude/releases/latest)
   (`fmsg-darwin-arm64.mcpb`, `fmsg-darwin-x64.mcpb`, or `fmsg-windows-x64.mcpb`).
2. Open it with Claude Desktop (double-click, or Settings → Extensions →
   Install Extension…) and paste your fmsg Web API URL and API key when
   prompted.
3. Done — ask Claude to *"share this session with @friend@example.com via
   fmsg"* or *"continue my latest fmsg thread"*.

**Claude Code (Linux / macOS / Windows)**

1. Download `fmsg-mcp_<os>_<arch>` and `fmsg_<os>_<arch>` from the
   [latest release](https://github.com/markmnl/fmsg-mcp-claude/releases/latest)
   and make them executable (e.g. into `~/bin`).
2. Register the server:
   ```sh
   claude mcp add fmsg \
     --env FMSG_API_URL=<your fmsg Web API URL> \
     --env FMSG_API_KEY=fmsgk_... \
     --env FMSG_CLI=~/bin/fmsg \
     -- ~/bin/fmsg-mcp
   ```
3. Optional but recommended: install the [SessionStart hook](#install) so
   shares capture your session verbatim. Then `/mcp__fmsg__share_session` and
   `/mcp__fmsg__continue_thread` are available in any session.

Sharing sends one Markdown fmsg message per prompt, pid-chained — recipients
read it in any fmsg client, or load the thread into Claude (or any other
agent) as context. Sending is permanent: fmsg messages cannot be recalled,
edited, or deleted.

Design documents: [ARCHITECTURE.md](./ARCHITECTURE.md) ·
[TOOLS.md](./TOOLS.md) · [UX_FLOWS.md](./UX_FLOWS.md) ·
[INTERFACES.md](./INTERFACES.md) · [OPEN_QUESTIONS.md](./OPEN_QUESTIONS.md) ·
[PHASED_PLAN.md](./PHASED_PLAN.md)

## Requirements

- [fmsg-cli](https://github.com/markmnl/fmsg-cli) with the global `--json` flag
  on `PATH` (or point `FMSG_CLI` at the binary). The server refuses to run
  against builds without `--json`.
- An fmsg sub-account API key (`fmsgk_…`), provisioned by an owner via
  `fmsg sub-accounts create <you>_claude --cidr … --expires …`.

## Install

```sh
go build -o fmsg-mcp .

# Claude Code
claude mcp add fmsg -- ./fmsg-mcp

# SessionStart hook (verbatim share needs it to find your session transcript)
mkdir -p ~/.claude/fmsg-mcp
cp hooks/session-start.sh ~/.claude/fmsg-mcp/
chmod +x ~/.claude/fmsg-mcp/session-start.sh
# then register it under hooks.SessionStart in ~/.claude/settings.json
```

The shared session travels as a **plain Markdown fmsg message** — the body is
the whole payload. Any fmsg client renders it; any agent (Claude or otherwise)
can load the thread into context by walking the `pid` chain. The identity you
send as is simply whatever address your configured API key grants.

## Configuration (environment)

| Variable | Default | Purpose |
|---|---|---|
| `FMSG_API_URL` | `http://127.0.0.1:8000` | fmsg-webapi base URL (passed through to fmsg-cli) |
| `FMSG_API_KEY` | — | `fmsgk_…` sub-account key (passed through to fmsg-cli); its granted address is the from address |
| `FMSG_CLI` | `fmsg` | Path to the fmsg-cli binary |
| `FMSG_DEFAULT_DOMAIN` | — | Convention fallback for resolving `bob` → `@bob@<domain>` |
| `FMSG_DIRECTORY` | — | JSON file mapping short names to full addresses |

## Tools

| Tool | What it does |
|---|---|
| `share_session` | Two-phase share: preview (recipients, size, redactions) → confirm → send the transcript as a Markdown fmsg message to exactly the recipients given |
| `continue_thread` | Walk a message's `pid` chain to the root and return every body on the lineage, in order, as context (`-1` = latest inbox message) |
| `reply_to_thread` | Markdown reply into a thread; defaults to all participants of the replied-to message |
| `list_threads` | Recent inbox messages (id, sender, topic, root or reply) |
| `whoami` / `resolve_address` | Identity introspection and dry-run name resolution |

`share_session` and `continue_thread` are also MCP prompts — in Claude Code:
`/mcp__fmsg__share_session bob` and `/mcp__fmsg__continue_thread`.

## Development

```sh
go test ./...
```

End-to-end testing uses the workspace `fmsg-docker` stack
(`start-local-stack.sh` + `seed-local-accounts.sh` at the workspace root);
see PHASED_PLAN.md §P0.
