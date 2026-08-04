package locator

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestSlug(t *testing.T) {
	got := Slug("/home/markmnl/github.com/fmsg/fmsg-mcp-claude")
	want := "-home-markmnl-github-com-fmsg-fmsg-mcp-claude"
	if got != want {
		t.Fatalf("slug: got %q want %q", got, want)
	}
}

func TestLocatePrefersPointer(t *testing.T) {
	home := t.TempDir()
	project := "/home/alice/proj"

	// Fallback candidate under the project slug dir.
	projDir := filepath.Join(home, ".claude", "projects", Slug(project))
	if err := os.MkdirAll(projDir, 0o755); err != nil {
		t.Fatal(err)
	}
	fallback := filepath.Join(projDir, "fallback-session.jsonl")
	if err := os.WriteFile(fallback, []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}

	// Hook pointer targeting a different transcript.
	target := filepath.Join(projDir, "hooked-session.jsonl")
	if err := os.WriteFile(target, []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	ptrDir := filepath.Dir(PointerPath(home, project))
	if err := os.MkdirAll(ptrDir, 0o755); err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(Pointer{SessionID: "hooked-session", TranscriptPath: target, TS: float64(time.Now().Unix())})
	if err := os.WriteFile(PointerPath(home, project), raw, 0o600); err != nil {
		t.Fatal(err)
	}

	path, sid, method, err := Locate(home, project, "")
	if err != nil {
		t.Fatal(err)
	}
	if method != "hook" || path != target || sid != "hooked-session" {
		t.Fatalf("got %s %s %s", path, sid, method)
	}

	// Without the pointer, the newest JSONL wins.
	if err := os.Remove(PointerPath(home, project)); err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	if err := os.Chtimes(fallback, now, now.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	path, _, method, err = Locate(home, project, "")
	if err != nil {
		t.Fatal(err)
	}
	if method != "mtime" || path != fallback {
		t.Fatalf("fallback: got %s %s", path, method)
	}
}
