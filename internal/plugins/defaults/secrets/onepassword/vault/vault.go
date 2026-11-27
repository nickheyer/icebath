// 1Password as a source of website logins for the vault, read through the op program once unlocked or with a service account token
package vault

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/nickheyer/icebath/internal/config"
	onepasswordconfig "github.com/nickheyer/icebath/internal/plugins/defaults/secrets/onepassword/config"
	onepasswordsecrets "github.com/nickheyer/icebath/internal/plugins/defaults/secrets/onepassword/secrets"
	"github.com/nickheyer/icebath/internal/secrets"
	"github.com/nickheyer/icebath/internal/vault"
	"github.com/nickheyer/icebath/pkg/strs"
)

// Longest one op call may take
const timeout = 30 * time.Second

func Register(registry *vault.Registry) {
	registry.Register(vault.Backends{{Name: onepasswordconfig.Name, Title: "1Password",
		Installed: func(cfg *config.Config) bool {
			return onepasswordsecrets.Find(cfg.GetString(onepasswordconfig.VaultBinary)) != ""
		},
		Open: open}})
}

func open(cfg *config.Config) vault.Manager {
	tokenEnv := strings.TrimSpace(cfg.GetString(onepasswordconfig.VaultTokenEnv))
	if tokenEnv == "" {
		tokenEnv = onepasswordconfig.DefaultTokenEnv
	}
	return &manager{cfg: cfg, account: strings.TrimSpace(cfg.GetString(onepasswordconfig.VaultAccount)), service: strings.TrimSpace(cfg.Getenv(tokenEnv)),
		vaults: map[string]string{}}
}

type manager struct {
	cfg     *config.Config
	account string
	// Service account token of the profile, which skips the unlock
	service string
	mu      sync.Mutex
	// Vault each listed login is in, by its id
	vaults map[string]string
}

func (m *manager) Ready() bool { return m.service != "" }

func (m *manager) op() (string, error) {
	if found := onepasswordsecrets.Find(m.cfg.GetString(onepasswordconfig.VaultBinary)); found != "" {
		return found, nil
	}
	return "", errors.New("op, the 1Password command line tool, is not installed. Install it, or set op program in /settings under Autofill vault › 1Password.")
}

// Allowed variables of the profile, its Connect credentials, the account and the service token or the session of an unlock
func (m *manager) env(session string) map[string]string {
	env := map[string]string{"NO_COLOR": "1"}
	for _, name := range onepasswordsecrets.Allowed {
		if value, ok := m.cfg.LookupEnv(name); ok && value != "" {
			env[name] = value
		}
	}
	if m.account != "" {
		env["OP_ACCOUNT"] = m.account
	}
	switch {
	case m.service != "":
		env["OP_SERVICE_ACCOUNT_TOKEN"] = m.service
	case session != "":
		name := "OP_SESSION"
		if m.account != "" {
			name += "_" + m.account
		}
		env[name] = session
	}
	return env
}

// Mints a session token from the master password, which op reads on its stdin
func (m *manager) Unlock(ctx context.Context, master string) (string, error) {
	op, err := m.op()
	if err != nil {
		return "", err
	}
	argv := []string{op, "signin", "--raw"}
	if m.account != "" {
		argv = append(argv, "--account", m.account)
	}
	out, err := secrets.Run(ctx, argv, m.env(""), timeout, "op", fmt.Sprintf("op did not answer within %s while unlocking.", strs.Span(timeout)),
		strings.NewReader(master+"\n"))
	if err != nil {
		return "", err
	}
	token := strings.TrimSpace(out.Stdout)
	if out.Code != 0 || token == "" {
		detail := strs.Head(onepasswordsecrets.Scrub(out.Stderr), 200)
		if detail == "" {
			detail = "op gave no session token."
		}
		return "", fmt.Errorf("1Password did not unlock: %s", secrets.Sentence(detail))
	}
	return token, nil
}

// Output of one op call, a failed session of a signed in account answered as vault.ErrLocked
func (m *manager) run(ctx context.Context, session string, args ...string) (string, error) {
	if m.service == "" && session == "" {
		return "", vault.ErrLocked
	}
	op, err := m.op()
	if err != nil {
		return "", err
	}
	out, err := secrets.Run(ctx, append([]string{op}, args...), m.env(session), timeout, "op", "op did not answer within "+strs.Span(timeout)+".", nil)
	if err != nil {
		return "", err
	}
	if out.Code != 0 {
		detail := onepasswordsecrets.Scrub(out.Stderr)
		lowered := strings.ToLower(detail)
		if m.service == "" && (strings.Contains(lowered, "session") || strings.Contains(lowered, "sign in") || strings.Contains(lowered, "not signed in")) {
			return "", vault.ErrLocked
		}
		return "", fmt.Errorf("op stopped with an error: %s", secrets.Sentence(strs.Head(detail, 200)))
	}
	return out.Stdout, nil
}

// Every Login item with an address, the vault of each kept for its later reads
func (m *manager) Items(ctx context.Context, session string) ([]vault.Item, error) {
	text, err := m.run(ctx, session, "item", "list", "--categories", "Login", "--format", "json")
	if err != nil {
		return nil, err
	}
	if strings.TrimSpace(text) == "" {
		text = "[]"
	}
	var listed []struct {
		ID      string `json:"id"`
		Title   string `json:"title"`
		Created string `json:"created_at"`
		Extra   string `json:"additional_information"`
		Vault   *struct {
			ID string `json:"id"`
		} `json:"vault"`
		URLs []struct {
			Href string `json:"href"`
		} `json:"urls"`
	}
	if err := json.Unmarshal([]byte(text), &listed); err != nil {
		return nil, fmt.Errorf("op printed a list of items icebath can't read: %v.", err)
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.vaults = map[string]string{}
	out := []vault.Item{}
	for _, entry := range listed {
		if entry.ID == "" {
			continue
		}
		if entry.Vault != nil {
			m.vaults[entry.ID] = entry.Vault.ID
		} else {
			m.vaults[entry.ID] = ""
		}
		var urls []string
		for _, u := range entry.URLs {
			if u.Href != "" {
				urls = append(urls, u.Href)
			}
		}
		if item, ok := vault.Site(entry.ID, entry.Title, entry.Created, entry.Extra, urls); ok {
			out = append(out, item)
		}
	}
	return out, nil
}

// Target of op item get for a listed login, naming the vault the listing said it is in
func (m *manager) selector(ctx context.Context, session, id string) ([]string, error) {
	m.mu.Lock()
	_, listed := m.vaults[id]
	m.mu.Unlock()
	if !listed {
		if _, err := m.Items(ctx, session); err != nil {
			return nil, err
		}
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	in, listed := m.vaults[id]
	if !listed {
		return nil, errors.New("That 1Password login is no longer in the list. List the logins again, then pick it.")
	}
	if in != "" {
		return []string{id, "--vault", in}, nil
	}
	return []string{id}, nil
}

func (m *manager) Password(ctx context.Context, session, id string) (string, error) {
	target, err := m.selector(ctx, session, id)
	if err != nil {
		return "", err
	}
	out, err := m.run(ctx, session, append(append([]string{"item", "get"}, target...), "--fields", "label=password", "--reveal")...)
	if err != nil {
		return "", err
	}
	return strings.TrimRight(out, "\r\n"), nil
}

func (m *manager) OTP(ctx context.Context, session, id string) (string, bool) {
	target, err := m.selector(ctx, session, id)
	if err != nil {
		return "", false
	}
	out, err := m.run(ctx, session, append(append([]string{"item", "get"}, target...), "--otp")...)
	if err != nil {
		return "", false
	}
	return vault.Code(out)
}
