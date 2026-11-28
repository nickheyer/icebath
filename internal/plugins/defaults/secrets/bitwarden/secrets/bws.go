package secrets

import (
	"bytes"
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/hkdf"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"os"
	"regexp"
	"strings"
	"time"

	"github.com/nickheyer/icebath/internal/config"
	"github.com/nickheyer/icebath/internal/packages"
	bitwardenpackages "github.com/nickheyer/icebath/internal/plugins/defaults/secrets/bitwarden/packages"
	"github.com/nickheyer/icebath/internal/secrets"
	"github.com/nickheyer/icebath/pkg/strs"
)

const (
	// Longest one bws call may take
	runTimeout = 30 * time.Second
	// Longest a project list may take
	projectsTimeout = 15 * time.Second
	cacheName       = "bws_cache.json"
	encryptedName   = "bws_cache.enc.json"
	encryptedFormat = 1
	encryptedInfo   = "icebath-bws-encrypted-cache-v1"
)

// Kinds of a bws failure by the words of its error, the first rule to match winning
var rules = []secrets.Rule{
	{Kind: secrets.Timeout, Words: []string{"timed out", "did not answer within"}},
	{Kind: secrets.BinaryMissing, Words: []string{"bws is not installed", "can't start"}},
	{Kind: secrets.AuthFailed, Words: []string{"unauthorized", "invalid token", "access token", "401", "403", "invalid_client", "invalid_grant", "400 bad request"}},
	{Kind: secrets.Network, Words: []string{"network", "connection", "resolve", "download", "dns"}},
}

// Bitwarden Secrets Manager through bws: the program found or installed, the projects a token sees, the secrets of a project with their
// caches
type Bws struct {
	packages *packages.Registry
	cache    *secrets.Cache
}

// The bws program: on the search path, else in the package store, else installed now when asked and the profile allows it; empty when
// there is none
func (b *Bws) Find(ctx context.Context, cfg *config.Config, install bool) string {
	return b.packages.Host().Program(ctx, cfg, bitwardenpackages.Bws, "bws", install)
}

// Installs the pinned bws program as a person asked, its entry hashed again and repaired, and answers it
func (b *Bws) Install(ctx context.Context, cfg *config.Config) (string, error) {
	return b.packages.Host().Provide(ctx, cfg, bitwardenpackages.Bws)
}

// Version of the program, "version unknown" when it says none
func (b *Bws) Version(ctx context.Context, binary string) string {
	return secrets.Version(ctx, binary, nil)
}

// Environment of one bws call: the profile's variables with the token and the server, and no color
func child(env map[string]string, token, server string) map[string]string {
	out := maps.Clone(env)
	if out == nil {
		out = map[string]string{}
	}
	out["BWS_ACCESS_TOKEN"] = token
	if _, set := out["NO_COLOR"]; !set {
		out["NO_COLOR"] = "1"
	}
	if server != "" {
		out["BWS_SERVER_URL"] = server
	}
	return out
}

// One pull of a project's secrets
type Pull struct {
	Token, Project, Server string
	// bws program, found and installed when allowed for none
	Binary string
	// Folder of the profile the caches are kept in
	Dir string
	// Seconds a pull is reused for, and whether the caches are read and written at all
	TTL   time.Duration
	Cache bool
	// An encrypted copy of the last pull, used past its ttl only when Bitwarden can't be reached, for at most MaxStale
	Encrypted bool
	MaxStale  time.Duration
	// Variables of the profile bws runs with
	Env map[string]string
}

// Key of a pull's cache entries: the token by its fingerprint, the project and the server
func (p Pull) key() string {
	return strings.Join([]string{secrets.Fingerprint(p.Token), p.Project, p.Server}, "|")
}

// Secrets of the project by name, with warnings; a cached pull while it is fresh, a stale one only after a transport failure
func (b *Bws) Pull(ctx context.Context, cfg *config.Config, p Pull) (map[string]string, []string, error) {
	if p.Token == "" {
		return nil, nil, errors.New("No Bitwarden access token is set. Run icebath secrets bitwarden token to add one.")
	}
	if p.Project == "" {
		return nil, nil, errors.New("No Bitwarden project is chosen. Run icebath secrets bitwarden setup to pick one.")
	}
	key := p.key()
	encrypted := func(age time.Duration) (secrets.Entry, bool) { return b.readEncrypted(p.Dir, key, p.Token, age) }
	if p.Cache && p.TTL > 0 {
		var read func() (secrets.Entry, bool)
		if p.Encrypted {
			read = func() (secrets.Entry, bool) { return encrypted(p.TTL) }
		}
		cached, ok, err := b.cache.Lookup(p.Dir, key, p.TTL, read)
		if err != nil {
			return nil, nil, err
		}
		if ok {
			return cached.Secrets, nil, nil
		}
	}
	binary := p.Binary
	if binary == "" {
		binary = b.Find(ctx, cfg, true)
	}
	if binary == "" {
		return nil, nil, errors.New("bws is not installed, and icebath could not download it. Install bws from " +
			"https://github.com/bitwarden/sdk-sm/releases, or run icebath secrets bitwarden setup again.")
	}
	got, warnings, err := b.list(ctx, binary, p)
	if err != nil {
		kind := secrets.Classify(err.Error(), rules)
		if p.Cache && (kind == secrets.Network || kind == secrets.Timeout) {
			var stale secrets.Entry
			found, label := false, ""
			if p.Encrypted {
				stale, found = encrypted(p.MaxStale)
				label = "the encrypted offline copy"
			} else if p.TTL > 0 {
				var readErr error
				if stale, found, readErr = b.cache.Read(p.Dir, key, time.Duration(1<<62)); readErr != nil {
					return nil, nil, readErr
				}
				label = "the copy saved on this computer"
			}
			if found {
				b.cache.Remember(key, stale)
				return stale.Secrets, []string{fmt.Sprintf("Bitwarden could not be reached, so icebath used %s from %s ago. %s", label,
					ago(time.Since(stale.Fetched)), strs.Sentence(err.Error()))}, nil
			}
		}
		return nil, nil, err
	}
	entry := secrets.Entry{Secrets: got, Fetched: time.Now()}
	if p.Cache {
		if p.TTL > 0 {
			b.cache.Remember(key, entry)
		}
		if p.Encrypted {
			b.writeEncrypted(p.Dir, key, p.Token, entry)
		} else {
			b.cache.Write(p.Dir, key, entry, p.TTL)
		}
	}
	return got, warnings, nil
}

// Age said in its largest whole unit, rounded: 1 hour, 3 days
func ago(age time.Duration) string {
	for _, unit := range []time.Duration{24 * time.Hour, time.Hour, time.Minute} {
		if age >= unit {
			return strs.Span(age.Round(unit))
		}
	}
	return strs.Span(max(age, 0).Round(time.Second))
}

// Numbered cause lines of a bws error dump
var causeNumber = regexp.MustCompile(`^\d+:\s*`)

// A bws error dump reduced to its numbered causes, the text itself when it has none
func summarize(raw string) string {
	text := strings.TrimSpace(strings.ReplaceAll(raw, "\x1b", ""))
	var causes []string
	for line := range strings.Lines(text) {
		stripped := strings.TrimSpace(line)
		if strings.HasPrefix(stripped, "Location:") || strings.HasPrefix(stripped, "Backtrace omitted") || strings.HasPrefix(stripped, "Run with ") {
			break
		}
		if stripped == "" || stripped == "Error:" {
			continue
		}
		if cause := causeNumber.ReplaceAllString(stripped, ""); cause != "" {
			causes = append(causes, cause)
		}
	}
	if len(causes) > 0 {
		return strings.Join(causes, "; ")
	}
	return text
}

// Asks bws for the secrets of the project, every one whose key is a variable name
func (b *Bws) list(ctx context.Context, binary string, p Pull) (map[string]string, []string, error) {
	out, err := secrets.Run(ctx, []string{binary, "secret", "list", p.Project, "--output", "json"}, child(p.Env, p.Token, p.Server), runTimeout, "bws",
		fmt.Sprintf("bws did not answer within %s while loading the secrets.", strs.Span(runTimeout)), nil)
	if err != nil {
		return nil, nil, err
	}
	if out.Code != 0 {
		text := out.Stderr
		if text == "" {
			text = out.Stdout
		}
		return nil, nil, fmt.Errorf("bws stopped with exit code %d: %s", out.Code, strs.Sentence(strs.Head(summarize(text), 200)))
	}
	raw := strings.TrimSpace(out.Stdout)
	if raw == "" {
		return map[string]string{}, []string{"bws printed nothing, so the project may have no secrets."}, nil
	}
	var payload any
	if err := json.Unmarshal([]byte(raw), &payload); err != nil {
		return nil, nil, fmt.Errorf("bws printed something icebath can't read: %v.", err)
	}
	items, ok := payload.([]any)
	if !ok {
		return nil, nil, errors.New("bws printed something other than a list of secrets.")
	}
	found := map[string]string{}
	var warnings []string
	for _, item := range items {
		m, ok := item.(map[string]any)
		if !ok {
			continue
		}
		key, keyOK := m["key"].(string)
		value, valueOK := m["value"].(string)
		if !keyOK || !valueOK {
			continue
		}
		if config.EnvName(key) {
			found[key] = value
		} else {
			warnings = append(warnings, fmt.Sprintf("The secret %q was left out because its name can't be a variable name.", key))
		}
	}
	return found, warnings, nil
}

// One project a token sees
type Project struct {
	ID, Name string
}

// Why listing the projects failed, and what that usually means: region for a token of another region, token for a wrong or revoked one
type Refusal struct {
	Text, Cause string
}

// Words of a failed project list and what they usually mean, the first to match winning
var causes = []struct {
	words []string
	cause string
}{
	{[]string{"invalid_client", "400 bad request"}, "region"},
	{[]string{"authorization", "invalid"}, "token"},
}

func cause(text string) string {
	lowered := strings.ToLower(text)
	for _, entry := range causes {
		for _, word := range entry.words {
			if strings.Contains(lowered, word) {
				return entry.cause
			}
		}
	}
	return ""
}

// Projects the token sees on the server, or why Bitwarden would not list them
func (b *Bws) Projects(ctx context.Context, binary, token, server string, env map[string]string) ([]Project, *Refusal) {
	out, err := secrets.Run(ctx, []string{binary, "project", "list", "--output", "json"}, child(env, token, server), projectsTimeout, "bws",
		fmt.Sprintf("bws did not answer within %s while listing the projects.", strs.Span(projectsTimeout)), nil)
	if err != nil {
		return nil, &Refusal{Text: err.Error()}
	}
	if out.Code != 0 {
		text := strings.TrimSpace(out.Stderr)
		if text == "" {
			text = strings.TrimSpace(out.Stdout)
		}
		text = strs.Head(text, 300)
		return nil, &Refusal{Text: "Bitwarden refused to list the projects: " + strs.Sentence(text), Cause: cause(text)}
	}
	raw := strings.TrimSpace(out.Stdout)
	if raw == "" {
		raw = "[]"
	}
	var items []any
	if err := json.Unmarshal([]byte(raw), &items); err != nil {
		var other any
		if json.Unmarshal([]byte(raw), &other) == nil {
			return []Project{}, nil
		}
		return nil, &Refusal{Text: "bws printed a list of projects icebath can't read: " + strs.Sentence(err.Error())}
	}
	projects := []Project{}
	for _, item := range items {
		m, ok := item.(map[string]any)
		if !ok {
			continue
		}
		id := field(m, "id")
		if id == "" || id == "?" {
			continue
		}
		projects = append(projects, Project{ID: id, Name: field(m, "name")})
	}
	return projects, nil
}

// Field of a project as text, a question mark when it has none
func field(m map[string]any, key string) string {
	v, ok := m[key]
	if !ok || v == nil {
		return "?"
	}
	if s, ok := v.(string); ok {
		return s
	}
	return fmt.Sprint(v)
}

// Key of the encrypted cache: HKDF-SHA256 of the access token with the salt
func encryptionKey(token string, salt []byte) []byte {
	key, err := hkdf.Key(sha256.New, []byte(token), salt, encryptedInfo, 32)
	if err != nil {
		panic("bitwarden: hkdf refused a 32 byte key: " + err.Error())
	}
	return key
}

// Keeps an AES-GCM copy of the pull keyed by its token, the plain cache file going once it is written
func (b *Bws) writeEncrypted(dir, key, token string, entry secrets.Entry) {
	salt, nonce := make([]byte, 16), make([]byte, 12)
	rand.Read(salt)
	rand.Read(nonce)
	plain, err := json.Marshal(secrets.Encode(entry))
	if err != nil {
		return
	}
	block, err := aes.NewCipher(encryptionKey(token, salt))
	if err != nil {
		return
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return
	}
	sealed := gcm.Seal(nil, nonce, plain, []byte(key))
	payload := map[string]any{"version": encryptedFormat, "key": key, "salt": base64.StdEncoding.EncodeToString(salt),
		"nonce": base64.StdEncoding.EncodeToString(nonce), "ciphertext": base64.StdEncoding.EncodeToString(sealed)}
	if secrets.WriteJSON(b.encryptedPath(dir), payload) == nil {
		secrets.RemoveFile(b.cache.Path(dir))
	}
}

func (b *Bws) encryptedPath(dir string) string {
	return (&secrets.Cache{Name: encryptedName}).Path(dir)
}

// The encrypted copy of the key when the token opens it and it is no older than age, never for no age
func (b *Bws) readEncrypted(dir, key, token string, age time.Duration) (secrets.Entry, bool) {
	if age <= 0 {
		return secrets.Entry{}, false
	}
	data, err := os.ReadFile(b.encryptedPath(dir))
	if err != nil {
		return secrets.Entry{}, false
	}
	var payload map[string]any
	if json.Unmarshal(bytes.TrimPrefix(data, []byte("\xef\xbb\xbf")), &payload) != nil {
		return secrets.Entry{}, false
	}
	if version, _ := payload["version"].(float64); int(version) != encryptedFormat {
		return secrets.Entry{}, false
	}
	if stored, _ := payload["key"].(string); stored != key {
		return secrets.Entry{}, false
	}
	decode := func(name string) ([]byte, bool) {
		text, _ := payload[name].(string)
		out, err := base64.StdEncoding.Strict().DecodeString(text)
		return out, err == nil
	}
	salt, okSalt := decode("salt")
	nonce, okNonce := decode("nonce")
	sealed, okSealed := decode("ciphertext")
	if !okSalt || !okNonce || !okSealed {
		return secrets.Entry{}, false
	}
	block, err := aes.NewCipher(encryptionKey(token, salt))
	if err != nil {
		return secrets.Entry{}, false
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil || len(nonce) != gcm.NonceSize() {
		return secrets.Entry{}, false
	}
	plain, err := gcm.Open(nil, nonce, sealed, []byte(key))
	if err != nil {
		return secrets.Entry{}, false
	}
	var decoded map[string]any
	if json.Unmarshal(plain, &decoded) != nil {
		return secrets.Entry{}, false
	}
	entry, ok := secrets.Decode(decoded)
	if !ok {
		return secrets.Entry{}, false
	}
	if since := time.Since(entry.Fetched); since < 0 || since > age {
		return secrets.Entry{}, false
	}
	return entry, true
}

// Forgets every pull kept, in this process and in both files, after a new token
func (b *Bws) Clear(dir string) {
	b.cache.Clear(dir)
	secrets.RemoveFile(b.encryptedPath(dir))
}
