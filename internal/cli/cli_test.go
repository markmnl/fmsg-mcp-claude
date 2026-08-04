package cli

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

// stubCLI writes a shell script that plays fmsg-cli: it logs its argv and
// emits canned stdout/stderr with a fixed exit code.
func stubCLI(t *testing.T, stdout, stderr string, exitCode int) (*Runner, string) {
	t.Helper()
	dir := t.TempDir()
	bin := filepath.Join(dir, "fmsg")
	argvLog := filepath.Join(dir, "argv")
	script := "#!/bin/sh\n" +
		"echo \"$@\" >> " + argvLog + "\n" +
		"printf '%s' " + shellQuote(stdout) + "\n" +
		"printf '%s' " + shellQuote(stderr) + " >&2\n" +
		"exit " + itoa(exitCode) + "\n"
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return &Runner{Bin: bin, Dir: dir, Env: []string{"PATH=/usr/bin:/bin"}}, argvLog
}

func shellQuote(s string) string { return "'" + s + "'" }
func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	return string(rune('0' + n))
}

func TestGetDecodesMessage(t *testing.T) {
	r, argvLog := stubCLI(t, `{"pid": 41, "from": "@alice_claude@example.com", "to": ["@bob@example.com"],
		"topic": "Fix auth", "type": "text/markdown", "short_text": "<!-- fmsg-claude-session v1 -->",
		"attachments": [{"filename": "claude-session.json", "size": 2048}],
		"add_to": [{"batch_id": 7, "add_to_from": "@alice_claude@example.com", "to": ["@bob_claude@example.com"], "time": 1.5}]}`, "", 0)
	m, err := r.Get(context.Background(), 42)
	if err != nil {
		t.Fatal(err)
	}
	if m.PID == nil || *m.PID != 41 {
		t.Fatalf("pid: got %v", m.PID)
	}
	if m.Attachments[0].Filename != "claude-session.json" {
		t.Fatalf("attachment: %+v", m.Attachments)
	}
	got := m.Participants()
	want := []string{"@alice_claude@example.com", "@bob@example.com", "@bob_claude@example.com"}
	if len(got) != len(want) {
		t.Fatalf("participants: got %v want %v", got, want)
	}
	argv, _ := os.ReadFile(argvLog)
	if string(argv) != "--json get 42\n" {
		t.Fatalf("argv: %q — --json must precede the subcommand", argv)
	}
}

func TestErrorClassification(t *testing.T) {
	cases := []struct {
		stderr string
		code   string
		status int
	}{
		{"Error: API error 404: message not found", "api_error", 404},
		{"Error: missing credentials; run fmsg login with an API key", "not_logged_in", 0},
		{"panic: something odd", "cli_error", 0},
	}
	for _, c := range cases {
		r, _ := stubCLI(t, "", c.stderr, 1)
		_, err := r.Get(context.Background(), 1)
		var cerr *Error
		if !errors.As(err, &cerr) {
			t.Fatalf("expected *Error, got %v", err)
		}
		if cerr.Code != c.code || cerr.Status != c.status {
			t.Fatalf("stderr %q: got %s/%d, want %s/%d", c.stderr, cerr.Code, cerr.Status, c.code, c.status)
		}
	}
}

func TestUpdateRecipientsArgOrder(t *testing.T) {
	r, argvLog := stubCLI(t, `{"id": 5}`, "", 0)
	if err := r.UpdateRecipients(context.Background(), 5, []string{"@x@example.com", "@y@example.com"}); err != nil {
		t.Fatal(err)
	}
	argv, _ := os.ReadFile(argvLog)
	want := "--json update --to @x@example.com,@y@example.com 5\n"
	if string(argv) != want {
		t.Fatalf("argv: %q want %q", argv, want)
	}
}

func TestDraftCreateArgOrder(t *testing.T) {
	r, argvLog := stubCLI(t, `{"id": 9}`, "", 0)
	id, err := r.DraftCreate(context.Background(), "@bob@example.com", "/tmp/body.md", "My topic", 0)
	if err != nil || id != 9 {
		t.Fatalf("id=%d err=%v", id, err)
	}
	argv, _ := os.ReadFile(argvLog)
	want := "--json draft create --topic My topic @bob@example.com /tmp/body.md\n"
	if string(argv) != want {
		t.Fatalf("argv: %q want %q", argv, want)
	}
}
