package session

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

const (
	// bodyCap keeps the rendered body under the webapi's default 10MB data
	// limit with headroom; beyond it, middle turns are elided (the preview
	// reports the size so the user sees before approving).
	bodyCap = 4 << 20
	// toolResultCap bounds each tool result in the rendering — the body is
	// the sole carrier, so results are included rather than collapsed, but
	// bounded so one giant log doesn't dominate the message.
	toolResultCap = 1500
)

// RenderMarkdown produces the fmsg message body: a short provenance header
// followed by the conversation. Any fmsg client renders it; any agent can
// load it as context.
func RenderMarkdown(t *Transcript) string {
	var b strings.Builder
	fmt.Fprintf(&b, "# %s\n\n", orDefault(t.Title, "Claude session"))
	fmt.Fprintf(&b, "*Claude session shared by %s · %s · %s · %s · %d turns*\n\n---\n\n",
		t.SharerAddress, t.Surface, t.Fidelity,
		time.Unix(int64(t.SharedAt), 0).UTC().Format("2006-01-02 15:04 UTC"),
		len(t.Turns))

	body := renderTurns(t.Turns)
	if len(body) > bodyCap {
		body = elideMiddle(t.Turns)
	}
	b.WriteString(body)
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
		fmt.Fprintf(&b, "\n*… %d middle turns elided to fit the message size limit …*\n\n", len(turns)-head-tail)
		for _, t := range turns[len(turns)-tail:] {
			renderTurn(&b, t)
		}
		if b.Len() <= bodyCap {
			return b.String()
		}
		head, tail = head/2, tail/2
	}
	return fmt.Sprintf("*Transcript too large to render (%d turns).*\n", len(turns))
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
			fmt.Fprintf(b, "> 🔧 %s: `%s`\n\n", blk.Name, summarizeInput(blk.Input))
		case "tool_result":
			text := strings.TrimSpace(blk.Text)
			if text == "" {
				continue
			}
			if len(text) > toolResultCap {
				text = text[:toolResultCap] + "\n…(truncated)"
			}
			fmt.Fprintf(b, "```\n%s\n```\n\n", text)
		}
	}
}

func summarizeInput(input any) string {
	raw, err := json.Marshal(input)
	if err != nil {
		return ""
	}
	s := string(raw)
	s = strings.ReplaceAll(s, "`", "'")
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
