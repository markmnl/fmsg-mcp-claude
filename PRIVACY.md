# Privacy Policy — fmsg MCP server

*Last updated: 2026-08-07*

`fmsg-mcp` is a local MCP server that shares Claude session content over the
[fmsg](https://github.com/markmnl/fmsg) protocol, at your request, to
recipients you name. This policy describes what data it touches and where
that data goes.

## What the server reads

- **Claude session transcripts** on your machine
  (`~/.claude/projects/…/*.jsonl`) — only when you ask it to share or
  summarise a session. Thinking blocks, tool commands, and tool output are
  never included in shares; only the conversation text (your prompts and
  Claude's replies) is.
- **Local configuration**: the fmsg API URL, API key, and optional name
  directory you configure; local share bookkeeping under
  `~/.claude/fmsg-mcp/`.
- **Your fmsg inbox** on the host you configure — only when you ask Claude
  to continue a thread, list threads, or run chat mode (wait for and reply
  to incoming messages). Inbound message bodies are shown to Claude as
  conversation data; nothing in them is executed.

## Where data goes

- Message content is sent **only to the fmsg host you configure**
  (`FMSG_API_URL`), addressed to the recipients you specify. Your host then
  delivers to the recipients' hosts per the fmsg protocol (federation, like
  email). Nothing is sent anywhere else.
- **Nothing is sent to the author of this software.** There is no telemetry,
  no analytics, no crash reporting, and no third-party service involved.

## Safeguards

- **Preview and confirm**: session and summary shares show you the
  recipients, size, and redactions, and nothing is sent until you approve.
  Direct sends (`send_message`) and chat-mode replies (`reply_to_thread`
  after `wait_for_message`) go out immediately at your explicit request.
- **Secret redaction**: known credential patterns (API keys, tokens, private
  keys, JWTs, environment secrets) are redacted from outgoing content before
  it leaves your machine, and every redaction is reported to you.
- **Immutability**: sent fmsg messages cannot be edited or recalled — this is
  why shares always preview first.

## Your API key

Your `fmsgk_…` API key is stored by your Claude client's extension
configuration and is sent only to the fmsg API URL you configured, to
authenticate as you. Revoke it at any time on your fmsg host.

## Contact

Questions or concerns: open an issue at
<https://github.com/markmnl/fmsg-mcp-claude/issues>.
