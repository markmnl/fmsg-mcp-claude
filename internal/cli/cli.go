// Package cli shells out to fmsg-cli with --json and decodes its output.
// It is the only package that touches fmsg; per ARCHITECTURE.md the server
// never speaks HTTP to fmsg-webapi directly.
package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"regexp"
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
func NewRunnerFromEnv() *Runner {
	bin := os.Getenv("FMSG_CLI")
	if bin == "" {
		bin = "fmsg"
	}
	var env []string
	for _, k := range []string{"FMSG_API_URL", "FMSG_API_KEY", "HOME", "XDG_CONFIG_HOME", "PATH"} {
		if v := os.Getenv(k); v != "" {
			env = append(env, k+"="+v)
		}
	}
	return &Runner{Bin: bin, Dir: os.TempDir(), Env: env, Timeout: 60 * time.Second}
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

// SetType sets a draft's MIME type.
func (r *Runner) SetType(ctx context.Context, id int64, mime string) error {
	_, err := decode[idResult](r, ctx, "update", "--type", mime, strconv.FormatInt(id, 10))
	return err
}

// SetBodyAndType replaces a draft's body from bodyPath and sets its MIME type
// in one update call.
func (r *Runner) SetBodyAndType(ctx context.Context, id int64, bodyPath, mime string) error {
	_, err := decode[idResult](r, ctx, "update", "--type", mime, strconv.FormatInt(id, 10), bodyPath)
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

// UpdateRecipients replaces a draft's recipient list (`update --to a,b,c`).
func (r *Runner) UpdateRecipients(ctx context.Context, id int64, recipients []string) error {
	_, err := decode[idResult](r, ctx, "update", "--to", strings.Join(recipients, ","), strconv.FormatInt(id, 10))
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
