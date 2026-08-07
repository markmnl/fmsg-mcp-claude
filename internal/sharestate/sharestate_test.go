package sharestate

import (
	"os"
	"strings"
	"testing"
)

func TestDelta(t *testing.T) {
	bodies := []string{"one", "two", "three"}
	prefix := HashBodies(bodies[:2])

	delta, ok := Delta(prefix, bodies)
	if !ok || len(delta) != 1 || delta[0] != "three" {
		t.Fatalf("extension not detected: ok=%v delta=%v", ok, delta)
	}

	if delta, ok := Delta(HashBodies(bodies), bodies); !ok || len(delta) != 0 {
		t.Fatalf("fully shared session should yield empty delta: ok=%v delta=%v", ok, delta)
	}

	if _, ok := Delta(HashBodies([]string{"one", "CHANGED"}), bodies); ok {
		t.Fatal("diverged prefix must not match")
	}

	if _, ok := Delta(HashBodies([]string{"one", "two", "three", "four"}), bodies); ok {
		t.Fatal("shrunken session must not match")
	}

	if delta, ok := Delta(nil, bodies); !ok || len(delta) != 3 {
		t.Fatalf("no prior state should yield the whole session: ok=%v n=%d", ok, len(delta))
	}
}

func TestSameRecipients(t *testing.T) {
	cases := []struct {
		prev, next []string
		want       bool
	}{
		{[]string{"@a@x.io", "@b@y.io"}, []string{"@b@y.io", "@a@x.io"}, true},
		{[]string{"@A@X.io"}, []string{"@a@x.io"}, true},
		{[]string{"@a@x.io"}, []string{"@a@x.io", "@b@y.io"}, false}, // widened
		{[]string{"@a@x.io", "@b@y.io"}, []string{"@a@x.io"}, false}, // narrowed
	}
	for i, c := range cases {
		if got := SameRecipients(c.prev, c.next); got != c.want {
			t.Errorf("case %d: SameRecipients(%v, %v) = %v, want %v", i, c.prev, c.next, got, c.want)
		}
	}
}

func TestSaveLoadRoundTrip(t *testing.T) {
	t.Setenv("HOME", t.TempDir())

	if st, err := Load("no-such-session", ""); err != nil || st != nil {
		t.Fatalf("missing state should be (nil, nil), got (%v, %v)", st, err)
	}

	in := &State{
		SessionID:      "abc-123",
		ThreadRoot:     10,
		LastFmsgID:     12,
		Recipients:     []string{"@kebbie@fmsg.io"},
		ExchangeHashes: HashBodies([]string{"one", "two", "three"}),
	}
	if err := Save(in); err != nil {
		t.Fatal(err)
	}
	out, err := Load("abc-123", "")
	if err != nil {
		t.Fatal(err)
	}
	if out.ThreadRoot != 10 || out.LastFmsgID != 12 || len(out.ExchangeHashes) != 3 {
		t.Fatalf("round trip mismatch: %+v", out)
	}
	if out.FormatVersion != CurrentFormat {
		t.Fatalf("Save must stamp CurrentFormat, got %d", out.FormatVersion)
	}

	// Overwrite advances the chain.
	in.LastFmsgID = 15
	in.ExchangeHashes = append(in.ExchangeHashes, HashBody("four"))
	if err := Save(in); err != nil {
		t.Fatal(err)
	}
	out, _ = Load("abc-123", "")
	if out.LastFmsgID != 15 || len(out.ExchangeHashes) != 4 {
		t.Fatalf("overwrite mismatch: %+v", out)
	}
}

func TestLegacyStateKeepsFormatZero(t *testing.T) {
	t.Setenv("HOME", t.TempDir())

	// A pre-FormatVersion file unmarshals with FormatVersion 0, which is how
	// the caller detects stale-format hashes and starts a fresh thread.
	legacy := &State{SessionID: "old-1", ThreadRoot: 1, LastFmsgID: 2}
	if err := Save(legacy); err != nil {
		t.Fatal(err)
	}
	p, err := path("old-1", "")
	if err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	stripped := strings.Replace(string(raw), `"format_version": 2,`, "", 1)
	if stripped == string(raw) {
		t.Fatal("fixture assumption broken: format_version not found in saved state")
	}
	if err := os.WriteFile(p, []byte(stripped), 0o600); err != nil {
		t.Fatal(err)
	}
	out, err := Load("old-1", "")
	if err != nil {
		t.Fatal(err)
	}
	if out.FormatVersion != 0 {
		t.Fatalf("legacy state must load with FormatVersion 0, got %d", out.FormatVersion)
	}
}

func TestSummaryKindSeparateFile(t *testing.T) {
	t.Setenv("HOME", t.TempDir())

	tx := &State{SessionID: "abc-123", ThreadRoot: 10, LastFmsgID: 12,
		Recipients: []string{"@kebbie@fmsg.io"}, ExchangeHashes: HashBodies([]string{"one"})}
	sum := &State{SessionID: "abc-123", Kind: KindSummary, ThreadRoot: 20, LastFmsgID: 20,
		Recipients: []string{"@kebbie@fmsg.io"}, SummaryCount: 1}
	if err := Save(tx); err != nil {
		t.Fatal(err)
	}
	if err := Save(sum); err != nil {
		t.Fatal(err)
	}

	outTx, err := Load("abc-123", "")
	if err != nil {
		t.Fatal(err)
	}
	outSum, err := Load("abc-123", KindSummary)
	if err != nil {
		t.Fatal(err)
	}
	if outTx.ThreadRoot != 10 || len(outTx.ExchangeHashes) != 1 || outTx.Kind != "" {
		t.Fatalf("transcript state clobbered: %+v", outTx)
	}
	if outSum.ThreadRoot != 20 || outSum.SummaryCount != 1 || outSum.Kind != KindSummary {
		t.Fatalf("summary state mismatch: %+v", outSum)
	}

	p, err := path("abc-123", KindSummary)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasSuffix(p, "abc-123.summary.json") {
		t.Fatalf("summary state path: %s", p)
	}
}
