// Package sharestate remembers what has already been shared of a Claude
// session, so re-sharing sends only the new exchanges pid-chained onto the
// existing fmsg thread instead of a whole new thread.
package sharestate

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// CurrentFormat identifies the rendering the stored hashes were computed
// over. Bump it whenever RenderExchanges output changes shape (format 2
// dropped tool calls/output from renderings): stored hashes from an older
// format can never prefix-match the new rendering, so the caller starts a
// new thread instead of comparing against a known-stale prefix.
const CurrentFormat = 2

// KindSummary is the State.Kind for summary-share threads ("" = transcript).
const KindSummary = "summary"

// State records one session's shared-thread progress.
type State struct {
	SessionID      string   `json:"session_id"`
	Kind           string   `json:"kind,omitempty"` // "" = transcript share; "summary" = summary thread
	FormatVersion  int      `json:"format_version"`
	ThreadRoot     int64    `json:"thread_root"`
	LastFmsgID     int64    `json:"last_fmsg_id"`
	Recipients     []string `json:"recipients"`
	ExchangeHashes []string `json:"exchange_hashes"` // hash of every exchange body sent, in order; empty for summary threads
	SummaryCount   int      `json:"summary_count,omitempty"`
	UpdatedAt      float64  `json:"updated_at"`
}

func dir() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".claude", "fmsg-mcp", "shares"), nil
}

func path(sessionID, kind string) (string, error) {
	d, err := dir()
	if err != nil {
		return "", err
	}
	// Session IDs are UUIDs, but sanitize defensively for the filesystem.
	safe := strings.Map(func(r rune) rune {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '-' {
			return r
		}
		return '_'
	}, sessionID)
	// Transcript shares keep the historical <id>.json name; other kinds get
	// their own file so a session can hold both threads independently.
	if kind != "" {
		safe += "." + kind
	}
	return filepath.Join(d, safe+".json"), nil
}

// Load returns the recorded state for a session and kind ("" = transcript
// share, KindSummary = summary thread), or nil when none exists.
func Load(sessionID, kind string) (*State, error) {
	if sessionID == "" {
		return nil, nil
	}
	p, err := path(sessionID, kind)
	if err != nil {
		return nil, err
	}
	raw, err := os.ReadFile(p)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var s State
	if err := json.Unmarshal(raw, &s); err != nil {
		return nil, fmt.Errorf("corrupt share state %s: %w", p, err)
	}
	return &s, nil
}

// Save persists the state (atomic write), stamping the current format.
func Save(s *State) error {
	p, err := path(s.SessionID, s.Kind)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		return err
	}
	s.FormatVersion = CurrentFormat
	s.UpdatedAt = float64(time.Now().Unix())
	raw, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return err
	}
	tmp := p + ".tmp"
	if err := os.WriteFile(tmp, raw, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, p)
}

// HashBody identifies an exchange body for prefix matching.
func HashBody(body string) string {
	sum := sha256.Sum256([]byte(body))
	return hex.EncodeToString(sum[:8])
}

// HashBodies hashes every body in order.
func HashBodies(bodies []string) []string {
	out := make([]string, len(bodies))
	for i, b := range bodies {
		out[i] = HashBody(b)
	}
	return out
}

// Delta reports whether the current bodies extend the previously shared
// prefix, and if so returns the new bodies to send. ok is false when the
// session content no longer matches what was shared (diverged/edited) — the
// caller should fall back to a full share on a new thread.
func Delta(prevHashes []string, bodies []string) (delta []string, ok bool) {
	if len(prevHashes) > len(bodies) {
		return nil, false
	}
	hashes := HashBodies(bodies[:len(prevHashes)])
	for i := range prevHashes {
		if hashes[i] != prevHashes[i] {
			return nil, false
		}
	}
	return bodies[len(prevHashes):], true
}

// SameRecipients reports whether two recipient sets are equal
// (case-insensitive, order-insensitive). Auto-continuation requires the
// exact same audience: a new recipient cannot read messages they were not
// part of (and their host will not even hold the parents), and a narrower
// set would fork the thread's bookkeeping — either difference falls back to
// a full share on a new thread.
func SameRecipients(prev, next []string) bool {
	norm := func(in []string) map[string]bool {
		m := make(map[string]bool, len(in))
		for _, a := range in {
			m[strings.ToLower(a)] = true
		}
		return m
	}
	a, b := norm(prev), norm(next)
	if len(a) != len(b) {
		return false
	}
	for k := range a {
		if !b[k] {
			return false
		}
	}
	return true
}
