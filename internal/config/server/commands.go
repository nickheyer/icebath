package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"math"
	"math/big"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"unicode"

	"connectrpc.com/connect/v2"
	"go.yaml.in/yaml/v3"

	"github.com/nickheyer/icebath/internal/config"
	"github.com/nickheyer/icebath/internal/provider"
	"github.com/nickheyer/icebath/internal/server"
	skillsconfig "github.com/nickheyer/icebath/internal/skills/config"
	toolsconfig "github.com/nickheyer/icebath/internal/tools/config"
	"github.com/nickheyer/icebath/internal/tools/diff"
	"github.com/nickheyer/icebath/pkg/fsx"
	configv1 "github.com/nickheyer/icebath/pkg/gen/icebath/config/v1"
)

// Name of a variable as config set takes one: upper case letters, digits and underscores
var envShape = regexp.MustCompile(`^[A-Z][A-Z0-9_]*$`)

// Endings and starts of names that make a key without dots a variable of the .env
var (
	envSuffixes = []string{"_API_KEY", "_TOKEN", "_SECRET"}
	envPrefixes = []string{"TERMINAL_SSH"}
)

// What makes the last part of a key name a credential, and what makes it not one
var (
	secretNames       = []string{"api_key", "apikey", "key", "token", "access_token", "refresh_token", "id_token", "secret", "client_secret", "password", "passwd", "authorization", "private_key", "bearer", "jwt"}
	secretEndings     = []string{"_api_key", "_token", "_secret", "_password", "_key", "_access_key"}
	nonSecretEndings  = []string{"_url", "_host", "_user", "_id", "_domain", "_scheme"}
	displayControlsRe = regexp.MustCompile(`[\x00-\x1f\x7f\x{80}-\x{9f}\x{200b}-\x{200f}\x{202a}-\x{202e}\x{2060}-\x{2064}]`)
)

// Credentials of the services icebath reaches that config set keeps in the .env by name, beside those shaped like one
var credentialKeys = []string{"OPENROUTER_API_KEY", "OPENAI_API_KEY", "ANTHROPIC_API_KEY", "VOICE_TOOLS_OPENAI_KEY", "EXA_API_KEY", "PARALLEL_API_KEY",
	"FIRECRAWL_API_KEY", "FIRECRAWL_API_URL", "TAVILY_API_KEY", "PERPLEXITY_API_KEY", "API_SERVER_KEY", "BROWSERBASE_API_KEY", "BROWSERBASE_PROJECT_ID",
	"BROWSER_USE_API_KEY", "FAL_KEY", "TELEGRAM_BOT_TOKEN", "DISCORD_BOT_TOKEN", "TERMINAL_SSH_HOST", "TERMINAL_SSH_USER", "TERMINAL_SSH_KEY",
	"SUDO_PASSWORD", "SLACK_BOT_TOKEN", "SLACK_APP_TOKEN", "GITHUB_TOKEN"}

// Whether a key without dots names a credential config set keeps in the .env, which it says as typed
func credentialKey(key string) bool {
	if strings.Contains(key, ".") {
		return false
	}
	upper := strings.ToUpper(key)
	return slices.Contains(credentialKeys, upper) || endsWith(upper, envSuffixes) ||
		slices.ContainsFunc(envPrefixes, func(p string) bool { return strings.HasPrefix(upper, p) })
}

func endsWith(text string, endings []string) bool {
	return slices.ContainsFunc(endings, func(ending string) bool { return strings.HasSuffix(text, ending) })
}

// Whether a key names a variable of the profile's .env rather than a setting: no dots, and shaped like a variable or read by a setting
func (s *service) envKey(cfg *config.Config, key string) bool {
	if strings.Contains(key, ".") || !config.EnvName(key) {
		return false
	}
	upper := strings.ToUpper(key)
	if envShape.MatchString(key) || credentialKey(key) {
		return true
	}
	for _, section := range cfg.Sections() {
		for _, setting := range section.Settings {
			if slices.Contains(setting.Env, upper) {
				return true
			}
		}
	}
	return false
}

// Whether the last part of a key names a credential
func secretKey(key string, env bool) bool {
	leaf := key[strings.LastIndex(key, ".")+1:]
	leaf = strings.ReplaceAll(strings.ToLower(leaf), "-", "_")
	if env {
		return !endsWith(leaf, nonSecretEndings)
	}
	return slices.Contains(secretNames, leaf) || endsWith(leaf, secretEndings)
}

// Secret shown by its first and last four characters, *** when it is shorter than twelve
func mask(value string) string {
	value = displayControlsRe.ReplaceAllString(value, "")
	if value == "" {
		return ""
	}
	runes := []rune(value)
	if len(runes) < 12 {
		return "***"
	}
	return string(runes[:4]) + "..." + string(runes[len(runes)-4:])
}

// Value with the credentials inside it masked, references to variables kept as they are
func redacted(value any, secret bool) any {
	switch v := value.(type) {
	case string:
		if secret && v != "" && !config.Reference(v) {
			return mask(v)
		}
		return v
	case map[string]any:
		out := make(map[string]any, len(v))
		for key, item := range v {
			out[key] = redacted(item, secretKey(key, false))
		}
		return out
	case []any:
		out := make([]any, len(v))
		for i, item := range v {
			out[i] = redacted(item, false)
		}
		return out
	}
	return value
}

// Value of a setting with the credentials inside it masked: its text when the setting or its name says it is one, and in a table or a list
// of records each entry by what the setting declares of its entries
func shielded(setting config.Setting, value any, secret bool) any {
	switch held := value.(type) {
	case map[string]any:
		if setting.Kind() != config.Table {
			break
		}
		out := make(map[string]any, len(held))
		for name, entry := range held {
			out[name] = entryShielded(setting, entry, secretKey(name, false))
		}
		return out
	case []any:
		if setting.Kind() != config.List {
			break
		}
		out := make([]any, len(held))
		for i, record := range held {
			out[i] = entryShielded(setting, record, secret)
		}
		return out
	}
	return redacted(value, secret || setting.Secret)
}

// One entry of a table, or one record of a list, with the credentials inside it masked: the one value it is, or each of its fields, by
// what the setting declares of them and by their names
func entryShielded(table config.Setting, entry any, secret bool) any {
	var fields []config.Setting
	if table.Entry != nil {
		fields = table.Entry.Fields
	}
	if len(fields) == 1 && fields[0].Key == "" {
		return shielded(fields[0], entry, secret || table.Secret)
	}
	held, ok := entry.(map[string]any)
	if !ok {
		return redacted(entry, secret || table.Secret)
	}
	out := make(map[string]any, len(held))
	for name, item := range held {
		named := table.Secret || secretKey(name, false)
		if at := fieldAt(fields, name); at >= 0 {
			out[name] = shielded(fields[at], item, named)
		} else {
			out[name] = redacted(item, named)
		}
	}
	return out
}

// Place of the named field the name answers to among the fields of an entry, as a declared key answers in any case, -1 when none does
func fieldAt(fields []config.Setting, name string) int {
	return slices.IndexFunc(fields, func(field config.Setting) bool { return field.Key != "" && strings.EqualFold(field.Key, name) })
}

// Part of a table's value under a path inside it with the credentials inside it masked as the table declares them: one entry, a field
// of one, or a part of a table one of those is
func partShielded(table config.Setting, path string, value any, secret bool) any {
	name, rest, more := strings.Cut(path, ".")
	var fields []config.Setting
	if table.Entry != nil {
		fields = table.Entry.Fields
	}
	part := -1
	switch {
	case len(fields) == 1 && fields[0].Key == "":
		part = 0
	case !more:
		return entryShielded(table, value, secretKey(name, false))
	default:
		name, rest, more = strings.Cut(rest, ".")
		part = fieldAt(fields, name)
	}
	switch {
	case part < 0:
		return redacted(value, secret || table.Secret)
	case !more:
		return shielded(fields[part], value, secret || table.Secret)
	case fields[part].Kind() == config.Table:
		return partShielded(fields[part], rest, value, secret)
	}
	return redacted(value, secret || table.Secret)
}

// Value as YAML lists and tables are written in a file, scalars on their own
func block(value any) (string, error) {
	switch v := value.(type) {
	case nil:
		return "null", nil
	case bool:
		return strconv.FormatBool(v), nil
	case string:
		return v, nil
	case map[string]any, []any, []string:
		data, err := yaml.Marshal(v)
		return strings.TrimRight(string(data), " \t\r\n"), err
	}
	return fmt.Sprint(value), nil
}

// Refusal of a key no setting names, saying a write it asked for was not made, with the setting nearest to it when one is near
func (s *service) unknown(cfg *config.Config, key string, write bool) error {
	var keys []string
	for _, section := range cfg.Sections() {
		for _, setting := range section.Settings {
			keys = append(keys, setting.Key)
		}
	}
	slices.Sort(keys)
	message := "There is no setting named " + key + "."
	if write {
		message += " Nothing was changed."
	}
	if near := diff.Close(key, keys, 1, 0.6); len(near) > 0 {
		message += " Did you mean " + near[0] + "?"
	}
	return connect.NewError(connect.CodeNotFound, message)
}

// Refusal of a key with spaces around it or an empty part between its dots
func spelling(key string) error {
	trimmed := strings.TrimFunc(key, unicode.IsSpace)
	if key != trimmed || trimmed == "" {
		return connect.NewError(connect.CodeInvalidArgument, fmt.Sprintf("%q can't be the key of a setting. A key is not empty and has no spaces around it.", key))
	}
	if slices.Contains(strings.Split(key, "."), "") {
		return connect.NewError(connect.CodeInvalidArgument, fmt.Sprintf("%q can't be the key of a setting. Check for a dot at its start or end, or two "+
			"dots in a row.", key))
	}
	return nil
}

func (s *service) Value(_ context.Context, request *configv1.ValueRequest) (*configv1.ValueResponse, error) {
	cfg, err := server.Profile(s.config, request.GetProfile())
	if err != nil {
		return nil, err
	}
	key := request.GetKey()
	var value any
	found, secret, note := false, false, ""
	_, setting, known := cfg.Setting(key)
	table, path, under := config.Setting{}, "", false
	if !known {
		table, path, under = cfg.Under(key)
	}
	// What masks the credentials of the value: its name alone, unless a setting declares what it holds
	shield := func(value any) any { return redacted(value, secret) }
	switch {
	case s.envKey(cfg, key):
		value, found = cfg.LookupEnv(strings.ToUpper(key))
		stale := cfg.Stored(key)
		if _, table := stale.(map[string]any); !found && stale != nil && !table {
			value, found = fmt.Sprint(stale), true
			note = fmt.Sprintf("%s comes from an old copy in the settings file. `icebath config set %s <value>` moves it to the keys file, and `icebath "+
				"config unset %s` removes it.", key, key, key)
		}
		secret = secretKey(key, true)
	case known:
		value, found, secret = cfg.Get(setting.Key), true, secretKey(setting.Key, false)
		shield = func(value any) any { return shielded(setting, value, secret) }
	case under:
		value, secret = cfg.Get(key), secretKey(key, false)
		found = value != nil
		shield = func(value any) any { return partShielded(table, path, value, secret) }
	case cfg.Stored(key) != nil:
		value, found = cfg.Get(key), true
		secret = secretKey(key, false)
	default:
		return nil, s.unknown(cfg, key, false)
	}
	if !found {
		return &configv1.ValueResponse{}, nil
	}
	if !request.GetRaw() && cfg.GetBool(toolsconfig.RedactSecrets) {
		value = shield(value)
	}
	encoded, err := json.Marshal(value)
	if err != nil {
		return nil, err
	}
	text, err := block(value)
	if err != nil {
		return nil, err
	}
	return &configv1.ValueResponse{Found: true, Json: string(encoded), Yaml: text, Note: note}, nil
}

// Removes the copy of a variable kept at the top of the profile's config.yaml, found in any case, so its .env holds the only one; whether
// there was one. A table under the name is a section of settings, which stays
func dropCopies(cfg *config.Config, key string) (bool, error) {
	if _, table := cfg.Stored(key).(map[string]any); table || cfg.Global(key) || !cfg.Holds(key) {
		return false, nil
	}
	return true, cfg.Set(key, nil)
}

// Integer of decimal text with an optional sign and single underscores between digits, as YAML writes one
func integer(text string) (any, bool) {
	sign, digits := "", text
	if digits != "" && (digits[0] == '+' || digits[0] == '-') {
		sign, digits = digits[:1], digits[1:]
	}
	if digits == "" || digits[0] == '_' || digits[len(digits)-1] == '_' || strings.Contains(digits, "__") || strings.Trim(digits, "0123456789_") != "" {
		return nil, false
	}
	n, ok := new(big.Int).SetString(sign+strings.ReplaceAll(digits, "_", ""), 10)
	if !ok || !n.IsInt64() || n.Int64() < math.MinInt || n.Int64() > math.MaxInt {
		return nil, false
	}
	return int(n.Int64()), true
}

// Words that stand for yes, no and nothing
var scalars = map[string]any{"true": true, "yes": true, "on": true, "false": false, "no": false, "off": false, "null": nil, "none": nil, "~": nil}

// Whether text looks like a list or a mapping written inline
func structured(text string) bool {
	trimmed := strings.TrimSpace(text)
	return strings.HasPrefix(trimmed, "[") || strings.HasPrefix(trimmed, "{")
}

// Value text typed for a setting stands for: text as it is for a setting of text, else yes, no, nothing, a number, or a list or mapping
// written in YAML or JSON
func typed(setting config.Setting, known bool, text string) (any, error) {
	if known && (setting.Kind() == config.Text || setting.Kind() == config.Choice) {
		return text, nil
	}
	stripped := strings.TrimSpace(text)
	if value, ok := scalars[strings.ToLower(stripped)]; ok {
		return value, nil
	}
	if value, ok := integer(stripped); ok {
		return value, nil
	}
	if f, err := strconv.ParseFloat(stripped, 64); err == nil && !math.IsNaN(f) && !math.IsInf(f, 0) {
		return f, nil
	}
	if !structured(text) {
		if known && setting.Kind() == config.List {
			var items []any
			for _, item := range strings.Split(text, ",") {
				if item = strings.TrimSpace(item); item != "" {
					items = append(items, item)
				}
			}
			return items, nil
		}
		return text, nil
	}
	var parsed any
	if err := yaml.Unmarshal([]byte(text), &parsed); err != nil {
		detail, _, _ := strings.Cut(strings.TrimPrefix(err.Error(), "yaml: "), "\n")
		return nil, fmt.Errorf("The value for %s can't be read as a list or a set of named values: %s. Nothing was changed, so fix the value, or put "+
			"it in quotes to save it as plain text.", setting.Key, detail)
	}
	switch parsed.(type) {
	case []any, map[string]any:
		return parsed, nil
	}
	return text, nil
}

func (s *service) Put(_ context.Context, request *configv1.PutRequest) (*configv1.PutResponse, error) {
	cfg, err := server.Profile(s.config, request.GetProfile())
	if err != nil {
		return nil, err
	}
	key, text := request.GetKey(), request.GetValue()
	if system := cfg.ManagedSystem(); system != "" {
		return nil, connect.NewError(connect.CodePermissionDenied, (&config.LockedError{By: config.ByPackageManager, Source: system}).Refusal())
	}
	if err := spelling(key); err != nil {
		return nil, err
	}
	if locked := cfg.Lock(key); locked != nil {
		return nil, connect.NewError(connect.CodePermissionDenied, locked.Refusal())
	}
	if s.envKey(cfg, key) {
		name := strings.ToUpper(key)
		value, warning := config.NonASCII(name, strings.NewReplacer("\r", "", "\n", "").Replace(text))
		old := cfg.Dotenv()[name]
		if err := cfg.SaveEnv(name, value); err != nil {
			return nil, connect.NewError(connect.CodeInvalidArgument, err.Error())
		}
		if _, err := dropCopies(cfg, key); err != nil {
			return nil, connect.NewError(connect.CodeFailedPrecondition, err.Error())
		}
		if credentialKey(key) {
			if err := s.credentials.Saved(cfg, name, old, value); err != nil {
				return nil, connect.NewError(connect.CodeInvalidArgument, err.Error())
			}
		}
		shown := name
		if credentialKey(key) {
			shown = key
		}
		return &configv1.PutResponse{Key: shown, Destination: cfg.EnvPath(), Env: true, Warning: warning}, nil
	}
	_, setting, known := cfg.Setting(key)
	verbatim := known
	if !known {
		table, path, under := cfg.Under(key)
		if !under {
			return nil, s.unknown(cfg, key, true)
		}
		if part, declared := table.Part(path); declared {
			// A part the table declares, the value of an entry or a field of one, is read as the field it is, so text stays as typed; a
			// declared field of one entry is checked as that field too, as a write from the app is
			setting, verbatim = part, true
			setting.Secret = setting.Secret || table.Secret
			_, known = table.EntryField(path)
		} else {
			// Any other part of a table setting takes any value, text kept as typed where the table's default holds text
			setting = config.Setting{Key: cfg.Key(key), Default: config.Dig(table.DefaultValue(), path), Secret: table.Secret}
			_, verbatim = setting.Default.(string)
		}
	}
	value, err := typed(setting, verbatim, text)
	if err != nil {
		return nil, connect.NewError(connect.CodeInvalidArgument, err.Error())
	}
	if known {
		if value, err = setting.Check(value); err != nil {
			return nil, connect.NewError(connect.CodeInvalidArgument, err.Error())
		}
		if value, err = setting.Located(value); err != nil {
			return nil, connect.NewError(connect.CodeInvalidArgument, err.Error())
		}
	}
	if err := cfg.Change(setting.Key, value); err != nil {
		if locked, ok := errors.AsType[*config.LockedError](err); ok {
			return nil, connect.NewError(connect.CodePermissionDenied, locked.Refusal())
		}
		return nil, connect.NewError(connect.CodeFailedPrecondition, err.Error())
	}
	shown := shielded(setting, cfg.Stored(setting.Key), secretKey(setting.Key, false))
	words, err := encode(shown)
	if err != nil {
		return nil, err
	}
	if shown == nil {
		words = "null"
	}
	destination, err := cfg.Destination(setting.Key)
	if err != nil {
		return nil, err
	}
	return &configv1.PutResponse{Key: setting.Key, Shown: words, Destination: destination}, nil
}

func (s *service) Unset(_ context.Context, request *configv1.UnsetRequest) (*configv1.UnsetResponse, error) {
	cfg, err := server.Profile(s.config, request.GetProfile())
	if err != nil {
		return nil, err
	}
	key := request.GetKey()
	if system := cfg.ManagedSystem(); system != "" {
		return nil, connect.NewError(connect.CodePermissionDenied, (&config.LockedError{By: config.ByPackageManager, Source: system}).Refusal())
	}
	if locked := cfg.Lock(key); locked != nil {
		return nil, connect.NewError(connect.CodePermissionDenied, locked.Refusal())
	}
	if s.envKey(cfg, key) {
		name := strings.ToUpper(key)
		old := cfg.Dotenv()[name]
		removed, err := cfg.RemoveEnv(name)
		if err != nil {
			return nil, connect.NewError(connect.CodeInvalidArgument, err.Error())
		}
		dropped, err := dropCopies(cfg, key)
		if err != nil {
			return nil, connect.NewError(connect.CodeFailedPrecondition, err.Error())
		}
		scrubbed := false
		if credentialKey(key) {
			if scrubbed, err = s.credentials.Removed(cfg, name, old); err != nil {
				return nil, connect.NewError(connect.CodeInvalidArgument, err.Error())
			}
		}
		return &configv1.UnsetResponse{Found: removed || dropped || scrubbed, Destination: cfg.EnvPath()}, nil
	}
	if _, setting, known := cfg.Setting(key); known {
		key = setting.Key
	} else if _, _, under := cfg.Under(key); !under && cfg.Stored(key) == nil {
		return nil, s.unknown(cfg, key, true)
	}
	destination, err := cfg.Destination(key)
	if err != nil {
		return nil, err
	}
	// A config.yaml that does not read or parse is refused before anything else is said of the key, as removing it would replace the file
	if err := cfg.Writable(key); err != nil {
		return nil, connect.NewError(connect.CodeFailedPrecondition, err.Error())
	}
	if !cfg.Holds(key) {
		return &configv1.UnsetResponse{Destination: destination}, nil
	}
	if err := cfg.Change(key, nil); err != nil {
		return nil, connect.NewError(connect.CodeFailedPrecondition, err.Error())
	}
	return &configv1.UnsetResponse{Found: true, Destination: destination}, nil
}

func (s *service) Files(_ context.Context, request *configv1.FilesRequest) (*configv1.FilesResponse, error) {
	cfg, err := server.Profile(s.config, request.GetProfile())
	if err != nil {
		return nil, err
	}
	shared, own := cfg.Files()
	host, _ := os.Hostname()
	response := &configv1.FilesResponse{Shared: shared, Own: own, Env: cfg.EnvPath(), Profile: cfg.Profile(), ManagedDir: config.ManagedDir(),
		ManagedKeys: cfg.ManagedKeys(), ManagedSystem: cfg.ManagedSystem(), Remote: cfg.RemoteState(), Host: host}
	if own != "" {
		response.Backups, response.OwnExists = config.BackupsDir(own), fsx.Exists(own)
	}
	response.EnvExists = fsx.Exists(response.Env)
	for name := range cfg.ManagedEnv() {
		response.ManagedEnv = append(response.ManagedEnv, name)
	}
	slices.Sort(response.ManagedEnv)
	return response, nil
}

// Path of one of the profile's config files, refused for any other
func (s *service) file(cfg *config.Config, path string) (string, error) {
	shared, own := cfg.Files()
	if path != shared && (own == "" || path != own) {
		return "", connect.NewError(connect.CodePermissionDenied, path+" is not a settings file of this profile.")
	}
	return path, nil
}

func (s *service) Read(_ context.Context, request *configv1.ReadRequest) (*configv1.ReadResponse, error) {
	cfg, err := server.Profile(s.config, request.GetProfile())
	if err != nil {
		return nil, err
	}
	path, err := s.file(cfg, request.GetPath())
	if err != nil {
		return nil, err
	}
	data, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return &configv1.ReadResponse{}, nil
	}
	if err != nil {
		return nil, err
	}
	return &configv1.ReadResponse{Text: string(data), Exists: true}, nil
}

// Saves an edited config file and reads the settings again, saying what is wrong with it when it does not parse
func (s *service) Write(_ context.Context, request *configv1.WriteRequest) (*configv1.WriteResponse, error) {
	cfg, err := server.Profile(s.config, request.GetProfile())
	if err != nil {
		return nil, err
	}
	if locked := cfg.EditLock(); locked != nil {
		return nil, connect.NewError(connect.CodePermissionDenied, locked.Refusal())
	}
	path, err := s.file(cfg, request.GetPath())
	if err != nil {
		return nil, err
	}
	if err := fsx.Write(path, []byte(request.GetText()), 0o600); err != nil {
		return nil, err
	}
	return &configv1.WriteResponse{Problem: problem([]byte(request.GetText()))}, nil
}

// Makes a config file of the profile that is not there yet from the example of its scope; an install a package manager keeps is refused
func (s *service) Seed(_ context.Context, request *configv1.SeedRequest) (*configv1.SeedResponse, error) {
	cfg, err := server.Profile(s.config, request.GetProfile())
	if err != nil {
		return nil, err
	}
	if locked := cfg.EditLock(); locked != nil {
		return nil, connect.NewError(connect.CodePermissionDenied, locked.Refusal())
	}
	path, err := s.file(cfg, request.GetPath())
	if err != nil {
		return nil, err
	}
	if fsx.Exists(path) {
		return &configv1.SeedResponse{}, nil
	}
	shared, _ := cfg.Files()
	text, err := cfg.Example(path == shared)
	if err != nil {
		return nil, err
	}
	if err := fsx.Write(path, text, 0o600); err != nil {
		return nil, err
	}
	return &configv1.SeedResponse{Seeded: true}, nil
}

// What is wrong with the text of an edited config file, empty when it parses to a mapping of settings; the settings themselves are read
// again at their next read, as the version of the file changed
func problem(text []byte) string {
	if err := config.Problem(text); err != nil {
		return strings.TrimPrefix(err.Error(), "yaml: ")
	}
	return ""
}

func (s *service) Check(_ context.Context, request *configv1.CheckRequest) (*configv1.CheckResponse, error) {
	cfg, err := server.Profile(s.config, request.GetProfile())
	if err != nil {
		return nil, err
	}
	response := &configv1.CheckResponse{}
	index := map[string]*configv1.Variable{}
	// The key variables of the providers signed in to with a key, then those the settings read, each once
	for _, spec := range s.providers.Specs() {
		if spec.AuthType != provider.AuthTypeAPIKey {
			continue
		}
		for _, name := range append(slices.Clone(spec.KeyEnv), spec.BaseURLEnv) {
			if name == "" || index[name] != nil {
				continue
			}
			key := !strings.HasSuffix(name, "_URL")
			about := spec.Label() + " base URL"
			if key {
				about = spec.Label() + " API key"
			}
			variable := &configv1.Variable{Name: name, Set: cfg.Getenv(name) != "", Secret: key, About: about}
			index[name] = variable
			response.Optional = append(response.Optional, variable)
		}
	}
	for _, section := range cfg.Sections() {
		for _, setting := range section.Settings {
			if setting.Internal {
				continue
			}
			for _, name := range setting.Env {
				variable := index[name]
				if variable == nil {
					variable = &configv1.Variable{Name: name, Set: cfg.Getenv(name) != ""}
					index[name] = variable
					response.Optional = append(response.Optional, variable)
				}
				variable.Settings = append(variable.Settings, setting.Key)
				variable.Secret = variable.Secret || setting.Secret
			}
		}
	}
	// A config file that does not parse is said first, as everything it holds is left out until it is fixed
	shared, own := cfg.Files()
	for _, path := range []string{shared, own} {
		data, err := os.ReadFile(path)
		if path == "" || errors.Is(err, fs.ErrNotExist) {
			continue
		}
		if err != nil {
			response.Diagnostics = append(response.Diagnostics, fmt.Sprintf("%s can't be read: %v.", path, err))
		} else if trouble := problem(data); trouble != "" {
			response.Diagnostics = append(response.Diagnostics, fmt.Sprintf("%s has a formatting error: %s. Until it is fixed, icebath uses the last "+
				"settings that worked.", path, trouble))
		}
	}
	response.Diagnostics = append(response.Diagnostics, s.inactive(cfg)...)
	return response, nil
}

// Credentials config show lists by the variable holding each, the Anthropic key after them
var shownKeys = [][2]string{{"OPENROUTER_API_KEY", "OpenRouter"}, {"VOICE_TOOLS_OPENAI_KEY", "OpenAI speech"}, {"EXA_API_KEY", "Exa"},
	{"PARALLEL_API_KEY", "Parallel"}, {"FIRECRAWL_API_KEY", "Firecrawl"}, {"TAVILY_API_KEY", "Tavily"}, {"PERPLEXITY_API_KEY", "Perplexity"},
	{"BROWSERBASE_API_KEY", "Browserbase"}, {"BROWSER_USE_API_KEY", "Browser Use"}, {"FAL_KEY", "FAL"}}

// Chat apps config show lists by the variable of each one's bot token
var shownPlatforms = [][2]string{{"TELEGRAM_BOT_TOKEN", "Telegram"}, {"DISCORD_BOT_TOKEN", "Discord"}}

func (s *service) Show(_ context.Context, request *configv1.ShowRequest) (*configv1.ShowResponse, error) {
	cfg, err := server.Profile(s.config, request.GetProfile())
	if err != nil {
		return nil, err
	}
	response := &configv1.ShowResponse{}
	if exe, err := os.Executable(); err == nil {
		if resolved, err := filepath.EvalSymlinks(exe); err == nil {
			exe = resolved
		}
		response.Install = filepath.Dir(exe)
	}
	key := func(label, value string) *configv1.Shown {
		return &configv1.Shown{Label: label, Value: mask(value), Set: value != ""}
	}
	for _, row := range shownKeys {
		response.Keys = append(response.Keys, key(row[1], cfg.Getenv(row[0])))
	}
	anthropic := ""
	if spec, ok := s.providers.Spec("anthropic"); ok {
		for _, name := range spec.KeyEnvNames() {
			if anthropic = cfg.Getenv(name); anthropic != "" {
				break
			}
		}
	}
	response.Keys = append(response.Keys, key("Anthropic", anthropic))
	for _, row := range shownPlatforms {
		response.Platforms = append(response.Platforms, &configv1.Shown{Label: row[1], Set: cfg.Getenv(row[0]) != ""})
	}
	if section, ok := cfg.Section(skillsconfig.Values); ok {
		for _, setting := range section.Settings {
			shown := &configv1.Shown{Label: setting.Title, Owner: setting.Skill}
			if value := cfg.Get(setting.Key); config.Truthy(value) {
				shown.Value, shown.Set = fmt.Sprint(value), true
			}
			response.Skills = append(response.Skills, shown)
		}
	}
	return response, nil
}

// What the saved settings leave inactive: keys of no setting, keys in the file of the other scope, values their setting refuses, and values
// an environment variable or the administrator decide instead
func (s *service) inactive(cfg *config.Config) []string {
	shared, own := cfg.Files()
	ownPlace, ownEdit := own, "icebath config edit"
	if ownPlace == "" {
		ownPlace, ownEdit = "The remote settings", "icebath config unset <key>"
	}
	var out []string
	for _, scope := range []struct {
		global bool
		place  string
		edit   string
	}{{true, shared, "icebath config edit --shared"}, {false, ownPlace, ownEdit}} {
		held := cfg.Held(scope.global)
		var walk func(m map[string]any, prefix string)
		walk = func(m map[string]any, prefix string) {
			for _, name := range sortedKeys(m) {
				key, value := prefix+name, m[name]
				_, setting, known := cfg.Setting(key)
				child, table := value.(map[string]any)
				switch {
				case known:
					out = append(out, s.misfit(cfg, scope.place, setting, value, scope.global)...)
				case table && s.registered(cfg, key):
					walk(child, key+".")
				default:
					out = append(out, stray(scope.place, scope.edit, leaves(key, value)))
				}
			}
		}
		walk(held, "")
	}
	return out
}

// Line naming the keys of a file that no setting declares, which nothing reads, and the command that opens the file
func stray(place, edit string, keys []string) string {
	if len(keys) == 1 {
		return fmt.Sprintf("%s holds %s, which is not a setting, so nothing reads it. Remove it with `%s`.", place, keys[0], edit)
	}
	return fmt.Sprintf("%s holds keys that are not settings, so nothing reads them: %s. Remove them with `%s`.", place, strings.Join(keys, ", "),
		edit)
}

// Full keys of the values a value of tables holds at its ends, the key itself for a value that is no table
func leaves(key string, value any) []string {
	table, ok := value.(map[string]any)
	if !ok || len(table) == 0 {
		return []string{key}
	}
	var keys []string
	for _, name := range sortedKeys(table) {
		keys = append(keys, leaves(key+"."+name, table[name])...)
	}
	return keys
}

// Whether a section is registered under the key at the top
func (s *service) registered(cfg *config.Config, key string) bool {
	_, ok := cfg.Section(key)
	return ok
}

// What leaves one stored value of a setting inactive
func (s *service) misfit(cfg *config.Config, place string, setting config.Setting, value any, global bool) []string {
	var out []string
	if shared := cfg.Global(setting.Key); shared != global {
		where := "the profile's own settings file"
		if shared {
			where = "the settings file every profile shares"
		}
		return []string{fmt.Sprintf("%s holds %s, which is only read from %s. Here it has no effect.", place, setting.Key, where)}
	}
	if _, err := setting.Check(value); err != nil {
		out = append(out, fmt.Sprintf("%s sets %s to %v, which can't be used. %s", place, setting.Key, shownValue(setting, value), err.Error()))
	} else if err := cfg.Refused(setting.Key, value); err != nil {
		out = append(out, fmt.Sprintf("%s sets %s to %v, which can't be used. %s", place, setting.Key, shownValue(setting, value), err.Error()))
	}
	switch source, variable := cfg.Source(setting); source {
	case config.FromManaged:
		out = append(out, fmt.Sprintf("%s sets %s, but your administrator sets it in %s. The value here has no effect.", place, setting.Key,
			filepath.Join(config.ManagedDir(), "config.yaml")))
	case config.FromEnvironment:
		out = append(out, fmt.Sprintf("%s sets %s, but the environment variable %s wins. The value here has no effect.", place, setting.Key, variable))
	}
	return out
}

func shownValue(setting config.Setting, value any) any {
	if text, ok := value.(string); ok && setting.Secret && text != "" {
		return mask(text)
	}
	return value
}

func sortedKeys(m map[string]any) []string {
	keys := make([]string, 0, len(m))
	for key := range m {
		keys = append(keys, key)
	}
	slices.Sort(keys)
	return keys
}
