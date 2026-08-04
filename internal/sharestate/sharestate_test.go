package sharestate

import "testing"

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

	if st, err := Load("no-such-session"); err != nil || st != nil {
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
	out, err := Load("abc-123")
	if err != nil {
		t.Fatal(err)
	}
	if out.ThreadRoot != 10 || out.LastFmsgID != 12 || len(out.ExchangeHashes) != 3 {
		t.Fatalf("round trip mismatch: %+v", out)
	}

	// Overwrite advances the chain.
	in.LastFmsgID = 15
	in.ExchangeHashes = append(in.ExchangeHashes, HashBody("four"))
	if err := Save(in); err != nil {
		t.Fatal(err)
	}
	out, _ = Load("abc-123")
	if out.LastFmsgID != 15 || len(out.ExchangeHashes) != 4 {
		t.Fatalf("overwrite mismatch: %+v", out)
	}
}
