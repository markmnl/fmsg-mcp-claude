// Package session parses Claude session transcripts and renders them as
// Markdown — the fmsg message body is the sole carrier of the shared session,
// readable by any fmsg client and loadable by any agent.
package session

// Transcript is a parsed session ready to share.
type Transcript struct {
	Title     string
	Surface   string // claude-code | claude-ai
	Fidelity  string // verbatim | model-serialized
	Model     string
	SessionID string
	// SessionSource is how the session was identified (env | session-id |
	// hook | mtime) — reported in the preview so the user can catch a share
	// resolved by the weakest signal, which is the one that can name another
	// session when several run on one project.
	SessionSource string
	SharerAddress string
	SharedAt      float64
	Turns         []Turn
}

// Turn is one conversation turn.
type Turn struct {
	Role   string // user | assistant
	Blocks []Block
}

// Block is one content block within a turn. Only text survives parsing —
// tool activity and thinking are excluded at the parse layer.
type Block struct {
	Type string
	Text string
}
