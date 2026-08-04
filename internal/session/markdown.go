package session

import (
	"encoding/json"
	"fmt"
	"net/url"
	"strings"
	"time"
)

// bodyCap is the Markdown body size limit (ARCHITECTURE.md §2): middle turns
// are elided from the rendering only — the JSON attachment stays complete.
const bodyCap = 256 << 10

// RenderMarkdown produces the human-readable message body shown by any fmsg
// client. First line is BodyMarker so transcript-bearing messages are
// detectable from short_text.
func RenderMarkdown(env *Envelope, openInClaudeID int64) string {
	var b strings.Builder
	b.WriteString(BodyMarker + "\n")
	fmt.Fprintf(&b, "# %s\n\n", orDefault(env.Title, "Claude session"))
	fmt.Fprintf(&b, "Claude session — shared by %s · %s · %s\n",
		orDefault(env.Provenance.SharerHumanAddress, env.Provenance.SharerAddress),
		env.Provenance.Surface, env.Provenance.Fidelity)
	fmt.Fprintf(&b, "%s · %d turns\n\n",
		time.Unix(int64(env.Provenance.SharedAt), 0).UTC().Format("2006-01-02 15:04 UTC"),
		len(env.Turns))
	if openInClaudeID > 0 {
		prompt := fmt.Sprintf("Use the fmsg tool continue_thread with message id %d", openInClaudeID)
		escaped := strings.ReplaceAll(url.QueryEscape(prompt), "+", "%20")
		fmt.Fprintf(&b, "**Open in Claude:** https://claude.ai/new?q=%s\n", escaped)
		fmt.Fprintf(&b, "Claude Code: `/mcp__fmsg__continue_thread %d` (full transcript attached)\n\n", openInClaudeID)
	}
	b.WriteString("---\n\n")

	turns := renderTurns(env.Turns)
	if len(turns) > bodyCap {
		turns = elideMiddle(env.Turns)
	}
	b.WriteString(turns)
	return b.String()
}

func renderTurns(turns []Turn) string {
	var b strings.Builder
	for _, t := range turns {
		renderTurn(&b, t)
	}
	return b.String()
}

// elideMiddle keeps the first and last turns and drops the middle until the
// rendering fits under bodyCap.
func elideMiddle(turns []Turn) string {
	head, tail := len(turns)/4, len(turns)/4
	for head+tail >= 2 {
		var b strings.Builder
		for _, t := range turns[:head] {
			renderTurn(&b, t)
		}
		fmt.Fprintf(&b, "\n*… %d turns elided — full transcript in the attached %s …*\n\n",
			len(turns)-head-tail, AttachmentName)
		for _, t := range turns[len(turns)-tail:] {
			renderTurn(&b, t)
		}
		if b.Len() <= bodyCap {
			return b.String()
		}
		head, tail = head/2, tail/2
	}
	return fmt.Sprintf("*Transcript too large to render — see the attached %s (%d turns).*\n", AttachmentName, len(turns))
}

func renderTurn(b *strings.Builder, t Turn) {
	label := "Claude"
	if t.Role == "user" {
		label = "User"
	}
	for _, blk := range t.Blocks {
		switch blk.Type {
		case "text":
			fmt.Fprintf(b, "**%s:** %s\n\n", label, strings.TrimSpace(blk.Text))
		case "tool_use":
			fmt.Fprintf(b, "> ran %s: %s\n\n", blk.Name, summarizeInput(blk.Input))
		case "tool_result":
			// Collapsed in the rendering; full text is in the attachment.
		}
	}
}

func summarizeInput(input any) string {
	raw, err := json.Marshal(input)
	if err != nil {
		return ""
	}
	s := string(raw)
	if len(s) > 160 {
		s = s[:160] + "…"
	}
	return s
}

func orDefault(s, def string) string {
	if s == "" {
		return def
	}
	return s
}
