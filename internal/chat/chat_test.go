package chat

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/markmnl/fmsg-mcp-claude/internal/cli"
)

func pid(v int64) *int64 { return &v }

// fake is an in-memory inbox + thread graph with a scriptable watch stream.
type fake struct {
	mu     sync.Mutex
	msgs   map[int64]*cli.Message
	inbox  []int64 // ids visible to List (any order)
	noWtch bool    // simulate a CLI without watch
	events chan cli.WatchEvent
	lists  int
}

func newFake() *fake {
	f := &fake{msgs: map[int64]*cli.Message{}, events: make(chan cli.WatchEvent, 16)}
	// Thread A: 10 (bob→me) ← 11 (me→bob) ← 12 (bob→me).  Thread B: 20 (carol→me).
	f.add(10, "@bob@x", nil)
	f.add(11, "@me@x", pid(10))
	f.add(12, "@bob@x", pid(11))
	f.add(20, "@carol@x", nil)
	return f
}

func (f *fake) add(id int64, from string, p *int64) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.msgs[id] = &cli.Message{From: from, To: []string{"@me@x"}, PID: p, Topic: fmt.Sprintf("t%d", id)}
	f.inbox = append(f.inbox, id)
}

func (f *fake) Get(_ context.Context, id int64) (*cli.Message, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	m, ok := f.msgs[id]
	if !ok {
		return nil, fmt.Errorf("not found %d", id)
	}
	return m, nil
}
func (f *fake) GetData(_ context.Context, id int64) ([]byte, error) {
	return []byte(fmt.Sprintf("body of %d", id)), nil
}
func (f *fake) List(_ context.Context, limit int) ([]cli.ListItem, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.lists++
	var out []cli.ListItem
	for i := len(f.inbox) - 1; i >= 0 && len(out) < limit; i-- { // newest first
		id := f.inbox[i]
		out = append(out, cli.ListItem{ID: id, Message: *f.msgs[id]})
	}
	return out, nil
}
func (f *fake) Watch(ctx context.Context, events ...string) (<-chan cli.WatchEvent, error) {
	if f.noWtch {
		return nil, cli.ErrNoWatch
	}
	return f.events, nil
}

func (f *fake) push(id int64) {
	f.events <- cli.WatchEvent{Type: cli.EventNewMsg, Data: []byte(fmt.Sprintf(`{"id":%d}`, id))}
}

func wait(t *testing.T, f *fake, flt Filter, d time.Duration) (*Hit, bool, error) {
	t.Helper()
	flt.Self = "@me@x"
	return Wait(context.Background(), f, flt, time.Now().Add(d))
}

func TestCatchUpReturnsOldestQualifyingAndPending(t *testing.T) {
	f := newFake()
	hit, usedWatch, err := wait(t, f, Filter{After: 9}, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if !usedWatch {
		t.Error("expected watch path")
	}
	// 10, 12, 20 qualify (11 is ours); oldest is 10, two more pending.
	if hit.ID != 10 || hit.Pending != 2 || hit.ThreadRoot != 10 || hit.Body != "body of 10" {
		t.Fatalf("hit = %+v", hit)
	}
	if hit.Context == "" {
		t.Error("expected assembled context")
	}
}

func TestThreadFilterExcludesOtherThreadsAndRoots(t *testing.T) {
	f := newFake()
	hit, _, err := wait(t, f, Filter{After: 11, ThreadOf: 10}, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if hit.ID != 12 || hit.Pending != 0 {
		t.Fatalf("hit = %+v (20 is another thread and must be excluded)", hit)
	}
	// ThreadOf may be any message on the thread, not just the root.
	hit, _, err = wait(t, f, Filter{After: 11, ThreadOf: 11}, time.Second)
	if err != nil || hit.ID != 12 {
		t.Fatalf("hit = %+v, err = %v", hit, err)
	}
}

func TestFromFilterAndNoReplyAndSelf(t *testing.T) {
	f := newFake()
	hit, _, err := wait(t, f, Filter{After: 9, From: "@CAROL@x"}, time.Second)
	if err != nil || hit.ID != 20 {
		t.Fatalf("hit = %+v err = %v", hit, err)
	}
	f.msgs[20].NoReply = true
	_, _, err = wait(t, f, Filter{After: 12}, 300*time.Millisecond)
	if !errors.Is(err, ErrTimeout) {
		t.Fatalf("no_reply message must not qualify; err = %v", err)
	}
	f.add(30, "@ME@x", nil)
	_, _, err = wait(t, f, Filter{After: 20}, 300*time.Millisecond)
	if !errors.Is(err, ErrTimeout) {
		t.Fatalf("own message must not qualify; err = %v", err)
	}
}

func TestWatchEventTriggersHit(t *testing.T) {
	f := newFake()
	go func() {
		time.Sleep(150 * time.Millisecond)
		f.add(30, "@bob@x", pid(12))
		f.push(30)
	}()
	start := time.Now()
	hit, usedWatch, err := wait(t, f, Filter{After: 20, ThreadOf: 10}, 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if !usedWatch || hit.ID != 30 || time.Since(start) > 2*time.Second {
		t.Fatalf("hit = %+v usedWatch=%v elapsed=%s", hit, usedWatch, time.Since(start))
	}
}

func TestReadyEventTriggersCatchUp(t *testing.T) {
	f := newFake()
	go func() {
		time.Sleep(100 * time.Millisecond)
		f.add(21, "@carol@x", pid(20)) // arrived while the socket was down
		f.events <- cli.WatchEvent{Type: cli.EventReady}
	}()
	hit, _, err := wait(t, f, Filter{After: 20}, 5*time.Second)
	if err != nil || hit.ID != 21 {
		t.Fatalf("hit = %+v err = %v", hit, err)
	}
}

func TestPollFallbackWithoutWatch(t *testing.T) {
	old := PollInterval
	PollInterval = 50 * time.Millisecond
	defer func() { PollInterval = old }()
	f := newFake()
	f.noWtch = true
	go func() {
		time.Sleep(120 * time.Millisecond)
		f.add(21, "@carol@x", pid(20))
	}()
	hit, usedWatch, err := wait(t, f, Filter{After: 20}, 5*time.Second)
	if err != nil || hit.ID != 21 || usedWatch {
		t.Fatalf("hit = %+v usedWatch=%v err = %v", hit, usedWatch, err)
	}
	if f.lists < 2 {
		t.Errorf("expected repeated list polls, got %d", f.lists)
	}
}

func TestWatchChannelCloseFallsBackToPolling(t *testing.T) {
	old := PollInterval
	PollInterval = 50 * time.Millisecond
	defer func() { PollInterval = old }()
	f := newFake()
	go func() {
		time.Sleep(50 * time.Millisecond)
		close(f.events)
		time.Sleep(100 * time.Millisecond)
		f.add(21, "@carol@x", pid(20))
	}()
	hit, usedWatch, err := wait(t, f, Filter{After: 20}, 5*time.Second)
	if err != nil || hit.ID != 21 || usedWatch {
		t.Fatalf("hit = %+v usedWatch=%v err = %v", hit, usedWatch, err)
	}
}

func TestTimeoutAndCancel(t *testing.T) {
	f := newFake()
	_, _, err := wait(t, f, Filter{After: 20}, 200*time.Millisecond)
	if !errors.Is(err, ErrTimeout) {
		t.Fatalf("err = %v, want ErrTimeout", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	go func() { time.Sleep(100 * time.Millisecond); cancel() }()
	_, _, err = Wait(ctx, f, Filter{After: 20, Self: "@me@x"}, time.Now().Add(5*time.Second))
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want Canceled", err)
	}
}
