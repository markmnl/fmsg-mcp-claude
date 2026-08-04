// Package identity resolves teammate names to fmsg addresses and reads the
// server's own identity (ARCHITECTURE.md §5). Heuristic resolutions are never
// sent blind — callers surface them for confirmation first.
package identity

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

var addrRe = regexp.MustCompile(`^@[^@\s]+@[^@\s]+\.[^@\s]+$`)

// IsAddress reports whether s is a full fmsg address.
func IsAddress(s string) bool { return addrRe.MatchString(s) }

// Config holds the org-provisioned resolution inputs.
type Config struct {
	DirectoryPath string // FMSG_DIRECTORY: JSON map of short name -> address
	DefaultDomain string // FMSG_DEFAULT_DOMAIN: convention fallback domain
	AgentSuffix   string // FMSG_AGENT_SUFFIX, default "_claude"
}

// FromEnv builds a Config from the server environment.
func FromEnv() Config {
	suffix := os.Getenv("FMSG_AGENT_SUFFIX")
	if suffix == "" {
		suffix = "_claude"
	}
	return Config{
		DirectoryPath: os.Getenv("FMSG_DIRECTORY"),
		DefaultDomain: os.Getenv("FMSG_DEFAULT_DOMAIN"),
		AgentSuffix:   suffix,
	}
}

// Resolve maps a name to an fmsg address. Resolution is one of
// "literal", "directory", or "convention"; only "literal" is safe to send
// without user confirmation.
func (c Config) Resolve(name string) (addr, resolution string, err error) {
	name = strings.TrimSpace(name)
	if IsAddress(name) {
		return name, "literal", nil
	}
	if c.DirectoryPath != "" {
		if a, ok := c.lookupDirectory(name); ok {
			return a, "directory", nil
		}
	}
	if c.DefaultDomain != "" {
		return fmt.Sprintf("@%s@%s", strings.ToLower(name), c.DefaultDomain), "convention", nil
	}
	return "", "", fmt.Errorf("cannot resolve %q: not a full @user@domain address, not in FMSG_DIRECTORY, and FMSG_DEFAULT_DOMAIN is unset", name)
}

func (c Config) lookupDirectory(name string) (string, bool) {
	raw, err := os.ReadFile(c.DirectoryPath)
	if err != nil {
		return "", false
	}
	var dir map[string]string
	if err := json.Unmarshal(raw, &dir); err != nil {
		return "", false
	}
	for k, v := range dir {
		if strings.EqualFold(k, name) && IsAddress(v) {
			return v, true
		}
	}
	return "", false
}

// AgentFor derives the _claude sub-account address for a human address
// (dual addressing, ARCHITECTURE.md §5).
func (c Config) AgentFor(humanAddr string) string {
	rest := strings.TrimPrefix(humanAddr, "@")
	i := strings.Index(rest, "@")
	if i < 0 {
		return ""
	}
	return "@" + rest[:i] + c.AgentSuffix + "@" + rest[i+1:]
}

// HumanFor strips the agent suffix from a sub-account address; returns the
// input unchanged when it carries no suffix.
func (c Config) HumanFor(agentAddr string) string {
	rest := strings.TrimPrefix(agentAddr, "@")
	i := strings.Index(rest, "@")
	if i < 0 {
		return agentAddr
	}
	user, domain := rest[:i], rest[i+1:]
	if strings.HasSuffix(user, c.AgentSuffix) && len(user) > len(c.AgentSuffix) {
		return "@" + strings.TrimSuffix(user, c.AgentSuffix) + "@" + domain
	}
	return agentAddr
}

// Whoami describes the CLI identity the server runs as.
type Whoami struct {
	Address      string `json:"address"`
	HumanAddress string `json:"human_address,omitempty"`
	APIURL       string `json:"api_url,omitempty"`
	AuthType     string `json:"auth_type"`
	ExpiresAt    string `json:"expires_at,omitempty"`
}

// authFile mirrors fmsg-cli's ~/.config/fmsg/auth.json (read-only).
type authFile struct {
	AuthType  string `json:"auth_type"`
	User      string `json:"user"`
	APIURL    string `json:"api_url"`
	ExpiresAt string `json:"expires_at"`
}

// ReadWhoami reads the logged-in identity: FMSG_API_KEY presence wins for
// auth type; the address comes from auth.json when present.
func (c Config) ReadWhoami() (*Whoami, error) {
	w := &Whoami{APIURL: os.Getenv("FMSG_API_URL")}
	if os.Getenv("FMSG_API_KEY") != "" {
		w.AuthType = "api_key_env"
	}

	cfgDir := os.Getenv("XDG_CONFIG_HOME")
	if cfgDir == "" {
		home, err := os.UserHomeDir()
		if err == nil {
			cfgDir = filepath.Join(home, ".config")
		}
	}
	if cfgDir != "" {
		if raw, err := os.ReadFile(filepath.Join(cfgDir, "fmsg", "auth.json")); err == nil {
			var af authFile
			if json.Unmarshal(raw, &af) == nil {
				if w.AuthType == "" {
					w.AuthType = af.AuthType
				}
				w.Address = af.User
				if w.APIURL == "" {
					w.APIURL = af.APIURL
				}
				w.ExpiresAt = af.ExpiresAt
			}
		}
	}
	if w.AuthType == "" {
		return nil, fmt.Errorf("not logged in: no FMSG_API_KEY and no fmsg auth.json; provision a sub-account key (see ARCHITECTURE.md §5)")
	}
	if w.Address != "" {
		w.HumanAddress = c.HumanFor(w.Address)
	}
	return w, nil
}
