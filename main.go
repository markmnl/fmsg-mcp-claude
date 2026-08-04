// Command fmsg-mcp is an MCP server that shares Claude sessions as fmsg
// threads and resumes Claude sessions from an fmsg thread's ancestor chain.
// All fmsg access shells out to fmsg-cli --json (see ARCHITECTURE.md).
package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log"
	"net/url"
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

const version = "0.1.0"

// pendingShare is phase-1 state awaiting the user's confirmation
// (immutability means no share leaves without an explicit preview).
type pendingShare struct {
	env       *session.Envelope
	recipient string // resolved human address
	replyTo   int64
	created   time.Time
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
		Description: "Share the current Claude session with a teammate as an fmsg thread. Two-phase: " +
			"the first call returns a preview (recipient, size, redactions) and a confirm_token; " +
			"present the preview to the user and, on their approval, call again with confirm_token to send. " +
			"fmsg messages are immutable and cannot be unsent.",
	}, s.shareSession)

	mcp.AddTool(srv, &mcp.Tool{
		Name: "continue_thread",
		Description: "Resume a Claude session from an fmsg thread: walks the message's ancestor chain to the root, " +
			"restores the deepest shared session transcript plus later replies, and returns it all as context. " +
			"Use fmsg_id -1 for the most recent inbox message.",
	}, s.continueThread)

	mcp.AddTool(srv, &mcp.Tool{
		Name: "reply_to_thread",
		Description: "Send a Markdown reply into an fmsg thread (visible to all participants in any fmsg client). " +
			"To attach a full session snapshot instead, use share_session with reply_to_fmsg_id.",
	}, s.replyToThread)

	mcp.AddTool(srv, &mcp.Tool{
		Name:        "list_shared_threads",
		Description: "List inbox messages that carry a shared Claude session transcript.",
	}, s.listSharedThreads)

	mcp.AddTool(srv, &mcp.Tool{
		Name:        "whoami",
		Description: "Report the fmsg identity this server is logged in as (address, API URL, auth type).",
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
	Recipient        string           `json:"recipient" jsonschema:"teammate: full fmsg address (@bob@example.com) or a short name to resolve"`
	Title            string           `json:"title,omitempty" jsonschema:"thread topic; on a root share becomes the immutable fmsg topic"`
	Note             string           `json:"note,omitempty" jsonschema:"optional Markdown intro rendered above the transcript"`
	ReplyToFmsgID    int64            `json:"reply_to_fmsg_id,omitempty" jsonschema:"share as a reply into an existing thread instead of starting a new one"`
	SessionID        string           `json:"session_id,omitempty" jsonschema:"Claude Code session id, if known, to disambiguate parallel sessions"`
	Transcript       []serializedTurn `json:"transcript,omitempty" jsonschema:"model-serialized transcript; only for surfaces where the server cannot read the session file (claude.ai/Desktop)"`
	ConfirmedAddress string           `json:"confirmed_address,omitempty" jsonschema:"echo of the resolved address from a needs_confirmation response"`
	ConfirmToken     string           `json:"confirm_token,omitempty" jsonschema:"token from the phase-1 preview; presence triggers the actual send"`
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
	who, err := s.idCfg.ReadWhoami()
	if err != nil {
		return nil, nil, err
	}

	recipient, resolution := args.ConfirmedAddress, "confirmed"
	if recipient == "" {
		recipient, resolution, err = s.idCfg.Resolve(args.Recipient)
		if err != nil {
			return nil, nil, err
		}
	}

	env, err := s.buildEnvelope(args, who)
	if err != nil {
		return nil, nil, err
	}
	hits := session.Redact(env.Turns)
	env.Redaction = session.Redaction{Applied: true, PatternsHit: hits}

	filename, data, err := session.Encode(env)
	if err != nil {
		return nil, nil, err
	}
	body := session.RenderMarkdown(env, 0)

	token := "st_" + randomHex(16)
	s.mu.Lock()
	for t, p := range s.pending { // expire stale previews
		if time.Since(p.created) > 15*time.Minute {
			delete(s.pending, t)
		}
	}
	s.pending[token] = &pendingShare{env: env, recipient: recipient, replyTo: args.ReplyToFmsgID, created: time.Now()}
	s.mu.Unlock()

	return jsonResult(map[string]any{
		"status":              "needs_confirmation",
		"resolved_recipient":  recipient,
		"resolution":          resolution,
		"also_added":          s.alsoAdded(recipient, who),
		"fidelity":            env.Provenance.Fidelity,
		"turns":               len(env.Turns),
		"body_bytes":          len(body),
		"attachment_bytes":    len(data),
		"attachment_filename": filename,
		"redactions":          hits,
		"warning":             "fmsg messages are immutable and cannot be unsent. Present this preview to the user and only re-invoke with confirm_token after their explicit approval.",
		"confirm_token":       token,
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

	filename, data, err := session.Encode(p.env)
	if err != nil {
		return nil, nil, err
	}
	attachPath := filepath.Join(dir, filename)
	if err := os.WriteFile(attachPath, data, 0o600); err != nil {
		return nil, nil, err
	}
	// Provisional body first; the final body embeds the fmsg id, which only
	// exists after draft create (ARCHITECTURE.md §3).
	bodyPath := filepath.Join(dir, "body.md")
	if err := os.WriteFile(bodyPath, []byte(session.BodyMarker+"\n(placeholder)"), 0o600); err != nil {
		return nil, nil, err
	}

	topic := ""
	if p.replyTo == 0 {
		topic = orDefault(p.env.Title, "Claude session")
	}
	id, err := s.runner.DraftCreate(ctx, p.recipient, bodyPath, topic, p.replyTo)
	if err != nil {
		return nil, nil, err
	}
	cleanup := func(cause error) (*mcp.CallToolResult, any, error) {
		if derr := s.runner.Del(ctx, id); derr != nil {
			return nil, nil, fmt.Errorf("%w (and draft %d cleanup failed: %v)", cause, id, derr)
		}
		return nil, nil, cause
	}

	finalBody := session.RenderMarkdown(p.env, id)
	if err := os.WriteFile(bodyPath, []byte(finalBody), 0o600); err != nil {
		return cleanup(err)
	}
	if err := s.runner.SetBodyAndType(ctx, id, bodyPath, "text/markdown"); err != nil {
		return cleanup(err)
	}
	if _, err := s.runner.Attach(ctx, id, attachPath); err != nil {
		return cleanup(err)
	}
	if err := s.runner.DraftSend(ctx, id); err != nil {
		return cleanup(err)
	}

	who, _ := s.idCfg.ReadWhoami()
	extra := s.alsoAdded(p.recipient, who)
	if err := s.runner.AddTo(ctx, id, extra); err != nil {
		// The share itself succeeded; report the partial add-to rather than failing.
		return jsonResult(map[string]any{
			"status": "sent", "fmsg_id": id,
			"warning": fmt.Sprintf("message sent, but adding %v failed: %v", extra, err),
		})
	}

	prompt := fmt.Sprintf("Use the fmsg tool continue_thread with message id %d", id)
	return jsonResult(map[string]any{
		"status":         "sent",
		"fmsg_id":        id,
		"recipients":     append([]string{p.recipient}, extra...),
		"open_in_claude": "https://claude.ai/new?q=" + strings.ReplaceAll(url.QueryEscape(prompt), "+", "%20"),
	})
}

// alsoAdded computes the dual-addressing add-to set: the recipient's _claude
// sub-account and the sharer's human address (ARCHITECTURE.md §5).
func (s *server) alsoAdded(recipient string, who *identity.Whoami) []string {
	var out []string
	if agent := s.idCfg.AgentFor(recipient); agent != "" && !strings.EqualFold(agent, recipient) {
		out = append(out, agent)
	}
	if who != nil && who.HumanAddress != "" && !strings.EqualFold(who.HumanAddress, who.Address) {
		out = append(out, who.HumanAddress)
	}
	return out
}

func (s *server) buildEnvelope(args shareArgs, who *identity.Whoami) (*session.Envelope, error) {
	env := &session.Envelope{
		Format:              session.FormatName,
		FormatVersion:       session.FormatVersion,
		Title:               args.Title,
		AttachmentsManifest: []session.ManifestEntry{},
		Provenance: session.Provenance{
			SharedAt:            float64(time.Now().Unix()),
			SharerAddress:       who.Address,
			SharerHumanAddress:  who.HumanAddress,
			IncorporatedFmsgIDs: []int64{},
		},
	}
	if args.ReplyToFmsgID != 0 {
		env.Provenance.BaseFmsgID = &args.ReplyToFmsgID
	}

	if len(args.Transcript) > 0 {
		env.Provenance.Surface = "claude-ai"
		env.Provenance.Fidelity = "model-serialized"
		for i, t := range args.Transcript {
			env.Turns = append(env.Turns, session.Turn{I: i, Role: t.Role, Blocks: []session.Block{{Type: "text", Text: t.Text}}})
		}
		env.Truncation = session.Truncation{OriginalTurnCount: len(env.Turns)}
		return env, nil
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
	env.Provenance.Surface = "claude-code"
	env.Provenance.Fidelity = "verbatim"
	env.Provenance.SessionID = sid
	env.Provenance.CWD = pt.CWD
	env.Provenance.Model = pt.Model
	if pt.GitBranch != "" {
		env.Provenance.Git = &session.GitInfo{Branch: pt.GitBranch}
	}
	env.Turns = pt.Turns
	env.Truncation = session.Truncation{OriginalTurnCount: len(pt.Turns)}
	if env.Title == "" {
		env.Title = deriveTitle(pt.Turns)
	}
	if args.Note != "" {
		note := session.Turn{I: 0, Role: "user", Blocks: []session.Block{{Type: "text", Text: "[share note] " + args.Note}}}
		env.Turns = append([]session.Turn{note}, env.Turns...)
	}
	return env, nil
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
	FmsgID    int64  `json:"fmsg_id" jsonschema:"message being replied to (pid target)"`
	Body      string `json:"body" jsonschema:"Markdown reply body"`
	AlsoShare bool   `json:"also_share,omitempty" jsonschema:"true to attach a full session snapshot instead; delegates to share_session"`
}

func (s *server) replyToThread(ctx context.Context, req *mcp.CallToolRequest, args replyArgs) (*mcp.CallToolResult, any, error) {
	if err := s.ensureCLI(ctx); err != nil {
		return nil, nil, err
	}
	if args.AlsoShare {
		return nil, nil, fmt.Errorf("to attach a session snapshot, call share_session with reply_to_fmsg_id=%d instead", args.FmsgID)
	}
	if strings.TrimSpace(args.Body) == "" {
		return nil, nil, fmt.Errorf("reply body is empty")
	}

	parent, err := s.runner.Get(ctx, args.FmsgID)
	if err != nil {
		return nil, nil, err
	}
	who, err := s.idCfg.ReadWhoami()
	if err != nil {
		return nil, nil, err
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

	id, err := s.runner.DraftCreate(ctx, parent.From, bodyPath, "", args.FmsgID)
	if err != nil {
		return nil, nil, err
	}
	if err := s.runner.SetType(ctx, id, "text/markdown"); err != nil {
		_ = s.runner.Del(ctx, id)
		return nil, nil, err
	}
	if err := s.runner.DraftSend(ctx, id); err != nil {
		_ = s.runner.Del(ctx, id)
		return nil, nil, err
	}

	// Fan the reply out to the parent's remaining participants, minus our own
	// addresses and the draft's recipient.
	var extras []string
	for _, addr := range parent.Participants() {
		if strings.EqualFold(addr, parent.From) ||
			strings.EqualFold(addr, who.Address) ||
			(who.HumanAddress != "" && strings.EqualFold(addr, who.HumanAddress)) {
			continue
		}
		extras = append(extras, addr)
	}
	if err := s.runner.AddTo(ctx, id, extras); err != nil {
		return jsonResult(map[string]any{
			"status": "sent", "fmsg_id": id,
			"warning": fmt.Sprintf("reply sent, but adding %v failed: %v", extras, err),
		})
	}
	return jsonResult(map[string]any{"status": "sent", "fmsg_id": id, "recipients": append([]string{parent.From}, extras...)})
}

// ---------------------------------------------------------------- list/identity

type listArgs struct {
	Limit int `json:"limit,omitempty" jsonschema:"max inbox messages to scan (default 20, max 100)"`
}

func (s *server) listSharedThreads(ctx context.Context, req *mcp.CallToolRequest, args listArgs) (*mcp.CallToolResult, any, error) {
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
		Title  string   `json:"title"`
		Time   *float64 `json:"time"`
		IsRoot bool     `json:"is_root"`
	}
	out := []entry{}
	for _, it := range items {
		for _, a := range it.Attachments {
			if a.Filename == session.AttachmentName || a.Filename == session.AttachmentNameGz {
				out = append(out, entry{FmsgID: it.ID, From: it.From, Title: it.Topic, Time: it.Time, IsRoot: it.PID == nil})
				break
			}
		}
	}
	return jsonResult(out)
}

func (s *server) whoami(ctx context.Context, req *mcp.CallToolRequest, args struct{}) (*mcp.CallToolResult, any, error) {
	w, err := s.idCfg.ReadWhoami()
	if err != nil {
		return nil, nil, err
	}
	return jsonResult(w)
}

type resolveArgs struct {
	Name string `json:"name" jsonschema:"teammate name or full fmsg address to resolve"`
}

func (s *server) resolveAddress(ctx context.Context, req *mcp.CallToolRequest, args resolveArgs) (*mcp.CallToolResult, any, error) {
	addr, resolution, err := s.idCfg.Resolve(args.Name)
	if err != nil {
		return nil, nil, err
	}
	return jsonResult(map[string]string{"address": addr, "resolution": resolution, "agent_address": s.idCfg.AgentFor(addr)})
}

// ---------------------------------------------------------------- prompts

func addPrompts(srv *mcp.Server) {
	srv.AddPrompt(&mcp.Prompt{
		Name:        "share_session",
		Title:       "Share this session via fmsg",
		Description: "Share the current Claude session with a teammate as an fmsg thread.",
		Arguments: []*mcp.PromptArgument{
			{Name: "recipient", Description: "teammate name or @user@example.com address", Required: true},
		},
	}, func(ctx context.Context, req *mcp.GetPromptRequest) (*mcp.GetPromptResult, error) {
		recipient := req.Params.Arguments["recipient"]
		text := fmt.Sprintf("Share this session with %q via fmsg: call the share_session tool, present its preview to me "+
			"(recipients, turn count, sizes, redactions, and the cannot-be-unsent warning), and only after I approve, "+
			"call share_session again with the confirm_token.", recipient)
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
			"from the returned context, telling me briefly what the session was about and where it left off.", id)
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
