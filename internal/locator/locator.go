// Package locator finds the Claude Code session transcript for the calling
// session (ARCHITECTURE.md §4). Precedence: the caller-supplied session id,
// then CLAUDE_CODE_SESSION_ID from the environment Claude Code spawns the
// stdio server with, then the SessionStart hook's pointer file, and only as a
// last resort the most-recently-modified JSONL under the project slug.
//
// The env var is preferred over the pointer because the pointer is per
// *project*, not per session: with several sessions open on one project
// directory (cmux, split panes, `claude` in two terminals) every SessionStart
// overwrites it, so the pointer — and the mtime fallback behind it — can name
// another agent's session. Sharing must never carry a session other than the
// caller's, so the mtime fallback now refuses to guess between concurrently
// active transcripts rather than picking the wrong one.
package locator

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
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

// SessionIDEnv is the environment variable Claude Code exports into the
// processes it spawns, naming the session that owns them. For a stdio MCP
// server that is definitive self-identification: no pointer file, no race.
const SessionIDEnv = "CLAUDE_CODE_SESSION_ID"

// ambiguityWindow is how close behind the newest transcript another one may
// have been written before the mtime fallback calls the choice ambiguous.
// Concurrent sessions on one project all write while they work; a single
// active session leaves its stale siblings far behind.
const ambiguityWindow = 2 * time.Minute

// Locate returns the transcript path and session id for projectDir, plus the
// method used ("session-id", "env", "hook" or "mtime"). sessionID, when
// supplied by the caller, wins outright.
func Locate(home, projectDir, sessionID string) (path, sid, method string, err error) {
	if sessionID != "" {
		if p, ok := findBySessionID(home, projectDir, sessionID); ok {
			return p, sessionID, "session-id", nil
		}
		return "", "", "", fmt.Errorf("no transcript found for session %s", sessionID)
	}
	if envID := os.Getenv(SessionIDEnv); envID != "" {
		if p, ok := findBySessionID(home, projectDir, envID); ok {
			return p, envID, "env", nil
		}
		// Claude Code named a session whose transcript we cannot see. Falling
		// back would risk sharing a different session, so say so instead.
		return "", "", "", fmt.Errorf("%s=%s but no transcript for it under %s",
			SessionIDEnv, envID, filepath.Join(home, ".claude", "projects"))
	}

	if p, perr := readPointer(PointerPath(home, projectDir)); perr == nil {
		if _, serr := os.Stat(p.TranscriptPath); serr == nil {
			return p.TranscriptPath, p.SessionID, "hook", nil
		}
	}

	dir := filepath.Join(home, ".claude", "projects", Slug(projectDir))
	entries, derr := os.ReadDir(dir)
	if derr != nil {
		return "", "", "", fmt.Errorf("no session pointer and no transcripts under %s: %w", dir, derr)
	}
	var newest, runnerUp string
	var newestMod, runnerUpMod int64
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".jsonl") {
			continue
		}
		info, ierr := e.Info()
		if ierr != nil {
			continue
		}
		m := info.ModTime().UnixNano()
		switch {
		case newest == "" || m > newestMod:
			newest, newestMod, runnerUp, runnerUpMod = e.Name(), m, newest, newestMod
		case m > runnerUpMod:
			runnerUp, runnerUpMod = e.Name(), m
		}
	}
	if newest == "" {
		return "", "", "", fmt.Errorf("no transcripts found under %s", dir)
	}
	if runnerUp != "" && newestMod-runnerUpMod < int64(ambiguityWindow) {
		return "", "", "", fmt.Errorf(
			"cannot tell which session is calling: %s and %s were both written in the last %s. "+
				"Two or more Claude sessions are open on this project — pass session_id explicitly, "+
				"or install the SessionStart hook (see README) so the caller identifies itself",
			strings.TrimSuffix(newest, ".jsonl"), strings.TrimSuffix(runnerUp, ".jsonl"), ambiguityWindow)
	}
	return filepath.Join(dir, newest), strings.TrimSuffix(newest, ".jsonl"), "mtime", nil
}

// findBySessionID resolves a session id to its transcript: the project slug
// directory first, then any other project directory (a session started in a
// different cwd — worktrees, `cd` mid-session — still belongs to its caller).
func findBySessionID(home, projectDir, sessionID string) (string, bool) {
	root := filepath.Join(home, ".claude", "projects")
	candidate := filepath.Join(root, Slug(projectDir), sessionID+".jsonl")
	if _, err := os.Stat(candidate); err == nil {
		return candidate, true
	}
	entries, err := os.ReadDir(root)
	if err != nil {
		return "", false
	}
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		p := filepath.Join(root, e.Name(), sessionID+".jsonl")
		if _, serr := os.Stat(p); serr == nil {
			return p, true
		}
	}
	return "", false
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
