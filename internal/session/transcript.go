// Package session parses Claude session transcripts and renders them as
// Markdown — the fmsg message body is the sole carrier of the shared session,
// readable by any fmsg client and loadable by any agent.
package session

// Transcript is a parsed session ready to share.
type Transcript struct {
	Title         string
	Surface       string // claude-code | claude-ai
	Fidelity      string // verbatim | model-serialized
	Model         string
	SessionID     string
	SharerAddress string
	SharedAt      float64
	Turns         []Turn
}

// Turn is one conversation turn.
type Turn struct {
	Role   string // user | assistant
	Blocks []Block
}

// Block is one content block within a turn (text | tool_use | tool_result).
type Block struct {
	Type      string
	Text      string // text and tool_result payloads
	Name      string // tool_use
	Input     any    // tool_use
	ToolUseID string // tool_result
}
