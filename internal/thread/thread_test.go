package thread

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"testing"

	"github.com/markmnl/fmsg-mcp-claude/internal/cli"
	"github.com/markmnl/fmsg-mcp-claude/internal/session"
)

// fake implements Client over an in-memory thread graph.
type fake struct {
	msgs    map[int64]*cli.Message
	bodies  map[int64]string
	attach  map[int64]*session.Envelope
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

func (f *fake) GetAttach(_ context.Context, id int64, filename, outPath string) error {
	env, ok := f.attach[id]
	if !ok {
		return fmt.Errorf("no attachment on %d", id)
	}
	raw, err := json.Marshal(env)
	if err != nil {
		return err
	}
	return os.WriteFile(outPath, raw, 0o600)
}

func pid(v int64) *int64 { return &v }

// buildFake constructs: root share 10 (transcript) ← plain reply 11 ← share 12
// (transcript, incorporates 11) ← plain reply 13 (target). A sibling branch 99
// also points at 10 and must be excluded.
func buildFake() *fake {
	envRoot := &session.Envelope{Format: session.FormatName, FormatVersion: 1,
		Provenance: session.Provenance{Surface: "claude-code", Fidelity: "verbatim", SharerAddress: "@alice_claude@example.com"},
		Turns:      []session.Turn{{Role: "user", Blocks: []session.Block{{Type: "text", Text: "ORIGINAL"}}}},
	}
	envDeep := &session.Envelope{Format: session.FormatName, FormatVersion: 1,
		Provenance: session.Provenance{Surface: "claude-code", Fidelity: "verbatim", SharerAddress: "@alice_claude@example.com",
			BaseFmsgID: pid(10), IncorporatedFmsgIDs: []int64{11}},
		Turns: []session.Turn{{Role: "user", Blocks: []session.Block{{Type: "text", Text: "DEEP-SNAPSHOT"}}}},
	}
	att := []cli.Attachment{{Filename: session.AttachmentName, Size: 1}}
	return &fake{
		msgs: map[int64]*cli.Message{
			10: {From: "@alice_claude@example.com", To: []string{"@bob@example.com"}, Topic: "Fix auth", Attachments: att},
			11: {From: "@bob@example.com", PID: pid(10)},
			12: {From: "@alice_claude@example.com", PID: pid(11), Attachments: att},
			13: {From: "@bob@example.com", PID: pid(12)},
			99: {From: "@carol@example.com", PID: pid(10)},
		},
		bodies: map[int64]string{11: "bob's phone reply", 13: "bob's latest reply"},
		attach: map[int64]*session.Envelope{10: envRoot, 12: envDeep},
	}
}

func TestWalkExcludesSiblings(t *testing.T) {
	f := buildFake()
	chain, err := Walk(context.Background(), f, 13)
	if err != nil {
		t.Fatal(err)
	}
	got := []int64{}
	for _, n := range chain {
		got = append(got, n.ID)
	}
	want := []int64{10, 11, 12, 13}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("chain: got %v want %v", got, want)
	}
	for _, id := range f.getLogs {
		if id == 99 {
			t.Fatal("sibling branch 99 must never be fetched")
		}
	}
}

func TestAssembleUsesDeepestBase(t *testing.T) {
	f := buildFake()
	asm, err := Assemble(context.Background(), f, 13)
	if err != nil {
		t.Fatal(err)
	}
	if asm.BaseID == nil || *asm.BaseID != 12 {
		t.Fatalf("base: got %v want 12", asm.BaseID)
	}
	if !strings.Contains(asm.Context, "DEEP-SNAPSHOT") {
		t.Fatal("deepest snapshot missing from context")
	}
	if strings.Contains(asm.Context, "ORIGINAL") {
		t.Fatal("earlier snapshot must be subsumed, not re-included")
	}
	// 11 is incorporated in the deep snapshot: not re-spliced.
	if strings.Contains(asm.Context, "bob's phone reply") {
		t.Fatal("incorporated above-base reply must not be duplicated")
	}
	if !strings.Contains(asm.Context, "bob's latest reply") {
		t.Fatal("below-base reply missing")
	}
	if !strings.Contains(asm.Context, "reply_to_thread") || !strings.Contains(asm.Context, "13") {
		t.Fatal("reply-target footer missing")
	}
	if !strings.Contains(asm.Context, "conversation data") {
		t.Fatal("data-not-instructions preamble missing")
	}
}

func TestAssembleSplicesUnincorporatedAboveBase(t *testing.T) {
	f := buildFake()
	f.attach[12].Provenance.IncorporatedFmsgIDs = nil // Alice re-shared without resuming first
	asm, err := Assemble(context.Background(), f, 13)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(asm.Context, "bob's phone reply") ||
		!strings.Contains(asm.Context, "not part of the shared session snapshot") {
		t.Fatal("unincorporated above-base reply must be spliced with a marker")
	}
}

func TestAssemblePlainThreadWithoutSnapshot(t *testing.T) {
	f := &fake{
		msgs: map[int64]*cli.Message{
			1: {From: "@a@example.com", Topic: "plain"},
			2: {From: "@b@example.com", PID: pid(1)},
		},
		bodies: map[int64]string{1: "first", 2: "second"},
	}
	asm, err := Assemble(context.Background(), f, 2)
	if err != nil {
		t.Fatal(err)
	}
	if asm.BaseID != nil {
		t.Fatal("no base expected")
	}
	for _, want := range []string{"first", "second", "plain fmsg thread"} {
		if !strings.Contains(asm.Context, want) {
			t.Fatalf("missing %q", want)
		}
	}
}

func TestWalkHopCap(t *testing.T) {
	f := &fake{msgs: map[int64]*cli.Message{}}
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
