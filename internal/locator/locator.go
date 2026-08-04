// Package locator finds the Claude Code session transcript for the calling
// session (ARCHITECTURE.md §4): primary is the SessionStart hook's pointer
// file; fallback is the most-recently-modified JSONL under the project slug.
package locator

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// Pointer is what the SessionStart hook writes.
type Pointer struct {
	SessionID      string  `json:"session_id"`
	TranscriptPath string  `json:"transcript_path"`
	TS             float64 `json:"ts"`
}

// Slug converts a project directory path to Claude Code's project-directory
// slug (every non-alphanumeric byte becomes '-').
func Slug(dir string) string {
	var b strings.Builder
	for _, r := range dir {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') {
			b.WriteRune(r)
		} else {
			b.WriteByte('-')
		}
	}
	return b.String()
}

// PointerPath is where the hook drops the pointer for a project.
func PointerPath(home, projectDir string) string {
	return filepath.Join(home, ".claude", "fmsg-mcp", "current-session-"+Slug(projectDir)+".json")
}

// Locate returns the transcript path and session id for projectDir, plus the
// method used ("hook" or "mtime"). sessionID, when supplied by the caller,
// acts as a tie-breaker for the fallback.
func Locate(home, projectDir, sessionID string) (path, sid, method string, err error) {
	if p, perr := readPointer(PointerPath(home, projectDir)); perr == nil {
		if _, serr := os.Stat(p.TranscriptPath); serr == nil {
			if sessionID == "" || p.SessionID == sessionID {
				return p.TranscriptPath, p.SessionID, "hook", nil
			}
		}
	}

	dir := filepath.Join(home, ".claude", "projects", Slug(projectDir))
	if sessionID != "" {
		candidate := filepath.Join(dir, sessionID+".jsonl")
		if _, serr := os.Stat(candidate); serr == nil {
			return candidate, sessionID, "session-id", nil
		}
	}
	entries, derr := os.ReadDir(dir)
	if derr != nil {
		return "", "", "", fmt.Errorf("no session pointer and no transcripts under %s: %w", dir, derr)
	}
	var newest string
	var newestMod int64
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".jsonl") {
			continue
		}
		info, ierr := e.Info()
		if ierr != nil {
			continue
		}
		if m := info.ModTime().UnixNano(); newest == "" || m > newestMod {
			newest, newestMod = e.Name(), m
		}
	}
	if newest == "" {
		return "", "", "", fmt.Errorf("no transcripts found under %s", dir)
	}
	return filepath.Join(dir, newest), strings.TrimSuffix(newest, ".jsonl"), "mtime", nil
}

func readPointer(path string) (*Pointer, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var p Pointer
	if err := json.Unmarshal(raw, &p); err != nil {
		return nil, err
	}
	if p.TranscriptPath == "" {
		return nil, fmt.Errorf("pointer file %s has no transcript_path", path)
	}
	return &p, nil
}
