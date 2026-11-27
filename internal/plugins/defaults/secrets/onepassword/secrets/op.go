package secrets

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"time"

	"github.com/nickheyer/icebath/internal/config"
	onepasswordconfig "github.com/nickheyer/icebath/internal/plugins/defaults/secrets/onepassword/config"
	"github.com/nickheyer/icebath/internal/secrets"
	"github.com/nickheyer/icebath/pkg/strs"
	"github.com/nickheyer/icebath/pkg/term"
)

const (
	// Longest one op call may take
	runTimeout = 30 * time.Second
	// Longest the identity check may take
	whoamiTimeout = 10 * time.Second
	cacheName     = "op_cache.json"
)

// Variables of the profile an op process sees, besides its sessions and its token
var Allowed = []string{"PATH", "HOME", "USERPROFILE", "APPDATA", "LOCALAPPDATA", "SystemRoot", "TMPDIR", "TMP", "TEMP", "XDG_CONFIG_HOME",
	"XDG_RUNTIME_DIR", "OP_CONFIG_DIR", "OP_ACCOUNT", "OP_CONNECT_HOST", "OP_CONNECT_TOKEN", "OP_LOAD_DESKTOP_APP_SETTINGS"}

// Kinds of an op failure by the words of its error, the first rule to match winning
var rules = []secrets.Rule{
	{Kind: secrets.Timeout, Words: []string{"timed out", "did not answer within"}},
	{Kind: secrets.BinaryMissing, Words: []string{"is not installed", "not a program icebath can run", "can't start"}},
	{Kind: secrets.AuthFailed, Words: []string{"unauthorized", "not signed in", "session expired", "authentication", "401", "403"}},
	{Kind: secrets.EmptyValue, Words: []string{"empty value"}},
	{Kind: secrets.Network, Words: []string{"network", "connection", "resolve host", "dns"}},
}

// The op program: the path pinned, used as it is when it is an executable and never looked up instead, else the one on the search path;
// empty when there is none
func Find(pinned string) string {
	found := strings.TrimSpace(pinned)
	if found == "" {
		if path, err := exec.LookPath("op"); err == nil {
			found = path
		}
	} else if info, err := os.Stat(found); err != nil || info.IsDir() || (runtime.GOOS != "windows" && info.Mode().Perm()&0o111 == 0) {
		return ""
	}
	if found == "" {
		return ""
	}
	return filepath.Clean(found)
}

// Text of an op failure without its escape sequences
func Scrub(text string) string {
	return strings.TrimSpace(strings.ReplaceAll(term.StripANSI(text), "\x1b", ""))
}

// Environment of one op call: the allowed variables and the sessions of the environment given, the token when there is one, no color
func Child(env map[string]string, token string) map[string]string {
	out := map[string]string{"NO_COLOR": "1"}
	for _, name := range Allowed {
		if value, ok := env[name]; ok {
			out[name] = value
		}
	}
	for name, value := range env {
		if strings.HasPrefix(name, "OP_SESSION_") {
			out[name] = value
		}
	}
	if token != "" {
		out["OP_SERVICE_ACCOUNT_TOKEN"] = token
	}
	return out
}

// Variables bound to op:// references, the pairs that are not with a warning each
func Validate(references map[string]string) (map[string]string, []string) {
	valid := map[string]string{}
	var warnings []string
	for _, name := range slices.Sorted(maps.Keys(references)) {
		reference := strings.TrimSpace(references[name])
		switch {
		case !config.EnvName(name):
			warnings = append(warnings, fmt.Sprintf("%q can't be the name of a variable, so it was left out. A name is letters, digits and "+
				"underscores, and does not start with a digit.", name))
		case !strings.HasPrefix(reference, "op://"):
			warnings = append(warnings, fmt.Sprintf("%s was left out because %q is not a 1Password reference. A reference starts with op://, "+
				"such as op://Private/OpenAI/credential.", name, references[name]))
		default:
			valid[name] = reference
		}
	}
	return valid, warnings
}

// Variables the profile binds to references, its settings keeping their names in lower case and the variables named here in upper case
func References(cfg *config.Config) map[string]string {
	out := map[string]string{}
	table, _ := cfg.Get(onepasswordconfig.References).(map[string]any)
	for name, value := range table {
		text, ok := value.(string)
		if !ok {
			text = fmt.Sprint(value)
		}
		out[strings.ToUpper(name)] = text
	}
	return out
}

// Fingerprint of everything op would sign in with, so another identity never reads values cached for this one
func identity(env map[string]string, tokenEnv string) string {
	parts := []string{"token=" + env[tokenEnv], "account=" + env["OP_ACCOUNT"], "connect_host=" + env["OP_CONNECT_HOST"], "connect_token=" + env["OP_CONNECT_TOKEN"]}
	for _, name := range slices.Sorted(maps.Keys(env)) {
		if strings.HasPrefix(name, "OP_SESSION_") {
			parts = append(parts, name+"="+env[name])
		}
	}
	return secrets.Fingerprint(strings.Join(parts, "\n"))
}

func bindings(references map[string]string) string {
	var parts []string
	for _, name := range slices.Sorted(maps.Keys(references)) {
		parts = append(parts, name+"="+references[name])
	}
	return secrets.Fingerprint(strings.Join(parts, "\n"))
}

// Value of one reference; any failure, an empty value op exits 0 with included, is an error
func read(ctx context.Context, op, reference, account, token string, env map[string]string) (string, error) {
	argv := []string{op, "read"}
	if account != "" {
		argv = append(argv, "--account", account)
	}
	argv = append(argv, "--", reference)
	out, err := secrets.Run(ctx, argv, Child(env, token), runTimeout, "op", fmt.Sprintf("op did not answer within %s while reading %s.",
		strs.Span(runTimeout), reference), nil)
	if err != nil {
		return "", err
	}
	if out.Code != 0 {
		if text := strs.Head(Scrub(out.Stderr), 200); text != "" {
			return "", fmt.Errorf("1Password could not read %s: %s", reference, secrets.Sentence(text))
		}
		return "", fmt.Errorf("op stopped with exit code %d while reading %s.", out.Code, reference)
	}
	value := strings.TrimRight(out.Stdout, "\r\n")
	if strings.TrimSpace(value) == "" {
		return "", fmt.Errorf("1Password gave an empty value for %s.", reference)
	}
	return value, nil
}

// One resolution of references
type Pull struct {
	References map[string]string
	Account    string
	TokenEnv   string
	// op program, Pinned found for none
	Binary, Pinned string
	// Folder of the profile the cache is kept in, the seconds it is reused for and whether it is read and written at all
	Dir   string
	TTL   time.Duration
	Cache bool
	// Variables of the profile op runs with
	Env map[string]string
}

// The 1Password store, which the source, the service and the commands of the plugin share
type Store struct {
	registry *secrets.Registry
	cache    *secrets.Cache
}

// Values of the valid references by variable, with warnings for the pairs that are not and the reads that failed; only a pull with every
// read working is cached
func (s *Store) Pull(ctx context.Context, p Pull) (map[string]string, []string, error) {
	valid, warnings := Validate(p.References)
	if len(valid) == 0 {
		return map[string]string{}, warnings, nil
	}
	tokenEnv := p.TokenEnv
	if tokenEnv == "" {
		tokenEnv = onepasswordconfig.DefaultTokenEnv
	}
	token := strings.TrimSpace(p.Env[tokenEnv])
	key := fmt.Sprintf("%s|%s|%s", identity(p.Env, tokenEnv), p.Account, bindings(valid))
	if p.Cache {
		cached, ok, err := s.cache.Lookup(p.Dir, key, p.TTL, nil)
		if err != nil {
			return nil, nil, err
		}
		if ok {
			return maps.Clone(cached.Secrets), warnings, nil
		}
	}
	op := p.Binary
	if op == "" {
		op = Find(p.Pinned)
	}
	if op == "" {
		return nil, warnings, errors.New("op, the 1Password command line tool, is not installed. " + InstallHint)
	}
	got := map[string]string{}
	failed := 0
	for _, name := range slices.Sorted(maps.Keys(valid)) {
		value, err := read(ctx, op, valid[name], p.Account, token, p.Env)
		if err != nil {
			warnings = append(warnings, err.Error())
			failed++
			continue
		}
		got[name] = value
	}
	if p.Cache && failed == 0 && len(got) > 0 {
		s.cache.Store(p.Dir, key, secrets.Entry{Secrets: maps.Clone(got), Fetched: time.Now()}, p.TTL)
	}
	return got, warnings, nil
}

// Forgets every resolution kept, after a new token
func (s *Store) Clear(dir string) {
	s.cache.Clear(dir)
}

// Who op signs in as, with the token given, else the one of the environment or its signed in session; false when it is not signed in
func Whoami(ctx context.Context, op, account, token string, env map[string]string) (string, bool) {
	argv := []string{op, "whoami"}
	if account != "" {
		argv = append(argv, "--account", account)
	}
	child := maps.Clone(env)
	if child == nil {
		child = map[string]string{}
	}
	child["NO_COLOR"] = "1"
	if token != "" {
		child["OP_SERVICE_ACCOUNT_TOKEN"] = token
	}
	out, err := secrets.Run(ctx, argv, child, whoamiTimeout, "op", "op did not say who it is signed in as within "+strs.Span(whoamiTimeout)+".", nil)
	if err != nil || out.Code != 0 {
		return "", false
	}
	who := strs.Head(strings.ReplaceAll(strings.TrimSpace(out.Stdout), "\n", " "), 120)
	if who == "" {
		return "an unnamed account", true
	}
	return who, true
}
