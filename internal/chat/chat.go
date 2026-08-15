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
}

// Hit is a qualifying message plus what the model needs to reply.
type Hit struct {
	ID         int64
	Msg        *cli.Message
	ThreadRoot int64
	Body       string
	Context    string // thread.Assemble block for ID
	Pending    int    // further qualifying messages already waiting after this one
}

// PollInterval is the list cadence when watch is unavailable.
var PollInterval = 2 * time.Second

// ErrTimeout reports that nothing qualifying arrived before the deadline.
var ErrTimeout = errors.New("no qualifying message before deadline")

// Wait blocks until a message matching f arrives (returning the OLDEST
// qualifying one so bursts are handled in order), the deadline passes
// (ErrTimeout), or ctx ends. usedWatch reports whether the WebSocket path was
// available, for the tool result.
func Wait(ctx context.Context, c Client, f Filter, deadline time.Time) (hit *Hit, usedWatch bool, err error) {
	ctx, cancel := context.WithDeadline(ctx, deadline)
	defer cancel()

	m := &matcher{c: c, f: f, rootCache: map[int64]int64{}}
	if f.ThreadOf != 0 {
		root, err := m.rootOf(ctx, f.ThreadOf)
		if err != nil {
			return nil, false, err
		}
		m.wantRoot = root
	}

	events, werr := c.Watch(ctx, cli.EventNewMsg)
	switch {
	case werr == nil:
		usedWatch = true
	case errors.Is(werr, cli.ErrNoWatch):
		usedWatch = false
	default:
		return nil, false, werr
	}

	// Anything already in the inbox beyond After qualifies immediately (and
	// covers the gap between the caller's last look and the socket opening).
	if hit, err := m.catchUp(ctx); err != nil || hit != nil {
		return hit, usedWatch, err
	}

	if !usedWatch {
		return m.pollLoop(ctx)
	}

	for {
		select {
		case <-ctx.Done():
			return nil, true, timeoutOr(ctx)
		case ev, ok := <-events:
			if !ok { // watch process died: keep going by polling
				return m.pollLoop(ctx)
			}
			switch ev.Type {
			case cli.EventReady: // (re)connected: events may have been missed
				if hit, err := m.catchUp(ctx); err != nil || hit != nil {
					return hit, true, err
				}
			case cli.EventNewMsg:
				// A pushed event says something new exists; re-list so a
				// burst is still answered oldest-first and Pending is right.
				if it, err := ev.Item(); err == nil && it.ID > m.f.After {
					if hit, err := m.catchUp(ctx); err != nil || hit != nil {
						return hit, true, err
					}
				}
			}
		}
	}
}

// pollLoop is the no-watch path: list every PollInterval until a hit or the
// deadline. usedWatch is always false here.
func (m *matcher) pollLoop(ctx context.Context) (*Hit, bool, error) {
	if hit, err := m.catchUp(ctx); err != nil || hit != nil {
		return hit, false, err
	}
	tick := time.NewTicker(PollInterval)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil, false, timeoutOr(ctx)
		case <-tick.C:
			if hit, err := m.catchUp(ctx); err != nil || hit != nil {
				return hit, false, err
			}
		}
	}
}

func timeoutOr(ctx context.Context) error {
	if errors.Is(ctx.Err(), context.DeadlineExceeded) {
		return ErrTimeout
	}
	return ctx.Err()
}

// matcher applies Filter against list items, caching thread roots.
type matcher struct {
	c         Client
	f         Filter
	wantRoot  int64
	rootCache map[int64]int64
}

// catchUp lists the inbox and returns the oldest qualifying message newer
// than After, or nil.
func (m *matcher) catchUp(ctx context.Context) (*Hit, error) {
	items, err := m.c.List(ctx, 20)
	if err != nil {
		return nil, err
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
			return nil, err
		}
		if ok {
			qualifying = append(qualifying, it)
		}
	}
	if len(qualifying) == 0 {
		return nil, nil
	}
	sort.Slice(qualifying, func(i, j int) bool { return qualifying[i].ID < qualifying[j].ID })
	oldest := qualifying[0]
	asm, err := thread.Assemble(ctx, m.c, oldest.ID)
	if err != nil {
		return nil, err
	}
	body, err := m.c.GetData(ctx, oldest.ID)
	if err != nil {
		return nil, err
	}
	root, _ := m.rootOf(ctx, oldest.ID)
	msg := oldest.Message
	return &Hit{
		ID: oldest.ID, Msg: &msg, ThreadRoot: root,
		Body: strings.TrimSpace(string(body)), Context: asm.Context,
		Pending: len(qualifying) - 1,
	}, nil
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
			return false, nil // a root can't be on an existing thread
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
