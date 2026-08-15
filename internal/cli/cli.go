// Package cli shells out to fmsg-cli with --json and decodes its output.
// It is the only package that touches fmsg; per ARCHITECTURE.md the server
// never speaks HTTP to fmsg-webapi directly.
package cli

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"time"
)

// RecipientDelivery mirrors fmsg-cli's JSON shape for per-recipient delivery state.
type RecipientDelivery struct {
	Addr          string  `json:"addr"`
	TimeDelivered *string `json:"time_delivered"`
	ResponseCode  *int    `json:"response_code"`
}

// AddToBatch is one add-to batch on a message.
type AddToBatch struct {
	BatchID    int64               `json:"batch_id"`
	AddToFrom  string              `json:"add_to_from"`
	To         []string            `json:"to"`
	ToDelivery []RecipientDelivery `json:"to_delivery"`
	Time       float64             `json:"time"`
}

// Attachment is an attachment entry on a message.
type Attachment struct {
	Size     int64  `json:"size"`
	Filename string `json:"filename"`
}

// Message mirrors `fmsg --json get` output.
type Message struct {
	Version     int                 `json:"version"`
	HasPID      bool                `json:"has_pid"`
	HasAddTo    bool                `json:"has_add_to"`
	Important   bool                `json:"important"`
	NoReply     bool                `json:"no_reply"`
	Deflate     bool                `json:"deflate"`
	PID         *int64              `json:"pid"`
	From        string              `json:"from"`
	To          []string            `json:"to"`
	ToDelivery  []RecipientDelivery `json:"to_delivery"`
	AddTo       []AddToBatch        `json:"add_to"`
	Time        *float64            `json:"time"`
	Topic       string              `json:"topic"`
	Type        string              `json:"type"`
	Size        int                 `json:"size"`
	ShortText   *string             `json:"short_text"`
	Read        bool                `json:"read"`
	TimeRead    *float64            `json:"time_read"`
	Attachments []Attachment        `json:"attachments"`
}

// ListItem mirrors one element of `fmsg --json list` output.
type ListItem struct {
	ID int64 `json:"id"`
	Message
}

// Participants returns every address that is a participant of the message
// (from ∪ to ∪ each add-to batch's adder and recipients), deduplicated
// case-insensitively, preserving first-seen casing.
func (m *Message) Participants() []string {
	var out []string
	seen := map[string]bool{}
	add := func(addr string) {
		k := strings.ToLower(addr)
		if addr != "" && !seen[k] {
			seen[k] = true
			out = append(out, addr)
		}
	}
	add(m.From)
	for _, a := range m.To {
		add(a)
	}
	for _, b := range m.AddTo {
		add(b.AddToFrom)
		for _, a := range b.To {
			add(a)
		}
	}
	return out
}

// Error is a classified failure from an fmsg-cli invocation.
type Error struct {
	Code   string // cli_error | not_logged_in | api_error
	Status int    // HTTP status when Code == api_error, else 0
	Detail string // stderr text
}

func (e *Error) Error() string {
	if e.Status != 0 {
		return fmt.Sprintf("%s (%d): %s", e.Code, e.Status, e.Detail)
	}
	return fmt.Sprintf("%s: %s", e.Code, e.Detail)
}

var apiErrRe = regexp.MustCompile(`API error (\d+):\s*(.*)`)

// Runner executes fmsg-cli commands with a pinned environment.
type Runner struct {
	Bin     string        // fmsg binary path (default "fmsg")
	Dir     string        // working directory for the CLI (never the project dir)
	Env     []string      // extra env (FMSG_API_URL, FMSG_API_KEY); appended to a minimal base
	Timeout time.Duration // per-invocation timeout (default 60s)
}

// NewRunnerFromEnv builds a Runner from the server's own environment.
// Precedence for the CLI binary: explicit FMSG_CLI, then the embedded CLI
// (release builds), then `fmsg` on PATH.
func NewRunnerFromEnv(version string) *Runner {
	bin := os.Getenv("FMSG_CLI")
	if bin == "" {
		if extracted, err := extractEmbeddedCLI(version); err == nil && extracted != "" {
			bin = extracted
		} else {
			bin = "fmsg"
		}
	}
	// Besides the fmsg settings, forward what the CLI needs to find its
	// config (auth.json) and cache (the JWT exchanged for FMSG_API_KEY —
	// without XDG_CACHE_HOME/LocalAppData the cache is silently disabled and
	// every invocation re-exchanges the key).
	var env []string
	for _, k := range []string{"FMSG_API_URL", "FMSG_API_KEY", "FMSG_NO_TOKEN_CACHE",
		"HOME", "XDG_CONFIG_HOME", "XDG_CACHE_HOME", "PATH",
		"USERPROFILE", "APPDATA", "LOCALAPPDATA", "SYSTEMROOT", "TEMP", "TMP"} {
		if v := os.Getenv(k); v != "" {
			env = append(env, k+"="+v)
		}
	}
	return &Runner{Bin: bin, Dir: os.TempDir(), Env: env, Timeout: 60 * time.Second}
}

// extractEmbeddedCLI writes the embedded fmsg binary (if this build carries
// one) to the user cache dir, keyed by server version, and returns its path.
// Returns "" when this build has no embedded CLI.
func extractEmbeddedCLI(version string) (string, error) {
	if len(embeddedCLI) == 0 {
		return "", nil
	}
	cacheDir, err := os.UserCacheDir()
	if err != nil {
		return "", err
	}
	dir := filepath.Join(cacheDir, "fmsg-mcp")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	name := "fmsg-" + version
	if runtime.GOOS == "windows" {
		name += ".exe"
	}
	path := filepath.Join(dir, name)
	if info, serr := os.Stat(path); serr == nil && info.Size() == int64(len(embeddedCLI)) {
		return path, nil
	}
	// Write via temp + rename so a concurrent server start never sees a
	// half-written binary.
	tmp, err := os.CreateTemp(dir, name+".tmp-*")
	if err != nil {
		return "", err
	}
	if _, err := tmp.Write(embeddedCLI); err != nil {
		tmp.Close()
		os.Remove(tmp.Name())
		return "", err
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmp.Name())
		return "", err
	}
	if err := os.Chmod(tmp.Name(), 0o755); err != nil {
		os.Remove(tmp.Name())
		return "", err
	}
	if err := os.Rename(tmp.Name(), path); err != nil {
		os.Remove(tmp.Name())
		return "", err
	}
	return path, nil
}

// run invokes the CLI with --json prepended (flags must precede any negative
// message index) and returns stdout on success.
func (r *Runner) run(ctx context.Context, args ...string) ([]byte, error) {
	timeout := r.Timeout
	if timeout == 0 {
		timeout = 60 * time.Second
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	cmd := exec.CommandContext(ctx, r.Bin, append([]string{"--json"}, args...)...)
	cmd.Dir = r.Dir
	cmd.Env = r.Env
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	if err := cmd.Run(); err != nil {
		detail := strings.TrimSpace(stderr.String())
		if detail == "" {
			detail = err.Error()
		}
		return nil, classify(detail)
	}
	return stdout.Bytes(), nil
}

func classify(stderr string) *Error {
	if strings.Contains(stderr, "missing credentials") ||
		strings.Contains(stderr, "run fmsg login") ||
		strings.Contains(stderr, "stored credentials are for") {
		return &Error{Code: "not_logged_in", Detail: stderr}
	}
	if m := apiErrRe.FindStringSubmatch(stderr); m != nil {
		status, _ := strconv.Atoi(m[1])
		return &Error{Code: "api_error", Status: status, Detail: stderr}
	}
	return &Error{Code: "cli_error", Detail: stderr}
}

// decode runs the command and unmarshals the single JSON value it emits.
func decode[T any](r *Runner, ctx context.Context, args ...string) (T, error) {
	var v T
	out, err := r.run(ctx, args...)
	if err != nil {
		return v, err
	}
	if err := json.Unmarshal(bytes.TrimSpace(out), &v); err != nil {
		return v, &Error{Code: "cli_error", Detail: fmt.Sprintf("decoding %q output: %v", args[0], err)}
	}
	return v, nil
}

// CheckJSONSupport verifies the pinned CLI exists and supports --json.
func (r *Runner) CheckJSONSupport(ctx context.Context) error {
	cmd := exec.CommandContext(ctx, r.Bin, "--help")
	cmd.Env = r.Env
	out, err := cmd.CombinedOutput()
	if err != nil {
		return &Error{Code: "cli_error", Detail: fmt.Sprintf("fmsg-cli not runnable at %q: %v", r.Bin, err)}
	}
	if !strings.Contains(string(out), "--json") {
		return &Error{Code: "cli_error", Detail: "installed fmsg-cli does not support --json; upgrade to a release including it"}
	}
	return nil
}

// WhoamiInfo mirrors `fmsg --json whoami` output.
type WhoamiInfo struct {
	Address        string `json:"address"`
	APIURL         string `json:"api_url"`
	TokenExpiresAt string `json:"token_expires_at"`
}

// Whoami reports the authenticated identity.
func (r *Runner) Whoami(ctx context.Context) (*WhoamiInfo, error) {
	w, err := decode[WhoamiInfo](r, ctx, "whoami")
	if err != nil {
		return nil, err
	}
	return &w, nil
}

// Get fetches a message by id (negative index allowed).
func (r *Runner) Get(ctx context.Context, id int64) (*Message, error) {
	m, err := decode[Message](r, ctx, "get", strconv.FormatInt(id, 10))
	if err != nil {
		return nil, err
	}
	return &m, nil
}

// List returns inbox items.
func (r *Runner) List(ctx context.Context, limit int) ([]ListItem, error) {
	return decode[[]ListItem](r, ctx, "list", "--limit", strconv.Itoa(limit))
}

// ListSent returns messages authored by the authenticated identity
// (including drafts).
func (r *Runner) ListSent(ctx context.Context, limit int) ([]ListItem, error) {
	return decode[[]ListItem](r, ctx, "sent", "--limit", strconv.Itoa(limit))
}

type idResult struct {
	ID int64 `json:"id"`
}

// DraftCreate creates a draft to one recipient with the body read from bodyPath.
// Exactly one of topic (root) or pid (reply) may be set.
func (r *Runner) DraftCreate(ctx context.Context, recipient, bodyPath, topic string, pid int64) (int64, error) {
	args := []string{"draft", "create"}
	if topic != "" {
		args = append(args, "--topic", topic)
	}
	if pid != 0 {
		args = append(args, "--pid", strconv.FormatInt(pid, 10))
	}
	args = append(args, recipient, bodyPath)
	res, err := decode[idResult](r, ctx, args...)
	return res.ID, err
}

// UpdateFull rewrites a draft with every field restated. fmsg-webapi's PUT is
// full-replacement while fmsg-cli's update sends only provided fields, so a
// partial update silently wipes recipients, pid, and topic (upstream bug —
// OPEN_QUESTIONS). Restating everything makes replacement semantics harmless.
// Exactly one of topic (root) or pid (reply) may be set.
func (r *Runner) UpdateFull(ctx context.Context, id int64, bodyPath string, to []string, mime, topic string, pid int64) error {
	args := []string{"update", "--type", mime, "--to", strings.Join(to, ",")}
	if topic != "" {
		args = append(args, "--topic", topic)
	}
	if pid != 0 {
		args = append(args, "--pid", strconv.FormatInt(pid, 10))
	}
	args = append(args, strconv.FormatInt(id, 10), bodyPath)
	_, err := decode[idResult](r, ctx, args...)
	return err
}

// DraftSend sends a draft.
func (r *Runner) DraftSend(ctx context.Context, id int64) error {
	_, err := decode[struct {
		ID   int64   `json:"id"`
		Time float64 `json:"time"`
	}](r, ctx, "draft", "send", strconv.FormatInt(id, 10))
	return err
}

// Del deletes a draft (cleanup after a failed share sequence).
func (r *Runner) Del(ctx context.Context, id int64) error {
	_, err := decode[idResult](r, ctx, "del", strconv.FormatInt(id, 10))
	return err
}

// GetData returns a message's raw body bytes (stdout mode is unaffected by --json).
func (r *Runner) GetData(ctx context.Context, id int64) ([]byte, error) {
	return r.run(ctx, "get-data", strconv.FormatInt(id, 10))
}

// WatchEvent is one NDJSON line from `fmsg --json watch`: the server's
// {type, data} envelope, or the CLI's own {"type":"ready"} marker emitted
// after every (re)connect — a signal that events may have been missed while
// the socket was down, so callers should re-list to catch up.
type WatchEvent struct {
	Type string          `json:"type"`
	Data json.RawMessage `json:"data"`
}

// EventReady is the CLI's connect marker; the rest are server event types.
const (
	EventReady           = "ready"
	EventNewMsg          = "new_msg"
	EventDelivered       = "delivered"
	EventRecipientsAdded = "recipients_added"
)

// Item decodes the event's message payload (list-item shape, with id).
func (e WatchEvent) Item() (*ListItem, error) {
	var it ListItem
	if err := json.Unmarshal(e.Data, &it); err != nil {
		return nil, fmt.Errorf("decoding %s event: %w", e.Type, err)
	}
	return &it, nil
}

// ErrNoWatch reports a CLI build without the watch command; callers fall
// back to polling `list`.
var ErrNoWatch = errors.New("fmsg-cli lacks the watch command")

// Watch starts `fmsg --json watch --events <events>` and streams its events
// on the returned channel until ctx is cancelled (which kills the process)
// or the CLI exits, after which the channel is closed. The subprocess is not
// bound by Runner.Timeout. Returns ErrNoWatch when the CLI predates watch.
func (r *Runner) Watch(ctx context.Context, events ...string) (<-chan WatchEvent, error) {
	args := []string{"--json", "watch"}
	if len(events) > 0 {
		args = append(args, "--events", strings.Join(events, ","))
	}
	cmd := exec.CommandContext(ctx, r.Bin, args...)
	cmd.Dir = r.Dir
	cmd.Env = r.Env
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	// Don't let a grandchild holding stderr open stall Wait after the CLI
	// itself has exited (or been killed on ctx cancel).
	cmd.WaitDelay = time.Second
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	if err := cmd.Start(); err != nil {
		return nil, &Error{Code: "cli_error", Detail: err.Error()}
	}

	// Killing the process on ctx cancel is not enough if it left a child
	// holding the pipe; closing our read end unblocks the scanner regardless.
	stopClose := context.AfterFunc(ctx, func() { stdout.Close() })

	out := make(chan WatchEvent, 16)
	first := make(chan error, 1) // outcome of the first line: ready, or a startup failure
	go func() {
		defer close(out)
		defer stopClose()
		sc := bufio.NewScanner(stdout)
		sc.Buffer(make([]byte, 0, 64<<10), 16<<20)
		got := false
		for sc.Scan() {
			line := bytes.TrimSpace(sc.Bytes())
			if len(line) == 0 {
				continue
			}
			var ev WatchEvent
			if err := json.Unmarshal(line, &ev); err != nil {
				continue // tolerate stray output
			}
			if !got {
				got = true
				first <- nil
			}
			select {
			case out <- ev:
			case <-ctx.Done():
				cmd.Wait()
				return
			}
		}
		werr := cmd.Wait()
		if !got {
			detail := strings.TrimSpace(stderr.String())
			switch {
			case strings.Contains(detail, `unknown command "watch"`):
				first <- ErrNoWatch
			case werr != nil && detail != "":
				first <- classify(detail)
			case werr != nil:
				first <- &Error{Code: "cli_error", Detail: werr.Error()}
			default:
				first <- &Error{Code: "cli_error", Detail: "watch exited before connecting"}
			}
		}
	}()

	select {
	case err := <-first:
		if err != nil {
			return nil, err
		}
		return out, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}
