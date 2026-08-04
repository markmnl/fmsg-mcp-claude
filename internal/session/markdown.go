package session

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

const (
	// exchangeCap bounds a single rendered message body. With per-prompt
	// granularity bodies are naturally small; this is a guard well under the
	// webapi's default 10MB data limit (compression is the host's concern).
	exchangeCap = 4 << 20
	// toolResultCap bounds each tool result in the rendering: head and tail
	// are kept with the middle cut, since the start (what ran) and the end
	// (the outcome/error) carry most of the signal.
	toolResultCap = 1500
)

// RenderExchanges splits the transcript at user prompts and renders one
// Markdown body per exchange: a user prompt plus everything up to the next
// user prompt. Sharing sends one fmsg message per exchange, pid-chained, so
// the fmsg thread mirrors the conversation and receivers can branch from or
// resume up to any prompt. The first body carries the provenance header.
func RenderExchanges(t *Transcript) []string {
	groups := splitExchanges(t.Turns)
	bodies := make([]string, 0, len(groups))
	for i, g := range groups {
		var b strings.Builder
		if i == 0 {
			fmt.Fprintf(&b, "# %s\n\n", orDefault(t.Title, "Claude session"))
			fmt.Fprintf(&b, "*Claude session shared by %s · %s · %s · %s · %d turns in %d messages*\n\n---\n\n",
				t.SharerAddress, t.Surface, t.Fidelity,
				time.Unix(int64(t.SharedAt), 0).UTC().Format("2006-01-02 15:04 UTC"),
				len(t.Turns), len(groups))
		}
		body := renderTurns(g)
		if len(body) > exchangeCap {
			body = elideMiddle(g)
		}
		b.WriteString(body)
		bodies = append(bodies, b.String())
	}
	return bodies
}

// splitExchanges starts a new group at every user turn that carries actual
// user text; tool_result-only user turns belong to the assistant's activity
// within the current exchange.
func splitExchanges(turns []Turn) [][]Turn {
	var groups [][]Turn
	for _, t := range turns {
		if t.Role == "user" && hasText(t) && len(groups) > 0 {
			groups = append(groups, []Turn{t})
			continue
		}
		if len(groups) == 0 {
			groups = append(groups, nil)
		}
		groups[len(groups)-1] = append(groups[len(groups)-1], t)
	}
	return groups
}

func hasText(t Turn) bool {
	for _, b := range t.Blocks {
		if b.Type == "text" && strings.TrimSpace(b.Text) != "" {
			return true
		}
	}
	return false
}

func renderTurns(turns []Turn) string {
	var b strings.Builder
	for _, t := range turns {
		renderTurn(&b, t)
	}
	return b.String()
}

// elideMiddle keeps the first and last turns of an oversized exchange and
// drops the middle until the rendering fits under exchangeCap.
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
		if b.Len() <= exchangeCap {
			return b.String()
		}
		head, tail = head/2, tail/2
	}
	return fmt.Sprintf("*Exchange too large to render (%d turns).*\n", len(turns))
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
			// Consistent, filterable form: consumers can skip lines starting
			// with "> 🔧 tool:" and fences labeled tool-output.
			fmt.Fprintf(b, "> 🔧 tool: %s `%s`\n\n", blk.Name, summarizeInput(blk.Input))
		case "tool_result":
			text := strings.TrimSpace(blk.Text)
			if text == "" {
				continue
			}
			fmt.Fprintf(b, "````tool-output\n%s\n````\n\n", truncateMiddle(text, toolResultCap))
		}
	}
}

// truncateMiddle keeps the head and tail of s within a total budget of max
// characters, cutting the middle — the start shows what ran, the end shows
// how it came out.
func truncateMiddle(s string, max int) string {
	if len(s) <= max {
		return s
	}
	head := max * 2 / 3
	tail := max - head
	cut := len(s) - max
	return s[:head] + fmt.Sprintf("\n…[%d chars truncated]…\n", cut) + s[len(s)-tail:]
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
