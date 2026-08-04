# Claude Instructions

**Read [STATUS.md](./STATUS.md) first** — it records where the project is,
the decisions already made (and reversed), live-testing findings, and the
prioritized remaining work. Do not re-litigate decisions logged there
without new evidence.

## Project

`fmsg-mcp`: a Go stdio MCP server sharing Claude sessions as fmsg threads
(Markdown messages, one per user prompt, pid-chained) and resuming fmsg
threads as Claude context. All fmsg access shells out to `fmsg-cli --json`
via `internal/cli` — never call fmsg-webapi over HTTP (see STATUS.md
decision 1 for the agreed future exception).

## Commands

```sh
go build -o fmsg-mcp .    # dev build; uses fmsg CLI from PATH or FMSG_CLI
go test ./...
gofmt -l . && go vet ./...
# release-style build (CLI embedded):
#   cp <fmsg binary> internal/cli/embedded/fmsg-bin && go build -tags embedcli .
```

## Invariants

- Every webapi-bound `update` must restate all fields (`cli.UpdateFull`) —
  the webapi PUT is full-replacement (upstream bug, OPEN_QUESTIONS #15).
- CLI argv: `--json` first; flags before any negative message index; message
  bodies passed as temp-file paths, never inline text.
- Sending is final: never bypass the two-phase preview/confirm or the
  redaction pass. Phrase immutability as "cannot be edited or recalled" —
  not "cannot be deleted" (hosts may delete from their own stores) and not
  "cannot be unsent" (models garble it into "unsendable").
- Transcript parsing must stay tolerant (assumption A3): skip unknown JSONL
  line/block types; always exclude thinking blocks and harness meta content.

## Docs

[STATUS.md](./STATUS.md) (current truth) · [README.md](./README.md) (users)
· design history: ARCHITECTURE.md / TOOLS.md (v0.1 design under a v0.2
supersession banner — trust the banner and STATUS.md over the body),
INTERFACES.md, UX_FLOWS.md, OPEN_QUESTIONS.md (incl. upstream issue list),
PHASED_PLAN.md.
