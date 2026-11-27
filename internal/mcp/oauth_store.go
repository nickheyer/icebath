package mcp

import (
	"encoding/json"
	"errors"
	"io/fs"
	"log/slog"
	"maps"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
	"unicode"

	"github.com/nickheyer/icebath/pkg/fsx"
)

// Key of the issuer a token file was minted by
const issuerKey = "icebath_issuer"

// OAuth state of one server kept on disk: its tokens, its client registration, the authorization server's metadata and whether the server
// refused the client ID metadata document
type Store struct {
	dir  string
	name string

	mu sync.Mutex
	// Issuer the loaded token file was minted by, and the one later writes are stamped with
	loaded, bound string
}

func NewStore(dir, server string) *Store {
	return &Store{dir: dir, name: server}
}

func (s *Store) Server() string { return s.name }

// Server name made safe for a file name: letters, digits, underscores and dashes
func fileName(name string) string {
	var b strings.Builder
	for _, r := range name {
		if unicode.IsLetter(r) || unicode.IsNumber(r) || r == '_' || r == '-' {
			b.WriteRune(r)
		} else {
			b.WriteByte('_')
		}
	}
	out := strings.Trim(b.String(), "_")
	if runes := []rune(out); len(runes) > 128 {
		out = string(runes[:128])
	}
	if out == "" {
		return "default"
	}
	return out
}

func (s *Store) path(suffix string) string { return filepath.Join(s.dir, fileName(s.name)+suffix) }

func (s *Store) TokensPath() string { return s.path(".json") }

func (s *Store) clientPath() string { return s.path(".client.json") }

func (s *Store) metaPath() string { return s.path(".meta.json") }

func (s *Store) cimdPath() string { return s.path(".cimd-off") }

// Object of a JSON file, nil when it is missing or unreadable
func readObject(path string) map[string]any {
	data, err := os.ReadFile(path)
	if err != nil {
		if !errors.Is(err, fs.ErrNotExist) {
			slog.Warn("MCP OAuth state unreadable", "path", path, "error", err)
		}
		return nil
	}
	value, err := DecodeExact(data)
	object, ok := value.(map[string]any)
	if err != nil || !ok {
		slog.Warn("MCP OAuth state corrupt, ignoring it", "path", path)
		return nil
	}
	return object
}

// Writes OAuth state readable by this user alone, under a folder only it may enter
func writeObject(path string, value any) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	os.Chmod(filepath.Dir(path), 0o700)
	data, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return err
	}
	return fsx.Write(path, data, 0o600)
}

// Tokens kept, their expiry counted from now, nil when none are
func (s *Store) Tokens() *Token {
	m := readObject(s.TokensPath())
	issuer, _ := m[issuerKey].(string)
	s.mu.Lock()
	s.loaded = issuer
	s.mu.Unlock()
	if m == nil {
		return nil
	}
	t, err := parseToken(m)
	if err != nil {
		slog.Warn("MCP OAuth tokens corrupt, ignoring them", "server", s.name, "error", err)
		return nil
	}
	if at, ok := number(m["expires_at"]); ok {
		t.Expiry = time.Unix(0, int64(at*1e9))
	} else if t.ExpiresIn > 0 {
		if info, err := os.Stat(s.TokensPath()); err == nil {
			t.Expiry = info.ModTime().Add(time.Duration(t.ExpiresIn) * time.Second)
		}
	}
	return t
}

// Keeps the tokens with their absolute expiry and the bound issuer
func (s *Store) SetTokens(t *Token) error {
	payload := t.object()
	if !t.Expiry.IsZero() {
		payload["expires_at"] = float64(t.Expiry.UnixNano()) / 1e9
	}
	s.mu.Lock()
	if s.bound != "" {
		payload[issuerKey] = s.bound
		s.loaded = s.bound
	}
	s.mu.Unlock()
	return writeObject(s.TokensPath(), payload)
}

// Issuer the loaded tokens were minted by
func (s *Store) LoadedIssuer() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.loaded
}

// Issuer stamped on later token writes
func (s *Store) Bind(issuer string) {
	s.mu.Lock()
	s.bound = issuer
	s.mu.Unlock()
}

// Stamps the issuer on a token file written before issuers were recorded
func (s *Store) StampIssuer(issuer string) {
	m := readObject(s.TokensPath())
	if m == nil || m[issuerKey] != nil {
		return
	}
	m[issuerKey] = issuer
	if err := writeObject(s.TokensPath(), m); err == nil {
		s.mu.Lock()
		s.loaded = issuer
		s.mu.Unlock()
	}
}

// Drops the refresh token, for tokens minted by an issuer no longer in use
func (s *Store) StripRefresh() {
	m := readObject(s.TokensPath())
	if m == nil {
		return
	}
	delete(m, "refresh_token")
	writeObject(s.TokensPath(), m)
}

// Client registration kept, nil when none is
func (s *Store) Client() *ClientInfo {
	m := readObject(s.clientPath())
	if m == nil {
		return nil
	}
	info, err := parseClientInfo(m)
	if err != nil {
		slog.Warn("MCP OAuth client registration corrupt, ignoring it", "server", s.name, "error", err)
		return nil
	}
	return info
}

func (s *Store) SetClient(info *ClientInfo) error {
	return writeObject(s.clientPath(), info.object())
}

// Removes the client registration the authorization server refused, so the next sign in registers again; false when there was none
func (s *Store) DropClient() bool {
	return os.Remove(s.clientPath()) == nil
}

// Authorization server metadata kept, nil when none is
func (s *Store) Metadata() *ServerMetadata {
	m := readObject(s.metaPath())
	if m == nil {
		return nil
	}
	md, err := parseServerMetadata(m, true)
	if err != nil {
		return nil
	}
	return md
}

func (s *Store) SetMetadata(md *ServerMetadata) error {
	return writeObject(s.metaPath(), md.raw)
}

// Remembers that the authorization server refused the client ID metadata document
func (s *Store) MarkCIMDRejected() {
	if err := os.MkdirAll(s.dir, 0o700); err == nil {
		os.WriteFile(s.cimdPath(), nil, 0o600)
	}
}

func (s *Store) CIMDRejected() bool {
	_, err := os.Stat(s.cimdPath())
	return err == nil
}

// Whether a token file is kept
func (s *Store) HasTokens() bool {
	_, err := os.Stat(s.TokensPath())
	return err == nil
}

// Removes the tokens and registration, and the metadata unless kept
func (s *Store) Remove(keepMetadata bool) error {
	paths := []string{s.TokensPath(), s.clientPath(), s.cimdPath()}
	if !keepMetadata {
		paths = append(paths, s.metaPath())
	}
	var errs []error
	for _, path := range paths {
		if err := os.Remove(path); err != nil && !errors.Is(err, fs.ErrNotExist) {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// When and how large the token and client files were, to tell when another process changed them
type Stamp [2]struct {
	mod  time.Time
	size int64
}

func (s *Store) Stamp() Stamp {
	var stamp Stamp
	for i, path := range []string{s.TokensPath(), s.clientPath()} {
		if info, err := os.Stat(path); err == nil {
			stamp[i].mod, stamp[i].size = info.ModTime(), info.Size()
		}
	}
	return stamp
}

// Bytes of every state file, to put them back when a trial sign in is undone
func (s *Store) Snapshot() map[string][]byte {
	snap := map[string][]byte{}
	for _, path := range []string{s.TokensPath(), s.clientPath(), s.metaPath(), s.cimdPath()} {
		if data, err := os.ReadFile(path); err == nil {
			snap[path] = data
		}
	}
	return snap
}

// Puts the state files back as the snapshot had them, removing those it lacked
func (s *Store) Restore(snap map[string][]byte) error {
	var errs []error
	for _, path := range []string{s.TokensPath(), s.clientPath(), s.metaPath(), s.cimdPath()} {
		data, kept := snap[path]
		if !kept {
			if err := os.Remove(path); err != nil && !errors.Is(err, fs.ErrNotExist) {
				errs = append(errs, err)
			}
			continue
		}
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			errs = append(errs, err)
			continue
		}
		errs = append(errs, os.WriteFile(path, data, 0o600))
	}
	return errors.Join(errs...)
}

func copyObject(m map[string]any) map[string]any { return maps.Clone(m) }
