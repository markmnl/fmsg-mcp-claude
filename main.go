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
	"github.com/markmnl/fmsg-mcp-claude/internal/sharestate"
	"github.com/markmnl/fmsg-mcp-claude/internal/thread"
)

// version is stamped by the release workflow (-X main.version=<tag>);
// "dev" identifies local builds.
var version = "dev"

// pendingShare is phase-1 state awaiting the user's confirmation
// (immutability means no share leaves without an explicit preview).
type pendingShare struct {
	bodies     []string // one fmsg message per exchange, pid-chained on send
	title      string
	recipients []string
	replyTo    int64
	created    time.Time

	// Incremental-share bookkeeping (sharestate): which session this is,
	// hashes of every exchange in it, how many were already shared, and the
	// existing thread root when continuing.
	sessionID  string
	allHashes  []string
	baseCount  int
	threadRoot int64
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
		runner:  cli.NewRunnerFromEnv(version),
		idCfg:   identity.FromEnv(),
		pending: map[string]*pendingShare{},
	}

	srv := mcp.NewServer(&mcp.Implementation{Name: "fmsg", Title: "fmsg session sharing", Version: version}, nil)

	mcp.AddTool(srv, &mcp.Tool{
		Name: "share_session",
		Description: "Share the current Claude session as an fmsg message (Markdown transcript) to the recipient " +
			"addresses given. Two-phase: the first call returns a preview (recipients, size, redactions) and a " +
			"confirm_token; show the user the preview, ask simply \"Are you sure?\", and only after they say yes " +
			"call again with confirm_token. Re-sharing a session already shared to the same recipients sends only " +
			"the new exchanges, chained onto the existing thread — no need to track message ids yourself.",
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
	Title         string           `json:"title,omitempty" jsonschema:"thread topic; defaults to the session's own summary. On a root share this becomes the immutable fmsg topic"`
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

	// Incremental share: when this session was already shared to the same
	// audience and its content still extends what was sent, chain only the
	// new exchanges onto the existing thread instead of resending everything.
	mode := "new_thread"
	sendBodies := bodies
	allHashes := sharestate.HashBodies(bodies)
	baseCount := 0
	replyTo := args.ReplyToFmsgID
	var threadRoot int64
	if replyTo == 0 && tr.SessionID != "" {
		if st, serr := sharestate.Load(tr.SessionID); serr == nil && st != nil {
			delta, extends := sharestate.Delta(st.ExchangeHashes, bodies)
			switch {
			case !extends:
				mode = "new_thread_session_diverged"
			case !sharestate.SameRecipients(st.Recipients, recipients):
				mode = "new_thread_different_audience"
			case len(delta) == 0:
				return jsonResult(map[string]any{
					"status": "nothing_new",
					"note": fmt.Sprintf("every exchange in this session is already in the fmsg thread rooted at message %d (last message %d); nothing to send",
						st.ThreadRoot, st.LastFmsgID),
				})
			default:
				mode = "continue_shared_thread"
				sendBodies = delta
				baseCount = len(st.ExchangeHashes)
				replyTo = st.LastFmsgID
				threadRoot = st.ThreadRoot
			}
		}
	}

	total, largest := 0, 0
	for _, b := range sendBodies {
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
		bodies: sendBodies, title: tr.Title, recipients: recipients,
		replyTo: replyTo, created: time.Now(),
		sessionID: tr.SessionID, allHashes: allHashes,
		baseCount: baseCount, threadRoot: threadRoot,
	}
	s.mu.Unlock()

	preview := map[string]any{
		"status":        "needs_confirmation",
		"mode":          mode,
		"from":          who.Address,
		"recipients":    resolutions,
		"fidelity":      tr.Fidelity,
		"title":         tr.Title,
		"turns":         len(tr.Turns),
		"messages":      len(sendBodies),
		"total_bytes":   total,
		"largest_bytes": largest,
		"redactions":    hits,
		"confirm":       "Present this preview to the user as a short multi-line list (from, recipients, title, turns/messages, size, any redactions) — then on its own line ask exactly: \"Are you sure?\" No extra warnings. Re-invoke with confirm_token only after they say yes.",
		"confirm_token": token,
	}
	if mode == "continue_shared_thread" {
		preview["already_shared"] = baseCount
		preview["continuing_thread_root"] = threadRoot
		preview["note"] = fmt.Sprintf("only the %d new message(s) will be sent, chained onto the thread already shared (root %d)", len(sendBodies), threadRoot)
	}
	return jsonResult(preview)
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
	// fmsg thread mirrors the conversation. Sent messages cannot be recalled,
	// so a mid-chain failure reports what did go out.
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
		// The update must restate every field: webapi PUT is full-replacement,
		// so a type-only update would wipe recipients, pid, and topic.
		if err := s.runner.UpdateFull(ctx, id, bodyPath, p.recipients, "text/markdown", topic, prev); err != nil {
			return s.sharePartial(ctx, sent, id, err)
		}
		if err := s.runner.DraftSend(ctx, id); err != nil {
			return s.sharePartial(ctx, sent, id, err)
		}
		sent = append(sent, id)
		prev = id
		s.saveShareState(p, sent)
		// Receiving hosts reject a reply whose parent they have not stored
		// yet (fmsg code 6), and federation delivery is concurrent — so wait
		// for this message to reach a terminal delivery state on every
		// recipient before sending its child (OPEN_QUESTIONS #18).
		if i < len(p.bodies)-1 {
			if pending := s.waitDelivered(ctx, id, 60*time.Second); len(pending) > 0 {
				return s.sharePartial(ctx, sent, 0, fmt.Errorf(
					"message %d not yet delivered to %v after 60s; stopping so later messages aren't rejected as parent-not-found — once delivery_status shows it delivered, continue the chain with reply_to_fmsg_id=%d",
					id, pending, id))
			}
		}
	}

	note := fmt.Sprintf("one message per prompt, pid-chained; recipients can branch from or resume up to any of them (continue_thread %d for the whole session)", sent[len(sent)-1])
	if p.threadRoot != 0 {
		note = fmt.Sprintf("continued the previously shared thread (root %d): only the %d new message(s) were sent; sharing again later will keep extending this thread", p.threadRoot, len(sent))
	}
	result := map[string]any{
		"status":      "sent",
		"fmsg_ids":    sent,
		"thread_head": sent[len(sent)-1],
		"recipients":  p.recipients,
		"note":        note,
	}
	// The chain pacing already confirmed every message but the last reached a
	// terminal state; wait briefly on the final one too, so the result usually
	// reports delivered/failed per recipient rather than "pending". A timeout
	// here is not a failure — delivery is async and delivery_status re-checks.
	stillPending := s.waitDelivered(ctx, sent[len(sent)-1], 60*time.Second)
	if delivery, derr := s.deliverySnapshot(ctx, sent[len(sent)-1]); derr == nil {
		result["delivery"] = delivery
	}
	if len(stillPending) > 0 {
		result["delivery_pending"] = stillPending
		result["delivery_note"] = "these recipients had not confirmed delivery within 60s; that can be normal for slow hosts — check later with delivery_status"
	}
	return jsonResult(result)
}

// saveShareState records how far this session's shared thread has got, so a
// later share of the same session continues the thread with only the new
// exchanges. Called after every successful send: a mid-chain failure still
// leaves the state at the last message that went out, and the next share
// picks up from there. Best-effort — a state write failure never fails a send.
func (s *server) saveShareState(p *pendingShare, sent []int64) {
	if p.sessionID == "" || len(sent) == 0 {
		return
	}
	root := p.threadRoot
	if root == 0 {
		root = sent[0]
	}
	st := &sharestate.State{
		SessionID:      p.sessionID,
		ThreadRoot:     root,
		LastFmsgID:     sent[len(sent)-1],
		Recipients:     p.recipients,
		ExchangeHashes: p.allHashes[:p.baseCount+len(sent)],
	}
	if err := sharestate.Save(st); err != nil {
		log.Printf("fmsg-mcp: share state not saved (next share will resend the whole session): %v", err)
	}
}

// waitDelivered polls a sent message until every recipient has a terminal
// delivery state (delivered, or a definitive failure code) or the timeout
// elapses. It returns the addresses still pending; failures are terminal here
// (a recipient whose root bounced will bounce the children too, which the
// final delivery snapshot reports).
func (s *server) waitDelivered(ctx context.Context, id int64, timeout time.Duration) []string {
	deadline := time.Now().Add(timeout)
	for {
		var pending []string
		if msg, err := s.runner.Get(ctx, id); err == nil {
			for _, d := range msg.ToDelivery {
				if d.TimeDelivered == nil && d.ResponseCode == nil {
					pending = append(pending, d.Addr)
				}
			}
		}
		if len(pending) == 0 || time.Now().After(deadline) || ctx.Err() != nil {
			return pending
		}
		time.Sleep(time.Second)
	}
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
	case 1:
		return "failed: message rejected as invalid"
	case 4:
		return "failed: too big for the receiving host"
	case 6:
		return "failed: receiving host had not stored the parent message yet (chain delivered out of order — see OPEN_QUESTIONS #18)"
	case 7:
		return "failed: message too old"
	case 8:
		return "failed: message timestamp in the future"
	case 9:
		return "failed: timestamp not after the parent's"
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
		"note":     fmt.Sprintf("%d of the chain's messages were sent before the failure and cannot be recalled; retry can continue the chain with reply_to_fmsg_id=%d", len(sent), sent[len(sent)-1]),
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
		// Prefer Claude Code's own session summary (what its session picker
		// shows); fall back to the first prompt's opening words.
		tr.Title = clipTitle(pt.Summary)
	}
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
				if title := clipTitle(b.Text); title != "" {
					return title
				}
			}
		}
	}
	return "Claude session"
}

// clipTitle reduces s to a single ≤80-char line suitable as an fmsg topic.
func clipTitle(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.IndexByte(s, '\n'); i > 0 {
		s = strings.TrimSpace(s[:i])
	}
	if len(s) > 80 {
		s = s[:80] + "…"
	}
	return s
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
	// Restate every field — webapi PUT is full-replacement (see UpdateFull).
	if err := s.runner.UpdateFull(ctx, id, bodyPath, recipients, "text/markdown", "", args.FmsgID); err != nil {
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
		text := fmt.Sprintf("Share this session with %q via fmsg: call the share_session tool, show me its preview "+
			"as a short multi-line list (from, recipients, title, turns/messages, size, redactions), then ask me simply "+
			"\"Are you sure?\", and only after I say yes call share_session again with the confirm_token.", recipients)
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
