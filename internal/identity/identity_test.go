package identity

import (
	"os"
	"path/filepath"
	"testing"
)

func cfg(t *testing.T, dir map[string]string, domain string) Config {
	t.Helper()
	c := Config{DefaultDomain: domain, AgentSuffix: "_claude"}
	if dir != nil {
		path := filepath.Join(t.TempDir(), "directory.json")
		raw := "{"
		first := true
		for k, v := range dir {
			if !first {
				raw += ","
			}
			raw += `"` + k + `":"` + v + `"`
			first = false
		}
		raw += "}"
		if err := os.WriteFile(path, []byte(raw), 0o600); err != nil {
			t.Fatal(err)
		}
		c.DirectoryPath = path
	}
	return c
}

func TestResolveChain(t *testing.T) {
	c := cfg(t, map[string]string{"bob": "@robert@corp.example.com"}, "example.com")

	addr, res, err := c.Resolve("@carol@other.example.org")
	if err != nil || res != "literal" || addr != "@carol@other.example.org" {
		t.Fatalf("literal: %s %s %v", addr, res, err)
	}
	addr, res, err = c.Resolve("Bob")
	if err != nil || res != "directory" || addr != "@robert@corp.example.com" {
		t.Fatalf("directory (case-insensitive): %s %s %v", addr, res, err)
	}
	addr, res, err = c.Resolve("dana")
	if err != nil || res != "convention" || addr != "@dana@example.com" {
		t.Fatalf("convention: %s %s %v", addr, res, err)
	}

	none := cfg(t, nil, "")
	if _, _, err := none.Resolve("dana"); err == nil {
		t.Fatal("expected error with no directory and no default domain")
	}
}

func TestAgentHumanRoundTrip(t *testing.T) {
	c := Config{AgentSuffix: "_claude"}
	if got := c.AgentFor("@bob@example.com"); got != "@bob_claude@example.com" {
		t.Fatalf("AgentFor: %s", got)
	}
	if got := c.HumanFor("@bob_claude@example.com"); got != "@bob@example.com" {
		t.Fatalf("HumanFor: %s", got)
	}
	if got := c.HumanFor("@bob@example.com"); got != "@bob@example.com" {
		t.Fatalf("HumanFor passthrough: %s", got)
	}
}
