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
	"sort"
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
	kind       string   // "" = transcript share; sharestate.KindSummary = summary
	bodies     []string // one fmsg message per exchange, pid-chained on send
	title      string
	recipients []string
	replyTo    int64
	created    time.Time

	// Incremental-share bookkeeping (sharestate): which session this is,
	// hashes of every exchange in it, how many were already shared, and the
	// existing thread root when continuing. Summary shares track only the
	// thread position plus how many summaries preceded this one.
	sessionID    string
	allHashes    []string
	baseCount    int
	threadRoot   int64
	summaryCount int
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

	// Annotation shapes (MCP hints, also required by the Anthropic extension
	// directory review): sends are additive-only — fmsg messages are immutable,
	// nothing existing is modified or deleted — so DestructiveHint is false;
	// every tool that talks to an fmsg host is open-world.
	sendHints := &mcp.ToolAnnotations{DestructiveHint: boolHint(false), OpenWorldHint: boolHint(true)}
	readHints := &mcp.ToolAnnotations{ReadOnlyHint: true, OpenWorldHint: boolHint(true)}

	mcp.AddTool(srv, &mcp.Tool{
		Name: "share_session",
		Description: "Share the current Claude session as an fmsg message (Markdown transcript) to the recipient " +
			"addresses given. Two-phase: the first call returns a preview (recipients, size, redactions) and a " +
			"confirm_token; show the user the preview, ask simply \"Are you sure?\", and only after they say yes " +
			"call again with confirm_token. Re-sharing a session already shared to the same recipients sends only " +
			"the new exchanges, chained onto the existing thread — no need to track message ids yourself.",
		Annotations: withTitle(sendHints, "Share session via fmsg"),
	}, s.shareSession)

	mcp.AddTool(srv, &mcp.Tool{
		Name: "share_summary",
		Description: "Share a summary of the current Claude session as a single fmsg message. YOU write the summary " +
			"and pass it as the summary argument — concise Markdown covering: the goal, what was done, key decisions " +
			"and why, current state, and next steps. Aim well under 8 KB; the fmsg federation default message limit " +
			"is 10 KiB. Two-phase like share_session: the first call returns a preview (recipients, size, redactions) " +
			"and a confirm_token; show the user the preview, ask simply \"Are you sure?\", and only after they say " +
			"yes call again with confirm_token. Summarising the same session again later automatically threads the " +
			"new summary as a reply to the previous one. For the full transcript use share_session instead.",
		Annotations: withTitle(sendHints, "Share session summary via fmsg"),
	}, s.shareSummary)

	mcp.AddTool(srv, &mcp.Tool{
		Name: "continue_thread",
		Description: "Resume from an fmsg thread: walks the message's ancestor chain to the root and returns every " +
			"message body on the lineage, in order, as context. Use fmsg_id -1 for the most recent inbox message.",
		Annotations: withTitle(readHints, "Continue fmsg thread"),
	}, s.continueThread)

	mcp.AddTool(srv, &mcp.Tool{
		Name: "reply_to_thread",
		Description: "Send a Markdown reply into an fmsg thread. By default the reply goes to all participants of " +
			"the message being replied to; pass recipients to address a different set.",
		Annotations: withTitle(sendHints, "Reply to fmsg thread"),
	}, s.replyToThread)

	mcp.AddTool(srv, &mcp.Tool{
		Name: "send_message",
		Description: "Send a standalone fmsg message immediately — no preview step. Use when the user asks to send " +
			"someone specific content: a verbatim text they dictate, or an answer they ask you to compose " +
			"(\"send @x the weather in london\"). recipients and body (Markdown) are required; topic defaults to the " +
			"body's first line. Secrets are auto-redacted and any redactions are reported in the result. This starts " +
			"a new thread — to reply within an existing fmsg thread use reply_to_thread; to share this session use " +
			"share_session or share_summary.",
		Annotations: withTitle(sendHints, "Send fmsg message"),
	}, s.sendMessage)

	mcp.AddTool(srv, &mcp.Tool{
		Name:        "list_threads",
		Description: "List recent inbox messages (id, sender, topic, whether it starts a thread).",
		Annotations: withTitle(readHints, "List fmsg inbox"),
	}, s.listThreads)

	mcp.AddTool(srv, &mcp.Tool{
		Name: "delivery_status",
		Description: "Report per-recipient delivery state for a sent fmsg message (delivered time and response code). " +
			"Cross-host delivery is asynchronous — a pending recipient may still deliver later.",
		Annotations: withTitle(readHints, "Check fmsg delivery status"),
	}, s.deliveryStatus)

	mcp.AddTool(srv, &mcp.Tool{
		Name:        "whoami",
		Description: "Report the fmsg identity this server sends as (from the configured API key), plus API URL and auth type.",
		Annotations: withTitle(readHints, "Show fmsg identity"),
	}, s.whoami)

	mcp.AddTool(srv, &mcp.Tool{
		Name:        "resolve_address",
		Description: "Dry-run the teammate-name → fmsg-address resolution chain without sending anything.",
		// Resolution is purely local (env + directory file): closed world.
		Annotations: &mcp.ToolAnnotations{Title: "Resolve fmsg address", ReadOnlyHint: true, OpenWorldHint: boolHint(false)},
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

// resolvedRecipient is one recipient's name→address resolution, for previews
// and results.
type resolvedRecipient struct {
	Address    string `json:"address"`
	Resolution string `json:"resolution"`
}

// resolveRecipients resolves every name through the identity chain
// (literal/directory/convention), failing on the first unresolvable one.
func (s *server) resolveRecipients(names []string) ([]string, []resolvedRecipient, error) {
	if len(names) == 0 {
		return nil, nil, fmt.Errorf("at least one recipient is required")
	}
	var addrs []string
	var resolutions []resolvedRecipient
	for _, name := range names {
		addr, resolution, err := s.idCfg.Resolve(name)
		if err != nil {
			return nil, nil, err
		}
		addrs = append(addrs, addr)
		resolutions = append(resolutions, resolvedRecipient{Address: addr, Resolution: resolution})
	}
	return addrs, resolutions, nil
}

// sendOne drafts and sends a single message: temp-file body, DraftCreate,
// full-restating UpdateFull (webapi PUT is full-replacement), DraftSend.
// A failure after draft creation deletes the draft before returning.
func (s *server) sendOne(ctx context.Context, recipients []string, body, topic string, pid int64) (int64, error) {
	dir, err := os.MkdirTemp("", "fmsg-mcp-send-*")
	if err != nil {
		return 0, err
	}
	defer os.RemoveAll(dir)
	bodyPath := filepath.Join(dir, "body.md")
	if err := os.WriteFile(bodyPath, []byte(body), 0o600); err != nil {
		return 0, err
	}
	id, err := s.runner.DraftCreate(ctx, recipients[0], bodyPath, topic, pid)
	if err != nil {
		return 0, err
	}
	cleanup := func(cause error) error {
		if derr := s.runner.Del(ctx, id); derr != nil {
			return fmt.Errorf("%w (and draft %d cleanup failed: %v)", cause, id, derr)
		}
		return cause
	}
	// The update must restate every field: webapi PUT is full-replacement,
	// so a type-only update would wipe recipients, pid, and topic.
	if err := s.runner.UpdateFull(ctx, id, bodyPath, recipients, "text/markdown", topic, pid); err != nil {
		return 0, cleanup(err)
	}
	if err := s.runner.DraftSend(ctx, id); err != nil {
		return 0, cleanup(err)
	}
	return id, nil
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
	Recipients    []string         `json:"recipients,omitempty" jsonschema:"recipient fmsg addresses (@user@example.com) or short names to resolve; list every address the thread should reach. Required on the preview call; not needed when confirming with confirm_token"`
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
	recipients, resolutions, err := s.resolveRecipients(args.Recipients)
	if err != nil {
		return nil, nil, err
	}

	tr, err := s.buildTranscript(args, who)
	if err != nil {
		return nil, nil, err
	}
	hits := session.Redact(tr.Turns)
	// contents (header-free) is the hash basis for incremental matching; the
	// first body's provenance header changes with every share.
	bodies, contents := session.RenderExchanges(tr)

	// Incremental share: when this session was already shared to the same
	// audience and its content still extends what was sent, chain only the
	// new exchanges onto the existing thread instead of resending everything.
	mode := "new_thread"
	sendBodies := bodies
	allHashes := sharestate.HashBodies(contents)
	baseCount := 0
	replyTo := args.ReplyToFmsgID
	var threadRoot int64
	if replyTo == 0 && tr.SessionID != "" {
		if st, serr := sharestate.Load(tr.SessionID, ""); serr == nil && st != nil {
			delta, extends := sharestate.Delta(st.ExchangeHashes, contents)
			switch {
			case st.FormatVersion != sharestate.CurrentFormat:
				// Stored hashes were computed over an older rendering format
				// and can never prefix-match — start a fresh thread rather
				// than mislabelling the mismatch as content divergence.
				mode = "new_thread_render_format_changed"
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
				baseCount = len(st.ExchangeHashes)
				// delta is header-free content; send the real bodies for
				// the same exchanges (non-first bodies carry no header).
				sendBodies = bodies[baseCount:]
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

	// Send one message per exchange, each pid-linked to the previous, so the
	// fmsg thread mirrors the conversation. Sent messages cannot be recalled,
	// so a mid-chain failure reports what did go out.
	var sent []int64
	prev := p.replyTo
	for i, body := range p.bodies {
		topic := ""
		if i == 0 && p.replyTo == 0 {
			topic = orDefault(p.title, "Claude session")
		}
		id, err := s.sendOne(ctx, p.recipients, body, topic, prev)
		if err != nil {
			return s.sharePartial(sent, err)
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
				return s.sharePartial(sent, fmt.Errorf(
					"message %d not yet delivered to %v after 60s; stopping so later messages aren't rejected as parent-not-found — once delivery_status shows it delivered, continue the chain with reply_to_fmsg_id=%d",
					id, pending, id))
			}
		}
	}

	note := fmt.Sprintf("one message per prompt, pid-chained; recipients can branch from or resume up to any of them (continue_thread %d for the whole session)", sent[len(sent)-1])
	if p.threadRoot != 0 {
		note = fmt.Sprintf("continued the previously shared thread (root %d): only the %d new message(s) were sent; sharing again later will keep extending this thread", p.threadRoot, len(sent))
	}
	if p.kind == sharestate.KindSummary {
		note = "summary sent as a single message; summarising this session again later threads an updated summary as a reply"
		if p.threadRoot != 0 {
			note = fmt.Sprintf("updated summary sent as a reply into the existing summary thread (root %d)", p.threadRoot)
		}
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
		SessionID:  p.sessionID,
		Kind:       p.kind,
		ThreadRoot: root,
		LastFmsgID: sent[len(sent)-1],
		Recipients: p.recipients,
	}
	if p.kind == sharestate.KindSummary {
		st.SummaryCount = p.summaryCount + 1
	} else {
		st.ExchangeHashes = p.allHashes[:p.baseCount+len(sent)]
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
// stay sent (the failed draft was already cleaned up by sendOne).
func (s *server) sharePartial(sent []int64, cause error) (*mcp.CallToolResult, any, error) {
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

	id, err := s.sendOne(ctx, recipients, args.Body, "", args.FmsgID)
	if err != nil {
		return nil, nil, err
	}
	result := map[string]any{"status": "sent", "fmsg_id": id, "recipients": recipients}
	if delivery, derr := s.deliverySnapshot(ctx, id); derr == nil {
		result["delivery"] = delivery
	}
	return jsonResult(result)
}

// ---------------------------------------------------------------- summary

type summaryArgs struct {
	Recipients   []string `json:"recipients,omitempty" jsonschema:"recipient fmsg addresses (@user@example.com) or short names to resolve. Required on the preview call; not needed when confirming with confirm_token"`
	Summary      string   `json:"summary,omitempty" jsonschema:"the summary you have written of this session, in Markdown. Required on the preview call"`
	Title        string   `json:"title,omitempty" jsonschema:"thread topic; defaults to the session's own summary line. On a root share this becomes the immutable fmsg topic"`
	SessionID    string   `json:"session_id,omitempty" jsonschema:"Claude Code session id, if known, to disambiguate parallel sessions"`
	ConfirmToken string   `json:"confirm_token,omitempty" jsonschema:"token from the phase-1 preview; presence triggers the actual send"`
}

func (s *server) shareSummary(ctx context.Context, req *mcp.CallToolRequest, args summaryArgs) (*mcp.CallToolResult, any, error) {
	if err := s.ensureCLI(ctx); err != nil {
		return nil, nil, err
	}
	if args.ConfirmToken != "" {
		return s.shareConfirm(ctx, args.ConfirmToken)
	}
	return s.summaryPreview(ctx, args)
}

func (s *server) summaryPreview(ctx context.Context, args summaryArgs) (*mcp.CallToolResult, any, error) {
	if strings.TrimSpace(args.Summary) == "" {
		return nil, nil, fmt.Errorf("summary is empty: write a concise Markdown summary of this session (goal, work done, key decisions, current state, next steps) and pass it as the summary argument")
	}
	who, err := s.resolveIdentity(ctx)
	if err != nil {
		return nil, nil, err
	}
	recipients, resolutions, err := s.resolveRecipients(args.Recipients)
	if err != nil {
		return nil, nil, err
	}

	// Best-effort session lookup: threading, turn count and default title all
	// come from it, but a summary can still be shared when the session file
	// isn't locatable (claude.ai/Desktop) — just stateless, with no turn
	// count in the header.
	surface := "claude-ai"
	sid := args.SessionID
	turnCount := 0
	title := args.Title
	if home, herr := os.UserHomeDir(); herr == nil {
		if projectDir, werr := os.Getwd(); werr == nil { // assumption A1: stdio server inherits the project CWD
			if path, locSID, _, lerr := locator.Locate(home, projectDir, args.SessionID); lerr == nil {
				if pt, perr := session.ParseJSONL(path); perr == nil {
					surface = "claude-code"
					sid = locSID
					turnCount = len(pt.Turns)
					if title == "" {
						title = clipTitle(pt.Summary)
					}
					if title == "" {
						title = deriveTitle(pt.Turns)
					}
				}
			}
		}
	}
	if title == "" {
		title = "Claude session summary"
	}

	summary, hits := session.RedactText(args.Summary)
	title, titleHits := session.RedactText(title)
	hits = mergeHits(hits, titleHits)

	mode := "new_summary_thread"
	var replyTo, threadRoot int64
	summaryCount := 0
	if sid != "" {
		if st, serr := sharestate.Load(sid, sharestate.KindSummary); serr == nil && st != nil {
			switch {
			case st.FormatVersion != sharestate.CurrentFormat:
				mode = "new_summary_thread_format_changed"
			case !sharestate.SameRecipients(st.Recipients, recipients):
				mode = "new_summary_thread_different_audience"
			default:
				mode = "continue_summary_thread"
				replyTo = st.LastFmsgID
				threadRoot = st.ThreadRoot
				summaryCount = st.SummaryCount
			}
		}
	}

	body := session.RenderSummary(session.SummaryMeta{
		Title: title, SharerAddress: who.Address, Surface: surface,
		SharedAt: float64(time.Now().Unix()), TurnCount: turnCount,
		FollowUp: replyTo != 0,
	}, summary)

	token := "st_" + randomHex(16)
	s.mu.Lock()
	for t, p := range s.pending { // expire stale previews
		if time.Since(p.created) > 15*time.Minute {
			delete(s.pending, t)
		}
	}
	s.pending[token] = &pendingShare{
		kind: sharestate.KindSummary, bodies: []string{body}, title: title,
		recipients: recipients, replyTo: replyTo, created: time.Now(),
		sessionID: sid, threadRoot: threadRoot, summaryCount: summaryCount,
	}
	s.mu.Unlock()

	preview := map[string]any{
		"status":        "needs_confirmation",
		"mode":          mode,
		"from":          who.Address,
		"recipients":    resolutions,
		"title":         title,
		"messages":      1,
		"total_bytes":   len(body),
		"redactions":    hits,
		"confirm":       "Present this preview to the user as a short multi-line list (from, recipients, title, size, any redactions) — then on its own line ask exactly: \"Are you sure?\" No extra warnings. Re-invoke with confirm_token only after they say yes.",
		"confirm_token": token,
	}
	if turnCount > 0 {
		preview["turns_summarised"] = turnCount
	}
	if mode == "continue_summary_thread" {
		preview["continuing_thread_root"] = threadRoot
		preview["note"] = "this session was summarised before; the updated summary will be sent as a reply into that thread"
	}
	if len(body) > 9728 {
		preview["size_warning"] = fmt.Sprintf("rendered summary is %d bytes — near or over the 10 KiB fmsg federation default message size; a cross-host recipient may bounce it as too big (delivery code 4). Consider a shorter summary.", len(body))
	}
	return jsonResult(preview)
}

// mergeHits unions two sorted redaction-hit lists.
func mergeHits(a, b []string) []string {
	if len(b) == 0 {
		return a
	}
	seen := map[string]bool{}
	for _, h := range a {
		seen[h] = true
	}
	for _, h := range b {
		if !seen[h] {
			a = append(a, h)
			seen[h] = true
		}
	}
	sort.Strings(a)
	return a
}

// ---------------------------------------------------------------- send

type sendArgs struct {
	Recipients []string `json:"recipients" jsonschema:"recipient fmsg addresses (@user@example.com) or short names to resolve"`
	Body       string   `json:"body" jsonschema:"the Markdown message body to send"`
	Topic      string   `json:"topic,omitempty" jsonschema:"thread topic; defaults to the body's first line"`
}

// sendMessage sends a standalone message immediately — the agreed exception
// to the two-phase preview/confirm (STATUS.md decision 11): the user's own
// prompt is the intent. Redaction still applies, reported in the result.
func (s *server) sendMessage(ctx context.Context, req *mcp.CallToolRequest, args sendArgs) (*mcp.CallToolResult, any, error) {
	if err := s.ensureCLI(ctx); err != nil {
		return nil, nil, err
	}
	if strings.TrimSpace(args.Body) == "" {
		return nil, nil, fmt.Errorf("message body is empty")
	}
	recipients, resolutions, err := s.resolveRecipients(args.Recipients)
	if err != nil {
		return nil, nil, err
	}
	body, hits := session.RedactText(args.Body)
	topic, _ := session.RedactText(clipTitle(orDefault(args.Topic, body)))

	id, err := s.sendOne(ctx, recipients, body, topic, 0)
	if err != nil {
		return nil, nil, err
	}
	result := map[string]any{
		"status": "sent", "fmsg_id": id,
		"recipients": resolutions, "redactions": hits,
	}
	if len(hits) > 0 {
		result["note"] = "secrets matching the listed patterns were redacted from the body before sending"
	}
	if len(body) > 10<<10 {
		result["size_note"] = fmt.Sprintf("body is %d bytes — over the 10 KiB fmsg federation default; a cross-host recipient may bounce it as too big (delivery code 4)", len(body))
	}
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
		Name:        "share_summary",
		Title:       "Share a summary of this session via fmsg",
		Description: "Summarise the current Claude session and share it as a single fmsg message.",
		Arguments: []*mcp.PromptArgument{
			{Name: "recipients", Description: "recipient fmsg address(es), comma-separated", Required: true},
		},
	}, func(ctx context.Context, req *mcp.GetPromptRequest) (*mcp.GetPromptResult, error) {
		recipients := req.Params.Arguments["recipients"]
		text := fmt.Sprintf("Write a concise Markdown summary of this session (goal, work done, key decisions and why, "+
			"current state, next steps), then call the share_summary tool to send it to %q. Show me its preview as a "+
			"short multi-line list (from, recipients, title, size, redactions), then ask me simply \"Are you sure?\", "+
			"and only after I say yes call share_summary again with the confirm_token.", recipients)
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

func boolHint(b bool) *bool { return &b }

// withTitle copies the shared annotation shape with a per-tool title.
func withTitle(base *mcp.ToolAnnotations, title string) *mcp.ToolAnnotations {
	a := *base
	a.Title = title
	return &a
}

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
