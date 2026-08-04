package thread

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/markmnl/fmsg-mcp-claude/internal/cli"
)

// fake implements Client over an in-memory thread graph.
type fake struct {
	msgs    map[int64]*cli.Message
	bodies  map[int64]string
	getLogs []int64
}

func (f *fake) Get(_ context.Context, id int64) (*cli.Message, error) {
	f.getLogs = append(f.getLogs, id)
	m, ok := f.msgs[id]
	if !ok {
		return nil, fmt.Errorf("not found: %d", id)
	}
	return m, nil
}

func (f *fake) GetData(_ context.Context, id int64) ([]byte, error) {
	return []byte(f.bodies[id]), nil
}

func pid(v int64) *int64 { return &v }

// buildFake constructs: share 10 ← reply 11 ← reply 12 (target); sibling 99
// also points at 10 and must be excluded.
func buildFake() *fake {
	return &fake{
		msgs: map[int64]*cli.Message{
			10: {From: "@alice@example.com", To: []string{"@bob@example.com"}, Topic: "Fix auth"},
			11: {From: "@bob@example.com", PID: pid(10)},
			12: {From: "@alice@example.com", PID: pid(11)},
			99: {From: "@carol@example.com", PID: pid(10)},
		},
		bodies: map[int64]string{
			10: "# Fix auth\n**User:** the transcript",
			11: "bob's phone reply",
			12: "alice's follow-up",
		},
	}
}

func TestWalkExcludesSiblings(t *testing.T) {
	f := buildFake()
	chain, err := Walk(context.Background(), f, 12)
	if err != nil {
		t.Fatal(err)
	}
	got := []int64{}
	for _, n := range chain {
		got = append(got, n.ID)
	}
	if fmt.Sprint(got) != fmt.Sprint([]int64{10, 11, 12}) {
		t.Fatalf("chain: got %v", got)
	}
	for _, id := range f.getLogs {
		if id == 99 {
			t.Fatal("sibling branch 99 must never be fetched")
		}
	}
}

func TestAssembleOrdersRootFirst(t *testing.T) {
	f := buildFake()
	asm, err := Assemble(context.Background(), f, 12)
	if err != nil {
		t.Fatal(err)
	}
	iTranscript := strings.Index(asm.Context, "the transcript")
	iBob := strings.Index(asm.Context, "bob's phone reply")
	iAlice := strings.Index(asm.Context, "alice's follow-up")
	if iTranscript < 0 || iBob < 0 || iAlice < 0 || !(iTranscript < iBob && iBob < iAlice) {
		t.Fatalf("bodies missing or out of order: %d %d %d", iTranscript, iBob, iAlice)
	}
	for _, want := range []string{"Topic: Fix auth", "conversation data", "reply_to_thread", "from @bob@example.com"} {
		if !strings.Contains(asm.Context, want) {
			t.Fatalf("missing %q", want)
		}
	}
	if asm.TargetID != 12 {
		t.Fatalf("target: %d", asm.TargetID)
	}
}

func TestWalkHopCap(t *testing.T) {
	f := &fake{msgs: map[int64]*cli.Message{}, bodies: map[int64]string{}}
	for i := int64(0); i < 200; i++ {
		m := &cli.Message{From: "@a@example.com"}
		if i > 0 {
			m.PID = pid(i - 1)
		}
		f.msgs[i] = m
	}
	if _, err := Walk(context.Background(), f, 199); err == nil {
		t.Fatal("expected hop-cap error")
	}
}
