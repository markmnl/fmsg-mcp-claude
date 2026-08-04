// Package session serializes Claude sessions to and from the
// claude-session.json v1 envelope (normative schema in TOOLS.md §7).
package session

const (
	// FormatName and FormatVersion identify the envelope.
	FormatName    = "fmsg-claude-session"
	FormatVersion = 1
	// AttachmentName is the fmsg attachment filename for the envelope;
	// AttachmentNameGz when gzipped (serialized JSON > 1 MiB).
	AttachmentName   = "claude-session.json"
	AttachmentNameGz = "claude-session.json.gz"
	// BodyMarker is the first line of every share's Markdown body — it lands
	// in the webapi's short_text prefix, so transcript-bearing messages are
	// detectable without fetching attachments.
	BodyMarker = "<!-- fmsg-claude-session v1 -->"
	// GzipThreshold is the serialized size above which the envelope is gzipped.
	GzipThreshold = 1 << 20
)

// Envelope is the machine payload attached to every share.
type Envelope struct {
	Format        string     `json:"format"`
	FormatVersion int        `json:"format_version"`
	Provenance    Provenance `json:"provenance"`
	Title         string     `json:"title"`
	Turns         []Turn     `json:"turns"`
	// AttachmentsManifest is reserved for v1.1 context-file bundling.
	AttachmentsManifest []ManifestEntry `json:"attachments_manifest"`
	Truncation          Truncation      `json:"truncation"`
	Redaction           Redaction       `json:"redaction"`
}

// Provenance records where the transcript came from.
type Provenance struct {
	Surface             string   `json:"surface"`  // claude-code | claude-desktop | claude-ai
	Fidelity            string   `json:"fidelity"` // verbatim | model-serialized
	Model               string   `json:"model,omitempty"`
	SessionID           string   `json:"session_id,omitempty"`
	SharedAt            float64  `json:"shared_at"`
	SharerAddress       string   `json:"sharer_address"`
	SharerHumanAddress  string   `json:"sharer_human_address,omitempty"`
	CWD                 string   `json:"cwd,omitempty"`
	Git                 *GitInfo `json:"git,omitempty"`
	BaseFmsgID          *int64   `json:"base_fmsg_id"`
	IncorporatedFmsgIDs []int64  `json:"incorporated_fmsg_ids"`
}

// GitInfo captures repository context at share time.
type GitInfo struct {
	Remote string `json:"remote,omitempty"`
	Branch string `json:"branch,omitempty"`
	Commit string `json:"commit,omitempty"`
}

// Turn is one conversation turn.
type Turn struct {
	I      int     `json:"i"`
	Role   string  `json:"role"` // user | assistant
	Blocks []Block `json:"blocks"`
}

// Block is one content block within a turn. Exactly one payload group is set
// depending on Type (text | tool_use | tool_result).
type Block struct {
	Type string `json:"type"`
	// text
	Text string `json:"text,omitempty"`
	// tool_use
	ID    string `json:"id,omitempty"`
	Name  string `json:"name,omitempty"`
	Input any    `json:"input,omitempty"`
	// tool_result
	ToolUseID string `json:"tool_use_id,omitempty"`
	Truncated bool   `json:"truncated,omitempty"`
}

// ManifestEntry indexes a context file carried as an extra fmsg attachment (v1.1).
type ManifestEntry struct {
	FmsgFilename string `json:"fmsg_filename"`
	Kind         string `json:"kind"`
	OriginalPath string `json:"original_path"`
	SHA256       string `json:"sha256"`
	Bytes        int64  `json:"bytes"`
}

// Truncation records whether turns were dropped from the envelope (never the default).
type Truncation struct {
	Truncated         bool    `json:"truncated"`
	Strategy          *string `json:"strategy"`
	OriginalTurnCount int     `json:"original_turn_count"`
}

// Redaction records the secret-scrub pass.
type Redaction struct {
	Applied     bool     `json:"applied"`
	PatternsHit []string `json:"patterns_hit"`
}
