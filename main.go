// Command fmsg-mcp is an MCP server that shares Claude sessions as fmsg
// threads and resumes Claude sessions from an fmsg thread's ancestor chain.
// The message body is Markdown and is the sole carrier — readable by any fmsg
// client, loadable by any agent. All fmsg access shells out to fmsg-cli --json.
package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/markmnl/fmsg-mcp-claude/internal/cli"
	"github.com/markmnl/fmsg-mcp-claude/internal/identity"
	"github.com/markmnl/fmsg-mcp-claude/internal/locator"
	"github.com/markmnl/fmsg-mcp-claude/internal/session"
	"github.com/markmnl/fmsg-mcp-claude/internal/thread"
)

const version = "0.2.0"

// pendingShare is phase-1 state awaiting the user's confirmation
// (immutability means no share leaves without an explicit preview).
type pendingShare struct {
	bodies     []string // one fmsg message per exchange, pid-chained on send
	title      string
	recipients []string
	replyTo    int64
	created    time.Time
}

type server struct {
	runner   *cli.Runner
	idCfg    identity.Config
	checked  sync.Once
	checkErr error

	mu      sync.Mutex
	pending map[string]*pendingShare
}

func main() {
	s := &server{
		runner:  cli.NewRunnerFromEnv(),
		idCfg:   identity.FromEnv(),
		pending: map[string]*pendingShare{},
	}

	srv := mcp.NewServer(&mcp.Implementation{Name: "fmsg", Title: "fmsg session sharing", Version: version}, nil)

	mcp.AddTool(srv, &mcp.Tool{
		Name: "share_session",
		Description: "Share the current Claude session as an fmsg message (Markdown transcript) to the recipient " +
			"addresses given. Two-phase: the first call returns a preview (recipients, size, redactions) and a " +
			"confirm_token; present the preview to the user and, on their approval, call again with confirm_token " +
			"to send. fmsg messages are immutable and cannot be unsent.",
	}, s.shareSession)

	mcp.AddTool(srv, &mcp.Tool{
		Name: "continue_thread",
		Description: "Resume from an fmsg thread: walks the message's ancestor chain to the root and returns every " +
			"message body on the lineage, in order, as context. Use fmsg_id -1 for the most recent inbox message.",
	}, s.continueThread)

	mcp.AddTool(srv, &mcp.Tool{
		Name: "reply_to_thread",
		Description: "Send a Markdown reply into an fmsg thread. By default the reply goes to all participants of " +
			"the message being replied to; pass recipients to address a different set.",
	}, s.replyToThread)

	mcp.AddTool(srv, &mcp.Tool{
		Name:        "list_threads",
		Description: "List recent inbox messages (id, sender, topic, whether it starts a thread).",
	}, s.listThreads)

	mcp.AddTool(srv, &mcp.Tool{
		Name: "delivery_status",
		Description: "Report per-recipient delivery state for a sent fmsg message (delivered time and response code). " +
			"Cross-host delivery is asynchronous — a pending recipient may still deliver later.",
	}, s.deliveryStatus)

	mcp.AddTool(srv, &mcp.Tool{
		Name:        "whoami",
		Description: "Report the fmsg identity this server sends as (from the configured API key), plus API URL and auth type.",
	}, s.whoami)

	mcp.AddTool(srv, &mcp.Tool{
		Name:        "resolve_address",
		Description: "Dry-run the teammate-name → fmsg-address resolution chain without sending anything.",
	}, s.resolveAddress)

	addPrompts(srv)

	if err := srv.Run(context.Background(), &mcp.StdioTransport{}); err != nil {
		if strings.Contains(err.Error(), "EOF") { // client disconnected — normal shutdown
			return
		}
		log.Fatalf("fmsg-mcp: %v", err)
	}
}

// ensureCLI verifies the pinned fmsg-cli supports --json, once.
func (s *server) ensureCLI(ctx context.Context) error {
	s.checked.Do(func() { s.checkErr = s.runner.CheckJSONSupport(ctx) })
	return s.checkErr
}

// resolveIdentity asks the CLI who it is (`fmsg --json whoami` — the JWT's
// granted address). Falls back to local config plus the newest authored
// message's `from` for older CLI builds without the command.
func (s *server) resolveIdentity(ctx context.Context) (*identity.Whoami, error) {
	if info, err := s.runner.Whoami(ctx); err == nil && info.Address != "" {
		authType := "auth.json"
		if os.Getenv("FMSG_API_KEY") != "" {
			authType = "api_key_env"
		}
		return &identity.Whoami{
			Address: info.Address, APIURL: info.APIURL,
			AuthType: authType, ExpiresAt: info.TokenExpiresAt,
		}, nil
	}
	w, err := s.idCfg.ReadWhoami()
	if err != nil {
		return nil, err
	}
	if w.Address == "" {
		if items, serr := s.runner.ListSent(ctx, 1); serr == nil && len(items) > 0 {
			w.Address = items[0].From
		}
	}
	return w, nil
}

func jsonResult(v any) (*mcp.CallToolResult, any, error) {
	raw, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return nil, nil, err
	}
	return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: string(raw)}}}, nil, nil
}

func textResult(text string) (*mcp.CallToolResult, any, error) {
	return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: text}}}, nil, nil
}

// ---------------------------------------------------------------- share

type serializedTurn struct {
	Role string `json:"role" jsonschema:"user or assistant"`
	Text string `json:"text" jsonschema:"the turn's text content"`
}

type shareArgs struct {
	Recipients    []string         `json:"recipients" jsonschema:"recipient fmsg addresses (@user@example.com) or short names to resolve; list every address the thread should reach"`
	Title         string           `json:"title,omitempty" jsonschema:"thread topic; on a root share becomes the immutable fmsg topic"`
	Note          string           `json:"note,omitempty" jsonschema:"optional Markdown intro rendered above the transcript"`
	ReplyToFmsgID int64            `json:"reply_to_fmsg_id,omitempty" jsonschema:"share as a reply into an existing thread instead of starting a new one"`
	SessionID     string           `json:"session_id,omitempty" jsonschema:"Claude Code session id, if known, to disambiguate parallel sessions"`
	Transcript    []serializedTurn `json:"transcript,omitempty" jsonschema:"model-serialized transcript; only for surfaces where the server cannot read the session file (claude.ai/Desktop)"`
	ConfirmToken  string           `json:"confirm_token,omitempty" jsonschema:"token from the phase-1 preview; presence triggers the actual send"`
}

func (s *server) shareSession(ctx context.Context, req *mcp.CallToolRequest, args shareArgs) (*mcp.CallToolResult, any, error) {
	if err := s.ensureCLI(ctx); err != nil {
		return nil, nil, err
	}
	if args.ConfirmToken != "" {
		return s.shareConfirm(ctx, args.ConfirmToken)
	}
	return s.sharePreview(ctx, args)
}

func (s *server) sharePreview(ctx context.Context, args shareArgs) (*mcp.CallToolResult, any, error) {
	who, err := s.resolveIdentity(ctx)
	if err != nil {
		return nil, nil, err
	}
	if len(args.Recipients) == 0 {
		return nil, nil, fmt.Errorf("at least one recipient is required")
	}

	type resolved struct {
		Address    string `json:"address"`
		Resolution string `json:"resolution"`
	}
	var recipients []string
	var resolutions []resolved
	for _, name := range args.Recipients {
		addr, resolution, rerr := s.idCfg.Resolve(name)
		if rerr != nil {
			return nil, nil, rerr
		}
		recipients = append(recipients, addr)
		resolutions = append(resolutions, resolved{Address: addr, Resolution: resolution})
	}

	tr, err := s.buildTranscript(args, who)
	if err != nil {
		return nil, nil, err
	}
	hits := session.Redact(tr.Turns)
	bodies := session.RenderExchanges(tr)
	total, largest := 0, 0
	for _, b := range bodies {
		total += len(b)
		if len(b) > largest {
			largest = len(b)
		}
	}

	token := "st_" + randomHex(16)
	s.mu.Lock()
	for t, p := range s.pending { // expire stale previews
		if time.Since(p.created) > 15*time.Minute {
			delete(s.pending, t)
		}
	}
	s.pending[token] = &pendingShare{
		bodies: bodies, title: tr.Title, recipients: recipients,
		replyTo: args.ReplyToFmsgID, created: time.Now(),
	}
	s.mu.Unlock()

	return jsonResult(map[string]any{
		"status":        "needs_confirmation",
		"from":          who.Address,
		"recipients":    resolutions,
		"fidelity":      tr.Fidelity,
		"title":         tr.Title,
		"turns":         len(tr.Turns),
		"messages":      len(bodies),
		"total_bytes":   total,
		"largest_bytes": largest,
		"redactions":    hits,
		"warning":       "This sends one pid-chained fmsg message per user prompt. fmsg messages are immutable and cannot be unsent. Present this preview to the user and only re-invoke with confirm_token after their explicit approval.",
		"confirm_token": token,
	})
}

func (s *server) shareConfirm(ctx context.Context, token string) (*mcp.CallToolResult, any, error) {
	s.mu.Lock()
	p := s.pending[token]
	delete(s.pending, token)
	s.mu.Unlock()
	if p == nil {
		return nil, nil, fmt.Errorf("unknown or expired confirm_token; run share_session again for a fresh preview")
	}

	dir, err := os.MkdirTemp("", "fmsg-mcp-share-*")
	if err != nil {
		return nil, nil, err
	}
	defer os.RemoveAll(dir)

	// Send one message per exchange, each pid-linked to the previous, so the
	// fmsg thread mirrors the conversation. Already-sent messages cannot be
	// unsent, so a mid-chain failure reports what did go out.
	var sent []int64
	prev := p.replyTo
	for i, body := range p.bodies {
		bodyPath := filepath.Join(dir, fmt.Sprintf("body-%d.md", i))
		if err := os.WriteFile(bodyPath, []byte(body), 0o600); err != nil {
			return s.sharePartial(ctx, sent, 0, err)
		}
		topic := ""
		if i == 0 && p.replyTo == 0 {
			topic = orDefault(p.title, "Claude session")
		}
		id, err := s.runner.DraftCreate(ctx, p.recipients[0], bodyPath, topic, prev)
		if err != nil {
			return s.sharePartial(ctx, sent, 0, err)
		}
		if len(p.recipients) > 1 {
			if err := s.runner.UpdateRecipients(ctx, id, p.recipients); err != nil {
				return s.sharePartial(ctx, sent, id, err)
			}
		}
		if err := s.runner.SetType(ctx, id, "text/markdown"); err != nil {
			return s.sharePartial(ctx, sent, id, err)
		}
		if err := s.runner.DraftSend(ctx, id); err != nil {
			return s.sharePartial(ctx, sent, id, err)
		}
		sent = append(sent, id)
		prev = id
	}

	result := map[string]any{
		"status":      "sent",
		"fmsg_ids":    sent,
		"thread_head": sent[len(sent)-1],
		"recipients":  p.recipients,
		"note":        fmt.Sprintf("one message per prompt, pid-chained; recipients can branch from or resume up to any of them (continue_thread %d for the whole session)", sent[len(sent)-1]),
	}
	// Delivery snapshot of the chain head: local recipients resolve
	// immediately; federation fills in later (delivery_status re-checks).
	if delivery, derr := s.deliverySnapshot(ctx, sent[len(sent)-1]); derr == nil {
		result["delivery"] = delivery
	}
	return jsonResult(result)
}

// deliveryEntry is one recipient's delivery state, decoded for readability.
type deliveryEntry struct {
	Addr          string  `json:"addr"`
	TimeDelivered *string `json:"time_delivered"`
	ResponseCode  *int    `json:"response_code"`
	Meaning       string  `json:"meaning"`
}

func (s *server) deliverySnapshot(ctx context.Context, id int64) ([]deliveryEntry, error) {
	msg, err := s.runner.Get(ctx, id)
	if err != nil {
		return nil, err
	}
	var out []deliveryEntry
	add := func(d cli.RecipientDelivery) {
		out = append(out, deliveryEntry{
			Addr: d.Addr, TimeDelivered: d.TimeDelivered, ResponseCode: d.ResponseCode,
			Meaning: deliveryMeaning(d),
		})
	}
	for _, d := range msg.ToDelivery {
		add(d)
	}
	for _, b := range msg.AddTo {
		for _, d := range b.ToDelivery {
			add(d)
		}
	}
	return out, nil
}

// deliveryMeaning decodes the response codes the webapi documents for local
// delivery; anything else is a wire-protocol code (see fmsg SPEC §8) or still
// pending.
func deliveryMeaning(d cli.RecipientDelivery) string {
	if d.ResponseCode == nil {
		if d.TimeDelivered != nil {
			return "delivered"
		}
		return "pending (cross-host delivery is asynchronous)"
	}
	switch *d.ResponseCode {
	case 200:
		return "delivered"
	case 100:
		return "failed: user unknown on that host"
	case 102:
		return "failed: user not accepting new messages"
	default:
		if d.TimeDelivered != nil {
			return fmt.Sprintf("delivered (response code %d)", *d.ResponseCode)
		}
		return fmt.Sprintf("response code %d (see fmsg spec response codes)", *d.ResponseCode)
	}
}

// sharePartial reports a mid-chain failure honestly: messages already sent
// stay sent; only the failed draft is cleaned up.
func (s *server) sharePartial(ctx context.Context, sent []int64, failedDraft int64, cause error) (*mcp.CallToolResult, any, error) {
	if failedDraft != 0 {
		if derr := s.runner.Del(ctx, failedDraft); derr != nil {
			cause = fmt.Errorf("%w (and draft %d cleanup failed: %v)", cause, failedDraft, derr)
		}
	}
	if len(sent) == 0 {
		return nil, nil, cause
	}
	return jsonResult(map[string]any{
		"status":   "partial",
		"fmsg_ids": sent,
		"error":    cause.Error(),
		"note":     fmt.Sprintf("%d of the chain's messages were sent before the failure and cannot be unsent; retry can continue the chain with reply_to_fmsg_id=%d", len(sent), sent[len(sent)-1]),
	})
}

func (s *server) buildTranscript(args shareArgs, who *identity.Whoami) (*session.Transcript, error) {
	tr := &session.Transcript{
		Title:         args.Title,
		SharerAddress: who.Address,
		SharedAt:      float64(time.Now().Unix()),
	}

	if len(args.Transcript) > 0 {
		tr.Surface = "claude-ai"
		tr.Fidelity = "model-serialized"
		for _, t := range args.Transcript {
			tr.Turns = append(tr.Turns, session.Turn{Role: t.Role, Blocks: []session.Block{{Type: "text", Text: t.Text}}})
		}
		return tr, nil
	}

	home, err := os.UserHomeDir()
	if err != nil {
		return nil, err
	}
	projectDir, err := os.Getwd() // assumption A1: stdio server inherits the project CWD
	if err != nil {
		return nil, err
	}
	path, sid, _, err := locator.Locate(home, projectDir, args.SessionID)
	if err != nil {
		return nil, fmt.Errorf("no transcript found (is this Claude Code with the SessionStart hook installed?); "+
			"on claude.ai/Desktop pass the transcript argument instead: %w", err)
	}
	pt, err := session.ParseJSONL(path)
	if err != nil {
		return nil, err
	}
	tr.Surface = "claude-code"
	tr.Fidelity = "verbatim"
	tr.SessionID = sid
	tr.Model = pt.Model
	tr.Turns = pt.Turns
	if tr.Title == "" {
		tr.Title = deriveTitle(pt.Turns)
	}
	if args.Note != "" {
		note := session.Turn{Role: "user", Blocks: []session.Block{{Type: "text", Text: "[share note] " + args.Note}}}
		tr.Turns = append([]session.Turn{note}, tr.Turns...)
	}
	return tr, nil
}

func deriveTitle(turns []session.Turn) string {
	for _, t := range turns {
		if t.Role != "user" {
			continue
		}
		for _, b := range t.Blocks {
			if b.Type == "text" && strings.TrimSpace(b.Text) != "" {
				title := strings.TrimSpace(b.Text)
				if i := strings.IndexByte(title, '\n'); i > 0 {
					title = title[:i]
				}
				if len(title) > 80 {
					title = title[:80] + "…"
				}
				return title
			}
		}
	}
	return "Claude session"
}

// ---------------------------------------------------------------- resume

type continueArgs struct {
	FmsgID int64 `json:"fmsg_id" jsonschema:"target fmsg message id; -1 means the most recent inbox message"`
}

func (s *server) continueThread(ctx context.Context, req *mcp.CallToolRequest, args continueArgs) (*mcp.CallToolResult, any, error) {
	if err := s.ensureCLI(ctx); err != nil {
		return nil, nil, err
	}
	if args.FmsgID == 0 {
		args.FmsgID = -1
	}
	id := args.FmsgID
	if id < 0 { // resolve the negative index once so the walk uses a stable id
		items, err := s.runner.List(ctx, int(-id))
		if err != nil {
			return nil, nil, err
		}
		if len(items) < int(-id) {
			return nil, nil, fmt.Errorf("inbox has fewer than %d messages", -id)
		}
		id = items[-id-1].ID
	}
	asm, err := thread.Assemble(ctx, s.runner, id)
	if err != nil {
		return nil, nil, err
	}
	return textResult(asm.Context)
}

// ---------------------------------------------------------------- reply

type replyArgs struct {
	FmsgID     int64    `json:"fmsg_id" jsonschema:"message being replied to (pid target)"`
	Body       string   `json:"body" jsonschema:"Markdown reply body"`
	Recipients []string `json:"recipients,omitempty" jsonschema:"override the recipient set; default is all participants of the message being replied to"`
}

func (s *server) replyToThread(ctx context.Context, req *mcp.CallToolRequest, args replyArgs) (*mcp.CallToolResult, any, error) {
	if err := s.ensureCLI(ctx); err != nil {
		return nil, nil, err
	}
	if strings.TrimSpace(args.Body) == "" {
		return nil, nil, fmt.Errorf("reply body is empty")
	}

	parent, err := s.runner.Get(ctx, args.FmsgID)
	if err != nil {
		return nil, nil, err
	}
	who, err := s.resolveIdentity(ctx)
	if err != nil {
		return nil, nil, err
	}

	recipients := args.Recipients
	if len(recipients) == 0 {
		for _, addr := range parent.Participants() {
			if strings.EqualFold(addr, who.Address) {
				continue // don't address ourselves
			}
			recipients = append(recipients, addr)
		}
	}
	if len(recipients) == 0 {
		return nil, nil, fmt.Errorf("no recipients: you are the only participant of message %d", args.FmsgID)
	}

	dir, err := os.MkdirTemp("", "fmsg-mcp-reply-*")
	if err != nil {
		return nil, nil, err
	}
	defer os.RemoveAll(dir)
	bodyPath := filepath.Join(dir, "reply.md")
	if err := os.WriteFile(bodyPath, []byte(args.Body), 0o600); err != nil {
		return nil, nil, err
	}

	id, err := s.runner.DraftCreate(ctx, recipients[0], bodyPath, "", args.FmsgID)
	if err != nil {
		return nil, nil, err
	}
	cleanup := func(cause error) (*mcp.CallToolResult, any, error) {
		if derr := s.runner.Del(ctx, id); derr != nil {
			return nil, nil, fmt.Errorf("%w (and draft %d cleanup failed: %v)", cause, id, derr)
		}
		return nil, nil, cause
	}
	if len(recipients) > 1 {
		if err := s.runner.UpdateRecipients(ctx, id, recipients); err != nil {
			return cleanup(err)
		}
	}
	if err := s.runner.SetType(ctx, id, "text/markdown"); err != nil {
		return cleanup(err)
	}
	if err := s.runner.DraftSend(ctx, id); err != nil {
		return cleanup(err)
	}
	result := map[string]any{"status": "sent", "fmsg_id": id, "recipients": recipients}
	if delivery, derr := s.deliverySnapshot(ctx, id); derr == nil {
		result["delivery"] = delivery
	}
	return jsonResult(result)
}

// ---------------------------------------------------------------- delivery

type deliveryArgs struct {
	FmsgID int64 `json:"fmsg_id" jsonschema:"the sent message to check"`
}

func (s *server) deliveryStatus(ctx context.Context, req *mcp.CallToolRequest, args deliveryArgs) (*mcp.CallToolResult, any, error) {
	if err := s.ensureCLI(ctx); err != nil {
		return nil, nil, err
	}
	delivery, err := s.deliverySnapshot(ctx, args.FmsgID)
	if err != nil {
		return nil, nil, err
	}
	return jsonResult(map[string]any{"fmsg_id": args.FmsgID, "delivery": delivery})
}

// ---------------------------------------------------------------- list/identity

type listArgs struct {
	Limit int `json:"limit,omitempty" jsonschema:"max inbox messages to return (default 20, max 100)"`
}

func (s *server) listThreads(ctx context.Context, req *mcp.CallToolRequest, args listArgs) (*mcp.CallToolResult, any, error) {
	if err := s.ensureCLI(ctx); err != nil {
		return nil, nil, err
	}
	limit := args.Limit
	if limit <= 0 {
		limit = 20
	}
	if limit > 100 {
		limit = 100
	}
	items, err := s.runner.List(ctx, limit)
	if err != nil {
		return nil, nil, err
	}
	type entry struct {
		FmsgID int64    `json:"fmsg_id"`
		From   string   `json:"from"`
		Topic  string   `json:"topic,omitempty"`
		Time   *float64 `json:"time"`
		IsRoot bool     `json:"is_root"`
	}
	out := []entry{}
	for _, it := range items {
		out = append(out, entry{FmsgID: it.ID, From: it.From, Topic: it.Topic, Time: it.Time, IsRoot: it.PID == nil})
	}
	return jsonResult(out)
}

func (s *server) whoami(ctx context.Context, req *mcp.CallToolRequest, args struct{}) (*mcp.CallToolResult, any, error) {
	if err := s.ensureCLI(ctx); err != nil {
		return nil, nil, err
	}
	w, err := s.resolveIdentity(ctx)
	if err != nil {
		return nil, nil, err
	}
	out := map[string]any{"address": w.Address, "api_url": w.APIURL, "auth_type": w.AuthType}
	if w.ExpiresAt != "" {
		out["expires_at"] = w.ExpiresAt
	}
	if w.Address == "" {
		out["note"] = "address not locally knowable: the API key's granted address lives server-side and nothing has been sent yet; it will be known after the first share (fmsg-cli lacks a whoami command — upstream ask)"
	}
	return jsonResult(out)
}

type resolveArgs struct {
	Name string `json:"name" jsonschema:"teammate name or full fmsg address to resolve"`
}

func (s *server) resolveAddress(ctx context.Context, req *mcp.CallToolRequest, args resolveArgs) (*mcp.CallToolResult, any, error) {
	addr, resolution, err := s.idCfg.Resolve(args.Name)
	if err != nil {
		return nil, nil, err
	}
	return jsonResult(map[string]string{"address": addr, "resolution": resolution})
}

// ---------------------------------------------------------------- prompts

func addPrompts(srv *mcp.Server) {
	srv.AddPrompt(&mcp.Prompt{
		Name:        "share_session",
		Title:       "Share this session via fmsg",
		Description: "Share the current Claude session as an fmsg thread.",
		Arguments: []*mcp.PromptArgument{
			{Name: "recipients", Description: "recipient fmsg address(es), comma-separated", Required: true},
		},
	}, func(ctx context.Context, req *mcp.GetPromptRequest) (*mcp.GetPromptResult, error) {
		recipients := req.Params.Arguments["recipients"]
		text := fmt.Sprintf("Share this session with %q via fmsg: call the share_session tool, present its preview to me "+
			"(recipients, turn count, size, redactions, and the cannot-be-unsent warning), and only after I approve, "+
			"call share_session again with the confirm_token.", recipients)
		return &mcp.GetPromptResult{Messages: []*mcp.PromptMessage{
			{Role: "user", Content: &mcp.TextContent{Text: text}},
		}}, nil
	})

	srv.AddPrompt(&mcp.Prompt{
		Name:        "continue_thread",
		Title:       "Continue an fmsg thread",
		Description: "Resume a Claude session seeded with an fmsg thread's ancestor history.",
		Arguments: []*mcp.PromptArgument{
			{Name: "fmsg_id", Description: "fmsg message id (omit for the most recent inbox message)"},
		},
	}, func(ctx context.Context, req *mcp.GetPromptRequest) (*mcp.GetPromptResult, error) {
		id := req.Params.Arguments["fmsg_id"]
		if id == "" {
			id = "-1"
		}
		text := fmt.Sprintf("Call the fmsg continue_thread tool with fmsg_id %s and continue the conversation "+
			"from the returned context, telling me briefly what the thread is about and where it left off.", id)
		return &mcp.GetPromptResult{Messages: []*mcp.PromptMessage{
			{Role: "user", Content: &mcp.TextContent{Text: text}},
		}}, nil
	})
}

// ---------------------------------------------------------------- misc

func randomHex(n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return hex.EncodeToString(b)
}

func orDefault(s, def string) string {
	if s == "" {
		return def
	}
	return s
}
