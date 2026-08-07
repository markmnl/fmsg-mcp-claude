package session

import (
	"fmt"
	"strings"
	"time"
)

// SummaryMeta carries the provenance for a rendered summary body.
type SummaryMeta struct {
	Title         string
	SharerAddress string
	Surface       string // claude-code | claude-ai
	SharedAt      float64
	TurnCount     int  // 0 = unknown (session file not locatable)
	FollowUp      bool // a later summary replying into the existing summary thread
}

// RenderSummary renders the single Markdown body for a summary share: the
// root message carries the title and full provenance line; follow-ups get a
// one-line header only, since the fmsg topic already lives on the thread
// root. The summary text itself is model-authored and carried verbatim.
func RenderSummary(m SummaryMeta, summary string) string {
	var b strings.Builder
	date := time.Unix(int64(m.SharedAt), 0).UTC().Format("2006-01-02 15:04 UTC")
	turns := ""
	if m.TurnCount > 0 {
		turns = fmt.Sprintf(" · %d turns summarised", m.TurnCount)
	}
	if m.FollowUp {
		fmt.Fprintf(&b, "*Updated summary · %s%s*\n\n---\n\n", date, turns)
	} else {
		fmt.Fprintf(&b, "# %s\n\n", orDefault(m.Title, "Claude session summary"))
		fmt.Fprintf(&b, "*Summary of a Claude session shared by %s · %s · %s%s*\n\n---\n\n",
			m.SharerAddress, m.Surface, date, turns)
	}
	b.WriteString(strings.TrimSpace(summary))
	b.WriteString("\n")
	return b.String()
}
