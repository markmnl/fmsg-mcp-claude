// Package chat implements "wait for the next inbound message": the core of
// chat mode. The server has no LLM — the model composes every reply — so
// this package only decides WHICH message qualifies (after a given id, not
// from ourselves, not no_reply, optionally from one sender or on one thread)
// and blocks until one arrives or the deadline passes. Arrival comes from
// `fmsg watch` (WebSocket) with a `list` catch-up after every (re)connect;
// CLI builds without watch fall back to polling `list`.
package chat

import (
	"context"
	"errors"
	"sort"
	"strings"
	"time"

	"github.com/markmnl/fmsg-mcp-claude/internal/cli"
	"github.com/markmnl/fmsg-mcp-claude/internal/thread"
)

// Client is the slice of the fmsg-cli runner this package needs; *cli.Runner
// satisfies it and tests substitute a fake.
type Client interface {
	thread.Client
	List(ctx context.Context, limit int) ([]cli.ListItem, error)
	Watch(ctx context.Context, events ...string) (<-chan cli.WatchEvent, error)
}

// Filter selects which inbound messages qualify.
type Filter struct {
	After    int64  // only ids strictly greater than this
	Self     string // our own address: never reply to ourselves
	From     string // optional: only this sender
	ThreadOf int64  // optional: only messages whose ancestor chain reaches this message's root

	// Settle is the quiet window after the first qualifying message: while
	// further qualifying messages keep arriving on the same thread within
	// Settle of each other they are batched into one Hit, so a sender who
	// splits a thought over several messages gets ONE reply covering all of
	// them. Zero disables batching (return on the first message).
	Settle time.Duration
	// MaxBatch caps a batch (default DefaultMaxBatch) so a runaway sender
	// cannot hold the call open until the deadline.
	MaxBatch int
}

// DefaultMaxBatch is the batch cap when Filter.MaxBatch is zero.
const DefaultMaxBatch = 20

// Message is one inbound message in a batch.
type Message struct {
	ID   int64
	Msg  *cli.Message
	Body string
}

// Hit is a batch of qualifying messages on one thread, oldest first, plus
// what the model needs to reply once to all of them.
type Hit struct {
	Messages   []Message
	Newest     int64  // reply target (pid) — keeps the chain linear
	ThreadRoot int64  // root of the thread the batch is on
	Context    string // thread.Assemble block for Newest (its lineage covers the batch)
	Pending    int    // qualifying messages on OTHER threads, still waiting (reply-once mode)
	Settled    bool   // false when the deadline cut the settle window short
}

// PollInterval is the list cadence when watch is unavailable.
var PollInterval = 2 * time.Second

// ErrTimeout reports that nothing qualifying arrived before the deadline.
var ErrTimeout = errors.New("no qualifying message before deadline")

// Wait blocks until at least one message matching f has arrived and the
// settle window has passed (or the batch cap / deadline is reached), then
// returns the batch — the OLDEST thread's messages when several threads
// qualify. ErrTimeout if nothing arrived by the deadline. usedWatch reports
// whether the WebSocket path was available, for the tool result.
func Wait(ctx context.Context, c Client, f Filter, deadline time.Time) (hit *Hit, usedWatch bool, err error) {
	ctx, cancel := context.WithDeadline(ctx, deadline)
	defer cancel()

	if f.MaxBatch <= 0 {
		f.MaxBatch = DefaultMaxBatch
	}
	m := &matcher{c: c, f: f, rootCache: map[int64]int64{}}
	if f.ThreadOf != 0 {
		root, err := m.rootOf(ctx, f.ThreadOf)
		if err != nil {
			return nil, false, err
		}
		m.wantRoot = root
	}

	src := &source{}
	defer src.stop()
	events, werr := c.Watch(ctx, cli.EventNewMsg)
	switch {
	case werr == nil:
		src.events = events
		usedWatch = true
	case errors.Is(werr, cli.ErrNoWatch):
		usedWatch = false
	default:
		return nil, false, werr
	}

	// Phase 1: wait for the first qualifying message. The initial catch-up
	// covers anything that arrived between the caller's last look and the
	// socket opening; every trigger (push event, reconnect, poll tick)
	// re-lists.
	batch, rest, err := m.catchUp(ctx)
	if err != nil {
		return nil, usedWatch, err
	}
	for len(batch) == 0 {
		if !src.next(ctx, nil) {
			return nil, usedWatch, timeoutOr(ctx)
		}
		if batch, rest, err = m.catchUp(ctx); err != nil {
			return nil, usedWatch, err
		}
	}

	// Phase 2: settle — keep gathering while the batch keeps growing.
	settled := true
	if f.Settle > 0 {
		lastGrowth := time.Now()
		for len(batch) < f.MaxBatch {
			remaining := f.Settle - time.Since(lastGrowth)
			if remaining <= 0 {
				break
			}
			timer := time.NewTimer(remaining)
			fired := src.next(ctx, timer.C)
			timer.Stop()
			if ctx.Err() != nil {
				settled = false // deadline hit mid-window: return what we have
				break
			}
			if !fired {
				continue // timer expired without a new arrival → check again (breaks)
			}
			nb, nr, err := m.catchUp(ctx)
			if err != nil {
				return nil, usedWatch, err
			}
			if len(nb) > len(batch) {
				lastGrowth = time.Now()
			}
			batch, rest = nb, nr
		}
	}
	// Bodies + context. Use a fresh context: the deadline may have passed.
	bctx, bcancel := context.WithTimeout(context.WithoutCancel(ctx), 60*time.Second)
	defer bcancel()
	hit, err = m.build(bctx, batch, rest)
	if err != nil {
		return nil, usedWatch, err
	}
	hit.Settled = settled
	return hit, src.events != nil, nil // false if watch died and we finished by polling
}

func timeoutOr(ctx context.Context) error {
	if errors.Is(ctx.Err(), context.DeadlineExceeded) {
		return ErrTimeout
	}
	return ctx.Err()
}

// source is the "something may have changed" trigger: the watch stream when
// available (any event — new_msg, or ready after a reconnect — is a reason
// to re-list), else a poll ticker. If the watch stream closes it degrades to
// polling for the rest of the call.
type source struct {
	events <-chan cli.WatchEvent
	tick   *time.Ticker
}

func (s *source) stop() {
	if s.tick != nil {
		s.tick.Stop()
	}
}

// next blocks until a trigger fires (returns true), the optional timer
// fires (returns false), or ctx ends (returns false).
func (s *source) next(ctx context.Context, timer <-chan time.Time) bool {
	for {
		if s.events == nil && s.tick == nil {
			s.tick = time.NewTicker(PollInterval)
		}
		var tickC <-chan time.Time
		if s.tick != nil {
			tickC = s.tick.C
		}
		select {
		case <-ctx.Done():
			return false
		case <-timer:
			return false
		case <-tickC:
			return true
		case _, ok := <-s.events:
			if !ok { // watch process died: fall back to polling
				s.events = nil
				continue
			}
			return true
		}
	}
}

// matcher applies Filter against list items, caching thread roots.
type matcher struct {
	c         Client
	f         Filter
	wantRoot  int64
	rootCache map[int64]int64
}

// catchUp lists the inbox and returns the qualifying messages newer than
// After on the OLDEST qualifying thread (oldest first), plus how many
// qualifying messages sit on other threads.
func (m *matcher) catchUp(ctx context.Context) (batch []cli.ListItem, rest int, err error) {
	items, err := m.c.List(ctx, 20)
	if err != nil {
		return nil, 0, err
	}
	if len(items) == 20 && items[len(items)-1].ID > m.f.After {
		// Every one of the newest 20 is unseen: widen once. Beyond 100 the
		// oldest unseen are simply not considered.
		if more, err := m.c.List(ctx, 100); err == nil {
			items = more
		}
	}
	var qualifying []cli.ListItem
	for _, it := range items {
		ok, err := m.qualifies(ctx, it)
		if err != nil {
			return nil, 0, err
		}
		if ok {
			qualifying = append(qualifying, it)
		}
	}
	if len(qualifying) == 0 {
		return nil, 0, nil
	}
	sort.Slice(qualifying, func(i, j int) bool { return qualifying[i].ID < qualifying[j].ID })
	// Group by thread: the batch is the oldest message's thread. With
	// ThreadOf set every qualifying message is on that thread already.
	root, err := m.rootOf(ctx, qualifying[0].ID)
	if err != nil {
		root = qualifying[0].ID
	}
	for _, it := range qualifying {
		r, err := m.rootOf(ctx, it.ID)
		if err == nil && r == root && len(batch) < m.f.MaxBatch {
			batch = append(batch, it)
		} else {
			rest++
		}
	}
	return batch, rest, nil
}

// build fetches bodies and the newest message's lineage context.
func (m *matcher) build(ctx context.Context, batch []cli.ListItem, rest int) (*Hit, error) {
	newest := batch[len(batch)-1]
	asm, err := thread.Assemble(ctx, m.c, newest.ID)
	if err != nil {
		return nil, err
	}
	root, _ := m.rootOf(ctx, newest.ID)
	hit := &Hit{Newest: newest.ID, ThreadRoot: root, Context: asm.Context, Pending: rest}
	for _, it := range batch {
		body, err := m.c.GetData(ctx, it.ID)
		if err != nil {
			return nil, err
		}
		msg := it.Message
		hit.Messages = append(hit.Messages, Message{ID: it.ID, Msg: &msg, Body: strings.TrimSpace(string(body))})
	}
	return hit, nil
}

func (m *matcher) qualifies(ctx context.Context, it cli.ListItem) (bool, error) {
	if it.ID <= m.f.After || it.NoReply {
		return false, nil
	}
	if m.f.Self != "" && strings.EqualFold(it.From, m.f.Self) {
		return false, nil
	}
	if m.f.From != "" && !strings.EqualFold(it.From, m.f.From) {
		return false, nil
	}
	if m.f.ThreadOf != 0 {
		if it.PID == nil {
			return it.ID == m.wantRoot, nil // a root is only on its own thread
		}
		root, err := m.rootOf(ctx, it.ID)
		if err != nil {
			return false, nil // unreadable lineage: not provably on the thread
		}
		if root != m.wantRoot {
			return false, nil
		}
	}
	return true, nil
}

// rootOf walks id's pid chain to the root, caching results.
func (m *matcher) rootOf(ctx context.Context, id int64) (int64, error) {
	if r, ok := m.rootCache[id]; ok {
		return r, nil
	}
	chain, err := thread.Walk(ctx, m.c, id)
	if err != nil {
		return 0, err
	}
	root := chain[0].ID
	for _, n := range chain {
		m.rootCache[n.ID] = root
	}
	return root, nil
}
