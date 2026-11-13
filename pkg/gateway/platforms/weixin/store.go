package weixin

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/nickheyer/icebath/pkg/config"
)

const typingTicketTTL = 600 * time.Second

// Home subdirectory holding per account credentials, poll cursors and context tokens
func accountDir(homeDir string) string { return filepath.Join(homeDir, "weixin", "accounts") }

// JSON file contents with any UTF-8 byte order mark removed, nil for a missing file
func readJSONFile(path string) ([]byte, error) {
	raw, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return bytes.TrimPrefix(raw, []byte{0xEF, 0xBB, 0xBF}), nil
}

// Two space indented JSON written atomically with owner only permissions
func writeJSONFile(path string, v any) error {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", "  ")
	if err := enc.Encode(v); err != nil {
		return err
	}
	return config.AtomicWrite(path, bytes.TrimRight(buf.Bytes(), "\n"), 0o600)
}

// Bot credentials saved by QR login for one iLink account
type savedAccount struct {
	Token   string `json:"token"`
	BaseURL string `json:"base_url"`
	UserID  string `json:"user_id"`
	SavedAt string `json:"saved_at"`
}

// Persists the QR login credentials of an account
func saveAccount(homeDir, accountID, token, baseURL, userID string) error {
	return writeJSONFile(filepath.Join(accountDir(homeDir), accountID+".json"), savedAccount{
		Token: token, BaseURL: baseURL, UserID: userID, SavedAt: time.Now().UTC().Format("2006-01-02T15:04:05Z")})
}

// Saved credentials of an account, nil when none were saved or the file is unreadable
func loadAccount(homeDir, accountID string) *savedAccount {
	raw, err := readJSONFile(filepath.Join(accountDir(homeDir), accountID+".json"))
	if err != nil || raw == nil {
		return nil
	}
	var acct struct {
		Token   flexString `json:"token"`
		BaseURL flexString `json:"base_url"`
		UserID  flexString `json:"user_id"`
		SavedAt flexString `json:"saved_at"`
	}
	if json.Unmarshal(raw, &acct) != nil {
		return nil
	}
	return &savedAccount{Token: string(acct.Token), BaseURL: string(acct.BaseURL), UserID: string(acct.UserID), SavedAt: string(acct.SavedAt)}
}

// Long poll cursor of an account, empty when none was saved
func loadSyncBuf(homeDir, accountID string) string {
	raw, err := readJSONFile(filepath.Join(accountDir(homeDir), accountID+".sync.json"))
	if err != nil || raw == nil {
		return ""
	}
	var data struct {
		GetUpdatesBuf flexString `json:"get_updates_buf"`
	}
	if json.Unmarshal(raw, &data) != nil {
		return ""
	}
	return string(data.GetUpdatesBuf)
}

// Persists the long poll cursor of an account
func saveSyncBuf(homeDir, accountID, syncBuf string) error {
	return writeJSONFile(filepath.Join(accountDir(homeDir), accountID+".sync.json"), map[string]string{"get_updates_buf": syncBuf})
}

// Disk backed context_token cache keyed by account and peer
type contextTokenStore struct {
	dir string

	mu      sync.Mutex
	cache   map[string]string
	persist sync.Mutex
}

// Empty store persisting under the account directory of the home
func newContextTokenStore(homeDir string) *contextTokenStore {
	return &contextTokenStore{dir: accountDir(homeDir), cache: map[string]string{}}
}

// Map key joining an account and a peer
func tokenKey(accountID, userID string) string { return accountID + ":" + userID }

// Path of the context token file of an account
func (s *contextTokenStore) path(accountID string) string {
	return filepath.Join(s.dir, accountID+".context-tokens.json")
}

// Loads the persisted tokens of an account into the cache, logging an unreadable file
func (s *contextTokenStore) restore(accountID string) {
	raw, err := readJSONFile(s.path(accountID))
	if err == nil && raw == nil {
		return
	}
	var data map[string]any
	if err == nil {
		err = json.Unmarshal(raw, &data)
	}
	if err != nil {
		slog.Warn(fmt.Sprintf("weixin: failed to restore context tokens for %s: %v", safeID(accountID), err))
		return
	}
	restored := 0
	s.mu.Lock()
	for userID, v := range data {
		if token, ok := v.(string); ok && token != "" {
			s.cache[tokenKey(accountID, userID)] = token
			restored++
		}
	}
	s.mu.Unlock()
	if restored > 0 {
		slog.Info(fmt.Sprintf("weixin: restored %d context token(s) for %s", restored, safeID(accountID)))
	}
}

// Latest context token of a peer, empty when none is known
func (s *contextTokenStore) get(accountID, userID string) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.cache[tokenKey(accountID, userID)]
}

// Drops the cached token of a peer after iLink rejected it
func (s *contextTokenStore) forget(accountID, userID string) {
	s.mu.Lock()
	delete(s.cache, tokenKey(accountID, userID))
	s.mu.Unlock()
}

// Records a peer token and persists every token of the account in arrival order
func (s *contextTokenStore) set(accountID, userID, token string) {
	s.persist.Lock()
	defer s.persist.Unlock()
	prefix := accountID + ":"
	payload := map[string]string{}
	s.mu.Lock()
	s.cache[tokenKey(accountID, userID)] = token
	for k, v := range s.cache {
		if userID, ok := strings.CutPrefix(k, prefix); ok {
			payload[userID] = v
		}
	}
	s.mu.Unlock()
	if err := writeJSONFile(s.path(accountID), payload); err != nil {
		slog.Warn(fmt.Sprintf("weixin: failed to persist context tokens for %s: %v", safeID(accountID), err))
	}
}

// Typing tickets from getconfig kept for ten minutes per peer
type typingTicketCache struct {
	mu      sync.Mutex
	tickets map[string]typingTicket
}

type typingTicket struct {
	ticket string
	at     time.Time
}

// Live ticket of a peer, empty once it expired
func (c *typingTicketCache) get(userID string) string {
	c.mu.Lock()
	defer c.mu.Unlock()
	entry, ok := c.tickets[userID]
	if ok && time.Since(entry.at) < typingTicketTTL {
		return entry.ticket
	}
	delete(c.tickets, userID)
	return ""
}

// Stores a fresh ticket of a peer
func (c *typingTicketCache) set(userID, ticket string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.tickets == nil {
		c.tickets = map[string]typingTicket{}
	}
	c.tickets[userID] = typingTicket{ticket: ticket, at: time.Now()}
}

// First eight code points of an identifier for logs, a question mark when blank
func safeID(value string) string {
	if value = strings.TrimSpace(value); value == "" {
		return "?"
	}
	return truncateRunes(value, 8)
}
