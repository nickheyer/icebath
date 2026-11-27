// Bitwarden Password Manager as a source of website logins for the vault, read through the bw program once unlocked
package vault

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/nickheyer/icebath/internal/config"
	bitwardenconfig "github.com/nickheyer/icebath/internal/plugins/defaults/secrets/bitwarden/config"
	"github.com/nickheyer/icebath/internal/secrets"
	"github.com/nickheyer/icebath/internal/vault"
	"github.com/nickheyer/icebath/pkg/strs"
	"github.com/nickheyer/icebath/pkg/term"
)

// Longest one bw call may take
const timeout = 30 * time.Second

// Variable the master password reaches bw through, set on its process alone
const masterEnv = "ICEBATH_BW_MASTER"

// Variables of the profile a bw process sees
var kept = []string{"PATH", "HOME", "USERPROFILE", "APPDATA", "LOCALAPPDATA", "SystemRoot", "TMPDIR", "TMP", "TEMP", "XDG_CONFIG_HOME",
	"BITWARDENCLI_APPDATA_DIR"}

func Register(registry *vault.Registry) {
	registry.Register(vault.Backends{{Name: bitwardenconfig.Name, Title: "Bitwarden", Installed: installed,
		Open: func(cfg *config.Config) vault.Manager { return manager{cfg: cfg} }}})
}

// The bw program the profile names, else the one on the search path, empty when there is none
func binary(cfg *config.Config) string {
	if pinned := strings.TrimSpace(cfg.GetString(bitwardenconfig.VaultBinary)); pinned != "" {
		return filepath.Clean(pinned)
	}
	found, _ := exec.LookPath("bw")
	return found
}

// Whether the bw program is there: the one the profile names a file, else one on the search path
func installed(cfg *config.Config) bool {
	path := binary(cfg)
	if path == "" {
		return false
	}
	info, err := os.Stat(path)
	return err == nil && info.Mode().IsRegular()
}

type manager struct {
	cfg *config.Config
}

func (manager) Ready() bool { return false }

func (m manager) bw() (string, error) {
	if found := binary(m.cfg); found != "" {
		return found, nil
	}
	return "", errors.New("Bitwarden CLI (bw) not found — install it or set vault.bitwarden.binary_path")
}

func (m manager) env(session string) map[string]string {
	env := map[string]string{"NO_COLOR": "1"}
	for _, name := range kept {
		if value, ok := m.cfg.LookupEnv(name); ok {
			env[name] = value
		}
	}
	if session != "" {
		env["BW_SESSION"] = session
	}
	return env
}

// Mints a session key with the master password, handed to bw through a variable of its process alone
func (m manager) Unlock(ctx context.Context, master string) (string, error) {
	bw, err := m.bw()
	if err != nil {
		return "", err
	}
	env := m.env("")
	env[masterEnv] = master
	out, err := secrets.Run(ctx, []string{bw, "unlock", "--raw", "--nointeraction", "--passwordenv", masterEnv}, env, timeout, "bw",
		fmt.Sprintf("bw unlock timed out after %.0fs", timeout.Seconds()), nil)
	if err != nil {
		return "", err
	}
	token := strings.TrimSpace(out.Stdout)
	if out.Code != 0 || token == "" {
		detail := strs.Head(strings.TrimSpace(term.StripANSI(out.Stderr)), 200)
		if strings.Contains(strings.ToLower(detail), "not logged in") {
			detail = "not logged in — run `bw login` once in a terminal first"
		}
		if detail == "" {
			detail = "no session key"
		}
		return "", fmt.Errorf("Bitwarden unlock failed: %s", detail)
	}
	return token, nil
}

// Output of one bw call, a locked or expired session answered as vault.ErrLocked
func (m manager) run(ctx context.Context, session string, args ...string) (string, error) {
	if session == "" {
		return "", vault.ErrLocked
	}
	bw, err := m.bw()
	if err != nil {
		return "", err
	}
	out, err := secrets.Run(ctx, append(append([]string{bw}, args...), "--nointeraction"), m.env(session), timeout, "bw", "bw timed out", nil)
	if err != nil {
		return "", err
	}
	if out.Code != 0 {
		detail := term.StripANSI(out.Stderr)
		if lowered := strings.ToLower(detail); strings.Contains(lowered, "locked") || strings.Contains(lowered, "session") {
			return "", vault.ErrLocked
		}
		return "", fmt.Errorf("bw failed: %s", strs.Head(detail, 200))
	}
	return out.Stdout, nil
}

// Every login with an address, those set to never match left out
func (m manager) Items(ctx context.Context, session string) ([]vault.Item, error) {
	text, err := m.run(ctx, session, "list", "items")
	if err != nil {
		return nil, err
	}
	var listed []struct {
		ID      string `json:"id"`
		Type    int    `json:"type"`
		Name    string `json:"name"`
		Created string `json:"creationDate"`
		Login   *struct {
			Username *string `json:"username"`
			URIs     []struct {
				URI   *string `json:"uri"`
				Match *int    `json:"match"`
			} `json:"uris"`
		} `json:"login"`
	}
	if strings.TrimSpace(text) == "" {
		text = "[]"
	}
	if err := json.Unmarshal([]byte(text), &listed); err != nil {
		return nil, fmt.Errorf("bw listed something other than items: %v", err)
	}
	out := []vault.Item{}
	for _, entry := range listed {
		if entry.Type != 1 || entry.Login == nil || entry.ID == "" {
			continue
		}
		var urls []string
		for _, uri := range entry.Login.URIs {
			if uri.URI != nil && (uri.Match == nil || *uri.Match != 5) {
				urls = append(urls, *uri.URI)
			}
		}
		username := ""
		if entry.Login.Username != nil {
			username = *entry.Login.Username
		}
		if item, ok := vault.Site(entry.ID, entry.Name, entry.Created, username, urls); ok {
			out = append(out, item)
		}
	}
	return out, nil
}

func (m manager) Password(ctx context.Context, session, id string) (string, error) {
	out, err := m.run(ctx, session, "get", "password", id)
	if err != nil {
		return "", err
	}
	return strings.TrimRight(out, "\r\n"), nil
}

func (m manager) OTP(ctx context.Context, session, id string) (string, bool) {
	out, err := m.run(ctx, session, "get", "totp", id)
	if err != nil {
		return "", false
	}
	return vault.Code(out)
}
