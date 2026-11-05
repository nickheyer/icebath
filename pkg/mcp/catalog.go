package mcp

import (
	"bytes"
	"context"
	"embed"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"sync"
	"syscall"
	"unicode"
	"unicode/utf8"

	"gopkg.in/yaml.v3"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/tools"
)

//go:embed catalog
var embeddedCatalog embed.FS

const (
	manifestVersion = 1
	installDirVar   = "${INSTALL_DIR}"
	maxApplications = 16
)

const (
	ansiReset  = "\033[0m"
	ansiBold   = "\033[1m"
	ansiDim    = "\033[2m"
	ansiRed    = "\033[31m"
	ansiGreen  = "\033[32m"
	ansiYellow = "\033[33m"
	ansiCyan   = "\033[36m"
)

var (
	manifestOAuthKeys = []string{"client_id", "client_secret", "redirect_host", "redirect_port", "scope"}
	catalogEnvRefRe   = regexp.MustCompile(`\$\{([A-Za-z_][A-Za-z0-9_]*)\}`)
	catalogEnvNameRe  = regexp.MustCompile(`\A[A-Za-z_][A-Za-z0-9_]*\n?\z`)
	catalogNameRe     = regexp.MustCompile(`\A[A-Za-z0-9_-]+\n?\z`)
	appLabelRe        = regexp.MustCompile(`\A[A-Za-z0-9][A-Za-z0-9 ._+-]{0,79}\z`)
	connectorSlugRe   = regexp.MustCompile(`\A[a-z0-9][a-z0-9_-]*\z`)
	shaRefRe          = regexp.MustCompile(`\A[0-9a-f]{7,40}\z`)
)

// Terminal surface for catalog, picker and tools commands, implemented by the cli_output and curses_ui port
type TerminalUI interface {
	Out() io.Writer
	Color(text string, codes ...string) string
	Prompt(question, def string, password bool) string
	YesNo(question string, def bool) bool
	Checklist(title string, items []string, selected, cancelReturns map[int]bool) map[int]bool
	SingleSelect(title string, items []string) (int, bool)
	Interactive() bool
}

type EnvVarSpec struct {
	Name     string
	Prompt   string
	Required bool
	Secret   bool
	Default  string
}

type AuthSpec struct {
	Type     string
	Env      []EnvVarSpec
	Provider any
	Scopes   []any
	EnvVar   any
	OAuth    map[string]any
}

type TransportSpec struct {
	Type    string
	Command any
	Args    []string
	URL     any
	Version any
	Env     map[string]string
}

type InstallSpec struct {
	Type      string
	URL       any
	Ref       any
	Bootstrap []string
}

type ToolsSpec struct {
	DefaultEnabled  []string
	DefaultExcluded []string
}

type SuggestSpec struct {
	Keywords     []string
	Hosts        []string
	Applications []string
	Examples     []string
	RequiresApp  bool
}

type CatalogEntry struct {
	Name          string
	Description   string
	Source        string
	Transport     TransportSpec
	Auth          AuthSpec
	ConnectorSlug string
	Tools         ToolsSpec
	Install       *InstallSpec
	PostInstall   string
	Suggest       *SuggestSpec
	ManifestPath  string
}

type CatalogDiagnostic struct {
	Name    string
	Kind    string
	Message string
}

// Manifest or install failure naming the failure class the install metric records
type CatalogError struct {
	Msg   string
	Class string
}

func (e *CatalogError) Error() string { return e.Msg }

// Failure class of the raise site, config_invalid when the site named none
func (e *CatalogError) FailureClass() string {
	if e.Class == "" {
		return "config_invalid"
	}
	return e.Class
}

func catalogErr(format string, a ...any) error { return &CatalogError{Msg: fmt.Sprintf(format, a...)} }

func catalogErrClass(class, format string, a ...any) error {
	return &CatalogError{Msg: fmt.Sprintf(format, a...), Class: class}
}

var (
	catalogDiagMu       sync.Mutex
	catalogDiagnostics  []CatalogDiagnostic
	catalogRootOverride = "ICEBATH_OPTIONAL_MCPS"
)

// Seams over the cli config probe, the shared metrics recorder and the git runner
var (
	catalogProbeServer   = probeSingleServer
	catalogRecordInstall = recordExtensionInstall
	catalogRunGit        = runGitWithCredentialFallback
	catalogLookPath      = exec.LookPath
)

// Catalog tree and its display path, the env override directory or the embedded manifests
var catalogRoot = func() (fs.FS, string) {
	if o := pyStrip(os.Getenv(catalogRootOverride)); o != "" {
		return os.DirFS(o), o
	}
	sub, err := fs.Sub(embeddedCatalog, "catalog")
	if err != nil {
		panic(err)
	}
	return sub, "optional-mcps"
}

// Mapping view of a decoded YAML value, reporting keys that were not strings
func yDict(v any) (map[string]any, bool, bool) {
	switch m := v.(type) {
	case map[string]any:
		return m, false, true
	case map[any]any:
		out := make(map[string]any, len(m))
		nonStr := false
		for k, x := range m {
			if _, ok := k.(string); !ok {
				nonStr = true
			}
			out[pyStr(k)] = x
		}
		return out, nonStr, true
	}
	return nil, false, false
}

func yTruthy(v any) bool {
	if m, ok := v.(map[any]any); ok {
		return len(m) > 0
	}
	return pyTruthy(v)
}

func yOr(a, b any) any {
	if yTruthy(a) {
		return a
	}
	return b
}

func yTypeName(v any) string {
	if _, ok := v.(map[any]any); ok {
		return "dict"
	}
	return pyTypeName(v)
}

// Python list() of a value, characters of a string and keys of a mapping
func pyList(v any) ([]any, error) {
	switch x := v.(type) {
	case []any:
		return slices.Clone(x), nil
	case string:
		out := make([]any, 0, len(x))
		for _, r := range x {
			out = append(out, string(r))
		}
		return out, nil
	case map[string]any:
		out := make([]any, 0, len(x))
		for _, k := range orderedKeys(x) {
			out = append(out, k)
		}
		return out, nil
	case map[any]any:
		m, _, _ := yDict(x)
		return pyList(m)
	}
	return nil, fmt.Errorf("'%s' object is not iterable", yTypeName(v))
}

func pyStrListRepr(items []string) string {
	parts := make([]string, len(items))
	for i, s := range items {
		parts[i] = pyReprStr(s)
	}
	return "[" + strings.Join(parts, ", ") + "]"
}

// True for Python str.isprintable, letters marks numbers punctuation symbols and the plain space
func pyIsPrintable(s string) bool {
	for _, r := range s {
		if r != ' ' && !unicode.In(r, unicode.L, unicode.M, unicode.N, unicode.P, unicode.S) {
			return false
		}
	}
	return true
}

func notStringError(v any) error {
	return fmt.Errorf("expected string or bytes-like object, got '%s'", yTypeName(v))
}

func isPyInt(v any) bool {
	switch v.(type) {
	case int, int64, uint64:
		return true
	}
	return false
}

func parseEnvSpec(raw any) (EnvVarSpec, error) {
	m, _, ok := yDict(raw)
	if !ok {
		return EnvVarSpec{}, catalogErr("env entry must be a mapping, got %s", yTypeName(raw))
	}
	nameRaw := yOr(m["name"], "")
	if !yTruthy(nameRaw) {
		return EnvVarSpec{}, catalogErr("invalid env var name: %s", pyRepr(nameRaw))
	}
	name, isStr := nameRaw.(string)
	if !isStr {
		return EnvVarSpec{}, notStringError(nameRaw)
	}
	if !catalogEnvNameRe.MatchString(name) {
		return EnvVarSpec{}, catalogErr("invalid env var name: %s", pyReprStr(name))
	}
	required, secret := true, true
	if v, ok := m["required"]; ok {
		required = yTruthy(v)
	}
	if v, ok := m["secret"]; ok {
		secret = yTruthy(v)
	}
	return EnvVarSpec{Name: name, Prompt: pyStr(yOr(m["prompt"], name)), Required: required, Secret: secret,
		Default: pyStr(yOr(m["default"], ""))}, nil
}

func requireMapping(path, key string, raw any) (map[string]any, error) {
	m, _, ok := yDict(raw)
	if !ok {
		return nil, catalogErr("%s: '%s' must be a mapping", path, key)
	}
	return m, nil
}

func requireList(path, field string, raw any) ([]any, error) {
	l, ok := raw.([]any)
	if !ok {
		return nil, catalogErr("%s: %s must be a list", path, field)
	}
	return l, nil
}

func requireStrList(path, field string, raw any, nonEmpty bool) ([]string, error) {
	l, ok := raw.([]any)
	out := make([]string, 0, len(l))
	for _, t := range l {
		s, isStr := t.(string)
		if !isStr || (nonEmpty && pyStrip(s) == "") {
			ok = false
			break
		}
		out = append(out, s)
	}
	if !ok {
		kind := "strings"
		if nonEmpty {
			kind = "non-empty strings"
		}
		return nil, catalogErr("%s: %s must be a list of %s", path, field, kind)
	}
	return out, nil
}

func parseTransport(path string, raw any) (TransportSpec, error) {
	tr, err := requireMapping(path, "transport", yOr(raw, map[string]any{}))
	if err != nil {
		return TransportSpec{}, err
	}
	tType, _ := tr["type"].(string)
	if tType != "stdio" && tType != "http" {
		return TransportSpec{}, catalogErr("%s: transport.type must be 'stdio' or 'http'", path)
	}
	args, err := requireList(path, "transport.args", yOr(tr["args"], []any{}))
	if err != nil {
		return TransportSpec{}, err
	}
	envRaw := yOr(tr["env"], map[string]any{})
	envMap, nonStr, ok := yDict(envRaw)
	env := map[string]string{}
	for k, v := range envMap {
		s, isStr := v.(string)
		if !isStr {
			ok = false
			break
		}
		env[k] = s
	}
	if !ok || nonStr {
		return TransportSpec{}, catalogErr("%s: transport.env must be a mapping of string to string", path)
	}
	t := TransportSpec{Type: tType, Command: tr["command"], URL: tr["url"], Version: tr["version"], Env: env,
		Args: make([]string, len(args))}
	for i, a := range args {
		t.Args[i] = pyStr(a)
	}
	if tType == "stdio" && !yTruthy(t.Command) {
		return TransportSpec{}, catalogErr("%s: stdio transport requires 'command'", path)
	}
	if tType == "http" && !yTruthy(t.URL) {
		return TransportSpec{}, catalogErr("%s: http transport requires 'url'", path)
	}
	return t, nil
}

func parseAuth(path string, raw any, name string, http bool) (AuthSpec, error) {
	a, err := requireMapping(path, "auth", yOr(raw, map[string]any{"type": "none"}))
	if err != nil {
		return AuthSpec{}, err
	}
	aType, _ := yOr(a["type"], "none").(string)
	if aType != "api_key" && aType != "oauth" && aType != "none" {
		return AuthSpec{}, catalogErr("%s: auth.type must be 'api_key'|'oauth'|'none'", path)
	}
	rawEnv, err := requireList(path, "auth.env", yOr(a["env"], []any{}))
	if err != nil {
		return AuthSpec{}, err
	}
	specs := make([]EnvVarSpec, 0, len(rawEnv))
	declared := map[string]bool{}
	for _, e := range rawEnv {
		spec, err := parseEnvSpec(e)
		if err != nil {
			return AuthSpec{}, err
		}
		specs = append(specs, spec)
		declared[spec.Name] = true
	}
	if http && aType == "api_key" {
		required := envKeyForServer(name)
		if !declared[required] {
			return AuthSpec{}, catalogErr("%s: http + api_key auth requires auth.env to declare '%s' (the key the Authorization header references)", path, required)
		}
	}
	oauthRaw := yOr(a["oauth"], map[string]any{})
	if yTruthy(oauthRaw) && aType != "oauth" {
		return AuthSpec{}, catalogErr("%s: auth.oauth is only valid with auth.type 'oauth'", path)
	}
	oauth, err := requireMapping(path, "auth.oauth", oauthRaw)
	if err != nil {
		return AuthSpec{}, err
	}
	_, nonStr, _ := yDict(oauthRaw)
	var unknown []string
	valuesOK := !nonStr
	for k, v := range oauth {
		if !slices.Contains(manifestOAuthKeys, k) {
			unknown = append(unknown, k)
		}
		if _, isStr := v.(string); !isStr && !isPyInt(v) {
			valuesOK = false
		}
	}
	slices.Sort(unknown)
	if len(unknown) > 0 || !valuesOK {
		msg := fmt.Sprintf("%s: auth.oauth allows string/int values for %s only", path, pyStrListRepr(manifestOAuthKeys))
		if len(unknown) > 0 {
			msg += fmt.Sprintf(" (unknown: %s)", pyStrListRepr(unknown))
		}
		return AuthSpec{}, &CatalogError{Msg: msg}
	}
	undeclaredSet := map[string]bool{}
	for _, v := range oauth {
		if s, ok := v.(string); ok {
			for _, m := range catalogEnvRefRe.FindAllStringSubmatch(s, -1) {
				if !declared[m[1]] {
					undeclaredSet[m[1]] = true
				}
			}
		}
	}
	if len(undeclaredSet) > 0 {
		undeclared := make([]string, 0, len(undeclaredSet))
		for k := range undeclaredSet {
			undeclared = append(undeclared, k)
		}
		slices.Sort(undeclared)
		return AuthSpec{}, catalogErr("%s: auth.oauth references env vars not declared in auth.env: %s", path, pyStrListRepr(undeclared))
	}
	scopes, err := pyList(yOr(a["scopes"], []any{}))
	if err != nil {
		return AuthSpec{}, err
	}
	copied := make(map[string]any, len(oauth))
	for k, v := range oauth {
		copied[k] = v
	}
	return AuthSpec{Type: aType, Env: specs, Provider: a["provider"], Scopes: scopes, EnvVar: a["env_var"], OAuth: copied}, nil
}

func parseTools(path string, raw any) (ToolsSpec, error) {
	t, err := requireMapping(path, "tools", yOr(raw, map[string]any{}))
	if err != nil {
		return ToolsSpec{}, err
	}
	var spec ToolsSpec
	for _, key := range []string{"default_enabled", "default_excluded"} {
		val := t[key]
		if val == nil {
			continue
		}
		list, err := requireStrList(path, "tools."+key, val, false)
		if err != nil {
			return ToolsSpec{}, err
		}
		if key == "default_enabled" {
			spec.DefaultEnabled = list
		} else {
			spec.DefaultExcluded = list
		}
	}
	if spec.DefaultEnabled != nil && spec.DefaultExcluded != nil {
		return ToolsSpec{}, catalogErr("%s: tools.default_enabled and tools.default_excluded are mutually exclusive", path)
	}
	return spec, nil
}

// Bounded display labels and aliases, never paths, commands or regexes
func validateApplications(labels any) ([]string, error) {
	l, ok := labels.([]any)
	if !ok || len(l) > maxApplications {
		return nil, errors.New("suggest.applications must be a list of at most 16 app labels")
	}
	out := make([]string, 0, len(l))
	for _, label := range l {
		s, isStr := label.(string)
		if !isStr || !appLabelRe.MatchString(s) || s != pyStrip(s) || strings.Contains(s, "..") || strings.Contains(s, " --") {
			return nil, errors.New("suggest.applications must contain safe app labels (1–80 characters)")
		}
		out = append(out, s)
	}
	return out, nil
}

func parseSuggest(path string, raw any) (*SuggestSpec, error) {
	if raw == nil {
		return nil, nil
	}
	m, err := requireMapping(path, "suggest", raw)
	if err != nil {
		return nil, err
	}
	kwRaw, hostsRaw := yOr(m["keywords"], []any{}), yOr(m["hosts"], []any{})
	keywords, err := requireStrList(path, "suggest.keywords", kwRaw, true)
	if err != nil {
		return nil, err
	}
	hosts, err := requireStrList(path, "suggest.hosts", hostsRaw, true)
	if err != nil {
		return nil, err
	}
	appsRaw, ok := m["applications"]
	if !ok {
		appsRaw = []any{}
	}
	applications, err := validateApplications(appsRaw)
	if err != nil {
		return nil, catalogErr("%s: %s", path, err)
	}
	examplesRaw, ok := m["examples"]
	if !ok {
		examplesRaw = []any{}
	}
	examples, err := requireStrList(path, "suggest.examples", examplesRaw, true)
	if err != nil {
		return nil, err
	}
	tooLong := len(examples) > 6
	for _, e := range examples {
		if utf8.RuneCountInString(e) > 240 || !pyIsPrintable(e) {
			tooLong = true
		}
	}
	if tooLong {
		return nil, catalogErr("%s: suggest.examples allows at most 6 single-line examples of 240 characters", path)
	}
	reqRaw, ok := m["requires_app"]
	if !ok {
		reqRaw = false
	}
	requiresApp, isBool := reqRaw.(bool)
	if !isBool || (requiresApp && len(applications) == 0) {
		return nil, catalogErr("%s: suggest.requires_app must be a boolean, with applications when true", path)
	}
	if len(keywords) == 0 && len(hosts) == 0 && len(applications) == 0 {
		return nil, catalogErr("%s: 'suggest' requires at least one keyword, host or application", path)
	}
	spec := &SuggestSpec{Applications: applications, Examples: examples, RequiresApp: requiresApp,
		Keywords: make([]string, len(keywords)), Hosts: make([]string, len(hosts))}
	for i, k := range keywords {
		spec.Keywords[i] = strings.ToLower(pyStrip(k))
	}
	for i, h := range hosts {
		spec.Hosts[i] = strings.TrimLeft(strings.ToLower(pyStrip(h)), ".")
	}
	return spec, nil
}

func parseConnectorSlug(path string, value any) (string, error) {
	if value == nil {
		return "", nil
	}
	s, ok := value.(string)
	if !ok || !connectorSlugRe.MatchString(s) {
		return "", catalogErr("%s: connector_slug must be a hosted connector slug", path)
	}
	return s, nil
}

func parseInstall(path string, raw any) (*InstallSpec, error) {
	if raw == nil {
		return nil, nil
	}
	m, err := requireMapping(path, "install", raw)
	if err != nil {
		return nil, err
	}
	iType, isStr := m["type"].(string)
	if !isStr || iType != "git" {
		return nil, catalogErr("%s: install.type must be 'git' (got %s)", path, pyRepr(m["type"]))
	}
	url, ref := yOr(m["url"], ""), yOr(m["ref"], "")
	if !yTruthy(url) || !yTruthy(ref) {
		return nil, catalogErr("%s: install.url and install.ref are required", path)
	}
	bootstrap, err := requireList(path, "install.bootstrap", yOr(m["bootstrap"], []any{}))
	if err != nil {
		return nil, err
	}
	spec := &InstallSpec{Type: iType, URL: url, Ref: ref, Bootstrap: make([]string, len(bootstrap))}
	for i, c := range bootstrap {
		spec.Bootstrap[i] = pyStr(c)
	}
	return spec, nil
}

// Python codec message for the first byte that is not valid UTF-8
func utf8DecodeError(data []byte) error {
	for i := 0; i < len(data); {
		r, size := utf8.DecodeRune(data[i:])
		if r != utf8.RuneError || size > 1 {
			i += size
			continue
		}
		reason := "invalid start byte"
		if b := data[i]; b >= 0xc2 && b <= 0xf4 {
			reason = "invalid continuation byte"
			if !utf8.FullRune(data[i:]) {
				reason = "unexpected end of data"
			}
		}
		return fmt.Errorf("'utf-8' codec can't decode byte 0x%02x in position %d: %s", data[i], i, reason)
	}
	return nil
}

// Single YAML document of a manifest decoded like yaml.safe_load
func loadManifestYAML(data []byte) (any, error) {
	data = bytes.TrimPrefix(data, []byte("\xef\xbb\xbf"))
	if err := utf8DecodeError(data); err != nil {
		return nil, err
	}
	dec := yaml.NewDecoder(bytes.NewReader(data))
	var doc any
	if err := dec.Decode(&doc); err != nil {
		if errors.Is(err, io.EOF) {
			return nil, nil
		}
		return nil, err
	}
	var extra any
	if err := dec.Decode(&extra); !errors.Is(err, io.EOF) {
		if err != nil {
			return nil, err
		}
		return nil, errors.New("expected a single document in the stream")
	}
	return doc, nil
}

func manifestVersionSupported(mv any) bool {
	switch v := mv.(type) {
	case bool:
		return v && manifestVersion == 1
	case int:
		return v == manifestVersion
	case int64:
		return v == manifestVersion
	case uint64:
		return v == manifestVersion
	case float64:
		return v == manifestVersion
	}
	return false
}

// Reads and validates one manifest, a CatalogError on any manifest problem
func parseManifest(fsys fs.FS, rel, path string) (*CatalogEntry, error) {
	raw, err := fs.ReadFile(fsys, rel)
	var doc any
	if err == nil {
		doc, err = loadManifestYAML(raw)
	}
	if err != nil {
		return nil, catalogErr("failed to read %s: %s", path, err)
	}
	data, _, ok := yDict(yOr(doc, map[string]any{}))
	if !ok {
		return nil, catalogErr("%s: manifest must be a mapping", path)
	}
	mv := data["manifest_version"]
	if !manifestVersionSupported(mv) {
		return nil, catalogErr("%s: manifest_version %s unsupported (this icebath understands version %d)", path, pyRepr(mv), manifestVersion)
	}
	nameRaw := yOr(data["name"], "")
	if !yTruthy(nameRaw) {
		return nil, catalogErr("%s: invalid or missing 'name'", path)
	}
	name, isStr := nameRaw.(string)
	if !isStr {
		return nil, notStringError(nameRaw)
	}
	if !catalogNameRe.MatchString(name) {
		return nil, catalogErr("%s: invalid or missing 'name'", path)
	}
	description := pyStrip(pyStr(yOr(data["description"], "")))
	if description == "" {
		return nil, catalogErr("%s: 'description' required", path)
	}
	transport, err := parseTransport(path, data["transport"])
	if err != nil {
		return nil, err
	}
	auth, err := parseAuth(path, data["auth"], name, transport.Type == "http")
	if err != nil {
		return nil, err
	}
	toolsSpec, err := parseTools(path, data["tools"])
	if err != nil {
		return nil, err
	}
	suggest, err := parseSuggest(path, data["suggest"])
	if err != nil {
		return nil, err
	}
	slug, err := parseConnectorSlug(path, data["connector_slug"])
	if err != nil {
		return nil, err
	}
	install, err := parseInstall(path, data["install"])
	if err != nil {
		return nil, err
	}
	return &CatalogEntry{Name: name, Description: description, Source: pyStrip(pyStr(yOr(data["source"], ""))),
		Transport: transport, Auth: auth, ConnectorSlug: slug, Tools: toolsSpec, Install: install,
		PostInstall: pyStr(yOr(data["post_install"], "")), Suggest: suggest, ManifestPath: path}, nil
}

// Every valid catalog entry sorted by name, recording skipped manifests for CatalogDiagnostics
func ListCatalog() ([]*CatalogEntry, error) {
	fsys, label := catalogRoot()
	if _, err := fs.Stat(fsys, "."); err != nil {
		return nil, nil
	}
	children, err := fs.ReadDir(fsys, ".")
	if err != nil {
		return nil, err
	}
	catalogDiagMu.Lock()
	defer catalogDiagMu.Unlock()
	catalogDiagnostics = nil
	var entries []*CatalogEntry
	for _, child := range children {
		rel := child.Name() + "/manifest.yaml"
		if info, err := fs.Stat(fsys, rel); err != nil || !info.Mode().IsRegular() {
			continue
		}
		entry, err := parseManifest(fsys, rel, filepath.Join(label, child.Name(), "manifest.yaml"))
		var ce *CatalogError
		switch {
		case err == nil:
			entries = append(entries, entry)
		case errors.As(err, &ce):
			kind := "invalid"
			if strings.Contains(ce.Msg, "manifest_version") && strings.Contains(ce.Msg, "unsupported") {
				kind = "future_manifest"
			}
			catalogDiagnostics = append(catalogDiagnostics, CatalogDiagnostic{Name: child.Name(), Kind: kind, Message: ce.Msg})
		default:
			return nil, err
		}
	}
	return entries, nil
}

// Skipped manifests of the latest listing, future_manifest for newer versions and invalid otherwise
func CatalogDiagnostics() []CatalogDiagnostic {
	catalogDiagMu.Lock()
	defer catalogDiagMu.Unlock()
	return slices.Clone(catalogDiagnostics)
}

// Catalog entry by name with an optional official/ prefix, nil when absent
func GetEntry(name string) (*CatalogEntry, error) {
	name = strings.TrimPrefix(name, "official/")
	entries, err := ListCatalog()
	if err != nil {
		return nil, err
	}
	for _, e := range entries {
		if e.Name == name {
			return e, nil
		}
	}
	return nil, nil
}

// True when mcp_servers holds the name
func IsInstalled(ctx context.Context, name string) (bool, error) {
	servers, err := getMCPServers(ctx, nil)
	if err != nil {
		return false, err
	}
	_, ok := servers[name]
	return ok, nil
}

// Enabled reader over a server block of any shape, a non mapping block fails like a missing get
func serverBlockEnabled(cfg any) (bool, error) {
	m, _, ok := yDict(cfg)
	if !ok {
		return false, fmt.Errorf("'%s' object has no attribute 'get'", yTypeName(cfg))
	}
	return ServerEnabled(m), nil
}

// True when the server block exists, is non empty and its enabled flag reads on
func IsEnabled(ctx context.Context, name string) (bool, error) {
	servers, err := getMCPServers(ctx, nil)
	if err != nil {
		return false, err
	}
	cfg := servers[name]
	if !yTruthy(cfg) {
		return false, nil
	}
	return serverBlockEnabled(cfg)
}

func say(ui TerminalUI, msg string, codes ...string) {
	if len(codes) == 0 {
		codes = []string{ansiGreen}
	}
	fmt.Fprintln(ui.Out(), ui.Color(msg, codes...))
}

func printInfo(ui TerminalUI, text string) { fmt.Fprintln(ui.Out(), ui.Color("  "+text, ansiDim)) }
func printSuccess(ui TerminalUI, text string) {
	fmt.Fprintln(ui.Out(), ui.Color("✓ "+text, ansiGreen))
}
func printWarning(ui TerminalUI, text string) {
	fmt.Fprintln(ui.Out(), ui.Color("⚠ "+text, ansiYellow))
}
func printError(ui TerminalUI, text string) { fmt.Fprintln(ui.Out(), ui.Color("✗ "+text, ansiRed)) }

// Per profile directory git bootstrapped servers are cloned into, created on demand
func catalogInstallRoot(ctx context.Context) (string, error) {
	root := filepath.Join(home.Dir(ctx), "mcp-installs")
	return root, os.MkdirAll(root, 0o777)
}

// Exit status like subprocess returncode, a negative signal number for a signalled child
func shellReturnCode(err error) (int, error) {
	if err == nil {
		return 0, nil
	}
	var ee *exec.ExitError
	if !errors.As(err, &ee) {
		return 0, err
	}
	if ws, ok := ee.Sys().(syscall.WaitStatus); ok && ws.Signaled() {
		return -int(ws.Signal()), nil
	}
	return ee.ExitCode(), nil
}

// Runs each bootstrap command through the shell in cwd, stopping at the first failure
func runBootstrap(ctx context.Context, ui TerminalUI, cwd string, commands []string) error {
	for _, c := range commands {
		say(ui, "  $ "+c, ansiDim)
		cmd := bootstrapShell(ctx, c)
		cmd.Dir, cmd.Stdin, cmd.Stdout, cmd.Stderr = cwd, os.Stdin, ui.Out(), os.Stderr
		rc, err := shellReturnCode(cmd.Run())
		if err != nil {
			return err
		}
		if rc != 0 {
			return catalogErrClass("bootstrap_failed", "bootstrap step failed (exit %d): %s", rc, c)
		}
	}
	return nil
}

// Clones the pinned ref into the install root and runs bootstrap, returning the clone directory
func doGitInstall(ctx context.Context, ui TerminalUI, entry *CatalogEntry) (string, error) {
	install := entry.Install
	root, err := catalogInstallRoot(ctx)
	if err != nil {
		return "", err
	}
	dest := filepath.Join(root, entry.Name)
	git, lookErr := catalogLookPath("git")
	if lookErr != nil {
		return "", catalogErrClass("git_missing", "git is required to install this MCP but was not found on PATH")
	}
	if _, err := os.Stat(dest); err == nil {
		say(ui, "  Removing existing install at "+dest, ansiDim)
		if err := tools.RmtreeReadonly(dest, false); err != nil {
			return "", err
		}
	}
	say(ui, fmt.Sprintf("  Cloning %s (%s) → %s", pyStr(install.URL), pyStr(install.Ref), dest), ansiCyan)
	ref, isStr := install.Ref.(string)
	if !isStr {
		return "", notStringError(install.Ref)
	}
	url, isStr := install.URL.(string)
	if !isStr {
		return "", fmt.Errorf("expected str, bytes or os.PathLike object, not %s", yTypeName(install.URL))
	}
	isSHA := shaRefRe.MatchString(ref)
	gitEnv := tools.NoninteractiveGitEnv(nil)
	runGit := func(args ...string) (int, error) {
		code, stderr, err := catalogRunGit(ctx, append([]string{git}, args...), url, gitEnv)
		if err != nil {
			return 0, err
		}
		if code != 0 && pyStrip(stderr) != "" {
			say(ui, pyStrip(stderr), ansiDim)
		}
		return code, nil
	}
	if !isSHA {
		code, err := runGit("clone", "--depth", "1", "--branch", ref, url, dest)
		if err != nil {
			return "", err
		}
		if code != 0 {
			if _, err := os.Stat(dest); err == nil {
				if err := tools.RmtreeReadonly(dest, false); err != nil {
					return "", err
				}
			}
			isSHA = true
		}
	}
	if isSHA {
		code, err := runGit("clone", url, dest)
		if err != nil {
			return "", err
		}
		if code != 0 {
			return "", catalogErrClass("clone_failed", "git clone failed for %s", url)
		}
		if code, err = runGit("-C", dest, "checkout", ref); err != nil {
			return "", err
		}
		if code != 0 {
			return "", catalogErrClass("clone_failed", "git checkout %s failed", ref)
		}
	}
	if len(install.Bootstrap) > 0 {
		if err := runBootstrap(ctx, ui, dest, install.Bootstrap); err != nil {
			return "", err
		}
	}
	return dest, nil
}

// Substitutes the install directory, failing when the manifest has no install block
func expandInstallDir(value any, installDir string) (string, error) {
	s, ok := value.(string)
	if !ok {
		return "", fmt.Errorf("argument of type '%s' is not iterable", yTypeName(value))
	}
	if !strings.Contains(s, installDirVar) {
		return s, nil
	}
	if installDir == "" {
		return "", catalogErr("manifest references %s but no install block exists", installDirVar)
	}
	return strings.ReplaceAll(s, installDirVar, installDir), nil
}

// Profile env value through the secret scope, then the profile .env file
func catalogEnvValue(ctx context.Context, env *tools.Env, name string) (string, error) {
	v, ok, err := getSecret(env, name)
	if err != nil {
		return "", err
	}
	if ok {
		return v, nil
	}
	return config.LoadEnvFile(home.EnvPath(home.Dir(ctx)))[name], nil
}

// Collects each env value, saving secrets to the profile .env and returning every value gathered
func promptEnvVars(ctx context.Context, env *tools.Env, ui TerminalUI, specs []EnvVarSpec, preloaded map[string]string) (map[string]string, error) {
	collected := map[string]string{}
	for _, spec := range specs {
		if pre := preloaded[spec.Name]; pre != "" {
			collected[spec.Name] = pre
			continue
		}
		if spec.Secret {
			existing, err := catalogEnvValue(ctx, env, spec.Name)
			if err != nil {
				return nil, err
			}
			if existing != "" {
				say(ui, fmt.Sprintf("  ✓ %s already set in .env", spec.Name))
				collected[spec.Name] = existing
				continue
			}
		}
		value := ui.Prompt(spec.Prompt, spec.Default, spec.Secret)
		switch {
		case value != "":
			if spec.Secret {
				if err := config.SaveEnvValue(home.Dir(ctx), spec.Name, value); err != nil {
					return nil, err
				}
			}
			collected[spec.Name] = value
		case spec.Required:
			return nil, catalogErrClass("missing_credentials", "%s is required but no value was provided", spec.Name)
		}
	}
	return collected, nil
}

// Replaces literal references to one non secret variable throughout a config value
func inlineNonSecretValue(obj any, name, value string) any {
	switch x := obj.(type) {
	case string:
		return strings.ReplaceAll(x, "${"+name+"}", value)
	case map[string]any:
		out := make(map[string]any, len(x))
		for k, v := range x {
			out[k] = inlineNonSecretValue(v, name, value)
		}
		return out
	case []any:
		out := make([]any, len(x))
		for i, v := range x {
			out[i] = inlineNonSecretValue(v, name, value)
		}
		return out
	}
	return obj
}

// The mcp_servers block a manifest translates to, before credentials and tool filters
func buildServerConfig(entry *CatalogEntry, installDir string) (map[string]any, error) {
	cfg := map[string]any{}
	t := entry.Transport
	switch t.Type {
	case "stdio":
		command, err := expandInstallDir(yOr(t.Command, ""), installDir)
		if err != nil {
			return nil, err
		}
		cfg["command"] = command
		if len(t.Args) > 0 {
			args := make([]any, len(t.Args))
			for i, a := range t.Args {
				if args[i], err = expandInstallDir(a, installDir); err != nil {
					return nil, err
				}
			}
			cfg["args"] = args
		}
		if len(t.Env) > 0 {
			env := make(map[string]any, len(t.Env))
			for k, v := range t.Env {
				env[k] = v
			}
			cfg["env"] = env
		}
	case "http":
		cfg["url"] = t.URL
		switch entry.Auth.Type {
		case "oauth":
			cfg["auth"] = "oauth"
			if len(entry.Auth.OAuth) > 0 {
				oauth := make(map[string]any, len(entry.Auth.OAuth))
				for k, v := range entry.Auth.OAuth {
					oauth[k] = v
				}
				cfg["oauth"] = oauth
			}
		case "api_key":
			cfg["headers"] = bearerAuthHeaders(entry.Name)
		}
	}
	return cfg, nil
}

// Prior well formed tools include or exclude list of a server, nil when absent or malformed
func readPriorToolList(ctx context.Context, name, key string) ([]string, error) {
	servers, err := getMCPServers(ctx, nil)
	if err != nil {
		return nil, err
	}
	cfg := yOr(servers[name], map[string]any{})
	m, _, ok := yDict(cfg)
	if !ok {
		return nil, fmt.Errorf("'%s' object has no attribute 'get'", yTypeName(cfg))
	}
	toolsCfg, _, ok := yDict(yOr(m["tools"], map[string]any{}))
	if !ok {
		return nil, nil
	}
	list, ok := toolsCfg[key].([]any)
	if !ok {
		return nil, nil
	}
	out := make([]string, 0, len(list))
	for _, t := range list {
		s, isStr := t.(string)
		if !isStr {
			return nil, nil
		}
		out = append(out, s)
	}
	return out, nil
}

// Tools of a freshly configured server, ok false when the server is missing or the probe failed
func probeCatalogTools(ctx context.Context, env *tools.Env, ui TerminalUI, name string) ([]ToolSummary, bool, error) {
	servers, err := getMCPServers(ctx, nil)
	if err != nil {
		return nil, false, err
	}
	cfg := servers[name]
	if !yTruthy(cfg) {
		return nil, false, nil
	}
	m, _, isMap := yDict(cfg)
	if !isMap {
		say(ui, fmt.Sprintf("  Probe failed: '%s' object has no attribute 'get'", yTypeName(cfg)), ansiYellow)
		return nil, false, nil
	}
	found, err := catalogProbeServer(ctx, env, name, m)
	if err != nil {
		say(ui, "  Probe failed: "+excStr(err), ansiYellow)
		return nil, false, nil
	}
	if found == nil {
		found = []ToolSummary{}
	}
	return found, true, nil
}

// Persists tools include or exclude for a server and clears the other mode, nil values drop the block
func writeToolsFilter(ctx context.Context, name, mode string, values []string) error {
	h := home.Dir(ctx)
	cfg, err := config.ReadUser(h)
	if err != nil {
		return err
	}
	serversRaw, present := cfg["mcp_servers"]
	if !present {
		serversRaw = map[string]any{}
	}
	servers, _, ok := yDict(serversRaw)
	if !ok {
		return fmt.Errorf("'%s' object has no attribute 'get'", yTypeName(serversRaw))
	}
	entryRaw := yOr(servers[name], map[string]any{})
	entry, _, ok := yDict(entryRaw)
	if !ok {
		return fmt.Errorf("'%s' object has no attribute 'pop'", yTypeName(entryRaw))
	}
	if values == nil {
		delete(entry, "tools")
	} else {
		block, _, ok := yDict(yOr(entry["tools"], map[string]any{}))
		if !ok {
			block = map[string]any{}
		}
		list := make([]any, len(values))
		for i, v := range values {
			list[i] = v
		}
		block[mode] = list
		if mode == "include" {
			delete(block, "exclude")
		} else {
			delete(block, "include")
		}
		entry["tools"] = block
	}
	servers[name] = entry
	cfg["mcp_servers"] = servers
	return config.SaveUser(h, cfg)
}

func indexSet(n int, keep func(int) bool) map[int]bool {
	out := map[int]bool{}
	for i := 0; i < n; i++ {
		if keep(i) {
			out[i] = true
		}
	}
	return out
}

// Probes the server and writes the tool filter the user picks, falling back to prior or manifest filters
func applyToolSelection(ctx context.Context, env *tools.Env, ui TerminalUI, entry *CatalogEntry, priorSelection, priorExclude []string) error {
	fmt.Fprintln(ui.Out())
	name := entry.Name
	configureHint := fmt.Sprintf("`icebath mcp configure %s`", name)
	if len(entry.Tools.DefaultExcluded) > 0 && priorSelection == nil {
		editHint := fmt.Sprintf("Edit mcp_servers.%s.tools.exclude in config.yaml or run %s to change.", name, configureHint)
		if priorExclude != nil {
			if err := writeToolsFilter(ctx, name, "exclude", priorExclude); err != nil {
				return err
			}
			say(ui, fmt.Sprintf("  Kept your existing exclude list (%d entries). %s", len(priorExclude), editHint))
			return nil
		}
		if err := writeToolsFilter(ctx, name, "exclude", entry.Tools.DefaultExcluded); err != nil {
			return err
		}
		say(ui, fmt.Sprintf("  Applied manifest exclude list (%d entries); everything else stays enabled. %s", len(entry.Tools.DefaultExcluded), editHint))
		return nil
	}
	say(ui, fmt.Sprintf("  Probing '%s' for available tools...", name), ansiCyan)
	probed, ok, err := probeCatalogTools(ctx, env, ui, name)
	if err != nil {
		return err
	}
	if !ok {
		manifestDefault := entry.Tools.DefaultEnabled
		refineHint := fmt.Sprintf("Run %s after the server is reachable to refine.", configureHint)
		var mode, msg string
		var values []string
		switch {
		case priorSelection != nil:
			mode, values = "include", priorSelection
			msg = fmt.Sprintf("Kept your previous tool selection (%d tools). %s", len(priorSelection), refineHint)
		case priorExclude != nil:
			mode, values = "exclude", priorExclude
			msg = fmt.Sprintf("Kept your existing exclude list (%d entries).", len(priorExclude))
		case len(manifestDefault) > 0:
			mode, values = "include", manifestDefault
			msg = fmt.Sprintf("Applied manifest default (%d tools). %s", len(manifestDefault), refineHint)
		default:
			mode = "include"
			msg = fmt.Sprintf("installed with no tool filter (all tools enabled when reachable). Run %s after first connect to prune.", configureHint)
		}
		if err := writeToolsFilter(ctx, name, mode, values); err != nil {
			return err
		}
		sep := "."
		if values == nil {
			sep = ";"
		}
		say(ui, fmt.Sprintf("  Couldn't probe server%s %s", sep, msg), ansiYellow)
		return nil
	}
	if len(probed) == 0 {
		if err := writeToolsFilter(ctx, name, "include", priorSelection); err != nil {
			return err
		}
		say(ui, "  Server reported no tools.", ansiYellow)
		return nil
	}
	toolNames := make([]string, len(probed))
	for i, t := range probed {
		toolNames[i] = t.Name
	}
	if !ui.Interactive() {
		preferred := priorSelection
		if preferred == nil && len(entry.Tools.DefaultEnabled) > 0 {
			preferred = entry.Tools.DefaultEnabled
		}
		var values []string
		if preferred != nil {
			values = []string{}
			for _, n := range preferred {
				if slices.Contains(toolNames, n) {
					values = append(values, n)
				}
			}
		}
		return writeToolsFilter(ctx, name, "include", values)
	}
	preferred := priorSelection
	if preferred == nil {
		preferred = entry.Tools.DefaultEnabled
		if len(preferred) == 0 {
			preferred = toolNames
		}
	}
	pre := indexSet(len(toolNames), func(i int) bool { return slices.Contains(preferred, toolNames[i]) })
	say(ui, fmt.Sprintf("  Found %d tool(s). Pre-checked: %d.", len(probed), len(pre)))
	labels := make([]string, len(probed))
	for i, t := range probed {
		d := t.Description
		if utf8.RuneCountInString(d) > 60 {
			d = pyHead(d, 60) + "..."
		}
		labels[i] = fmt.Sprintf("%s  —  %s", t.Name, d)
	}
	chosen := ui.Checklist(fmt.Sprintf("Select tools for '%s' (SPACE toggle, ENTER confirm)", name), labels, pre, nil)
	if len(chosen) == 0 {
		if err := writeToolsFilter(ctx, name, "include", []string{}); err != nil {
			return err
		}
		say(ui, fmt.Sprintf("  No tools selected. Run %s to change.", configureHint), ansiYellow)
		return nil
	}
	if len(chosen) == len(probed) {
		if err := writeToolsFilter(ctx, name, "include", nil); err != nil {
			return err
		}
		say(ui, fmt.Sprintf("  ✓ All %d tools enabled (no filter — new tools the server adds later will be auto-enabled).", len(probed)))
		return nil
	}
	idx := make([]int, 0, len(chosen))
	for i := range chosen {
		idx = append(idx, i)
	}
	slices.Sort(idx)
	chosenNames := make([]string, len(idx))
	for i, j := range idx {
		chosenNames[i] = toolNames[j]
	}
	if err := writeToolsFilter(ctx, name, "include", chosenNames); err != nil {
		return err
	}
	say(ui, fmt.Sprintf("  ✓ %d/%d tools enabled.", len(chosenNames), len(probed)))
	return nil
}

// Server block a connection card installs, built in memory with the prior or manifest tool filter
func CardInstallConfig(ctx context.Context, ui TerminalUI, entry *CatalogEntry) (map[string]any, error) {
	installDir := ""
	if entry.Install != nil {
		dir, err := doGitInstall(ctx, ui, entry)
		if err != nil {
			return nil, err
		}
		installDir = dir
	}
	cfg, err := buildServerConfig(entry, installDir)
	if err != nil {
		return nil, err
	}
	cfg["enabled"] = true
	priorInclude, err := readPriorToolList(ctx, entry.Name, "include")
	if err != nil {
		return nil, err
	}
	priorExclude, err := readPriorToolList(ctx, entry.Name, "exclude")
	if err != nil {
		return nil, err
	}
	switch {
	case priorInclude != nil:
		cfg["tools"] = map[string]any{"include": stringsToAny(priorInclude)}
	case priorExclude != nil:
		cfg["tools"] = map[string]any{"exclude": stringsToAny(priorExclude)}
	case len(entry.Tools.DefaultExcluded) > 0:
		cfg["tools"] = map[string]any{"exclude": stringsToAny(entry.Tools.DefaultExcluded)}
	case len(entry.Tools.DefaultEnabled) > 0:
		cfg["tools"] = map[string]any{"include": stringsToAny(entry.Tools.DefaultEnabled)}
	}
	return cfg, nil
}

// One shared metrics extension install for an MCP server, name empty for a custom server
func RecordMCPInstall(ctx context.Context, source, name, outcome, failureClass string, err error) {
	catalogRecordInstall(ctx, "mcp_server", source, name, outcome, failureClass, err)
}

// Runs body and records a first install of the entry once, failed when body errors and success otherwise
func recordedCatalogInstall(ctx context.Context, name string, body func() error) error {
	installed, err := IsInstalled(ctx, name)
	if err != nil {
		return err
	}
	fresh := !installed
	if err := body(); err != nil {
		if fresh {
			RecordMCPInstall(ctx, "catalog", name, "failed", "", err)
		}
		return err
	}
	if fresh {
		RecordMCPInstall(ctx, "catalog", name, "success", "", nil)
	}
	return nil
}

// Installs a catalog entry end to end and records the first install metric
func InstallEntry(ctx context.Context, env *tools.Env, ui TerminalUI, entry *CatalogEntry, enable bool, preloadedEnv map[string]string) error {
	return recordedCatalogInstall(ctx, entry.Name, func() error {
		return installEntry(ctx, env, ui, entry, enable, preloadedEnv)
	})
}

// Clone and bootstrap, credential prompts, server block write, tool selection and post install notes
func installEntry(ctx context.Context, env *tools.Env, ui TerminalUI, entry *CatalogEntry, enable bool, preloadedEnv map[string]string) error {
	out := ui.Out()
	fmt.Fprintln(out)
	say(ui, fmt.Sprintf("  Installing MCP '%s'", entry.Name), ansiCyan+ansiBold)
	if entry.Description != "" {
		say(ui, "  "+entry.Description, ansiDim)
	}
	if entry.Source != "" {
		say(ui, "  Source: "+entry.Source, ansiDim)
	}
	fmt.Fprintln(out)
	installDir := ""
	if entry.Install != nil {
		dir, err := doGitInstall(ctx, ui, entry)
		if err != nil {
			return err
		}
		installDir = dir
	}
	envValues := map[string]string{}
	if len(entry.Auth.Env) > 0 {
		fmt.Fprintln(out)
		say(ui, "  Configure credentials:", ansiCyan)
		values, err := promptEnvVars(ctx, env, ui, entry.Auth.Env, preloadedEnv)
		if err != nil {
			return err
		}
		envValues = values
	}
	if entry.Auth.Type == "oauth" && yTruthy(entry.Auth.Provider) {
		p := pyStr(entry.Auth.Provider)
		say(ui, fmt.Sprintf("  This MCP uses %s OAuth. Run `icebath auth %s` if you have not already authenticated.", p, p), ansiYellow)
	} else if entry.Auth.Type == "oauth" {
		client := "native OAuth 2.1"
		if yTruthy(entry.Auth.OAuth["client_id"]) {
			client = "your pre-registered OAuth client"
		}
		say(ui, fmt.Sprintf("  This MCP uses %s; tokens will be acquired on first connection (browser flow).", client), ansiDim)
	}
	priorSelection, err := readPriorToolList(ctx, entry.Name, "include")
	if err != nil {
		return err
	}
	priorExclude, err := readPriorToolList(ctx, entry.Name, "exclude")
	if err != nil {
		return err
	}
	serverCfg, err := buildServerConfig(entry, installDir)
	if err != nil {
		return err
	}
	for _, spec := range entry.Auth.Env {
		if v, ok := envValues[spec.Name]; ok && !spec.Secret {
			serverCfg = inlineNonSecretValue(serverCfg, spec.Name, v).(map[string]any)
		}
	}
	serverCfg["enabled"] = enable
	saved, err := SaveServer(ctx, ui, entry.Name, serverCfg)
	if err != nil {
		return err
	}
	if !saved {
		return catalogErrClass("config_rejected", "catalog entry '%s' rejected: suspicious command/args configuration", entry.Name)
	}
	if err := applyToolSelection(ctx, env, ui, entry, priorSelection, priorExclude); err != nil {
		return err
	}
	fmt.Fprintln(out)
	state := "disabled"
	if enable {
		state = "enabled"
	}
	say(ui, fmt.Sprintf("  ✓ Installed '%s' (%s). Start a new icebath session to load its tools.", entry.Name, state))
	if entry.PostInstall != "" {
		fmt.Fprintln(out)
		for _, line := range pySplitlines(pyStrip(entry.PostInstall)) {
			say(ui, "  "+line, ansiDim)
		}
	}
	fmt.Fprintln(out)
	return nil
}

// Removes a catalog installed server from config and optionally its clone, true when anything was removed
func UninstallEntry(ctx context.Context, name string, purgeInstallDir bool) (bool, error) {
	removed, err := RemoveServer(ctx, name)
	if err != nil {
		return false, err
	}
	if purgeInstallDir {
		root, err := catalogInstallRoot(ctx)
		if err != nil {
			return removed, err
		}
		clone := filepath.Join(root, name)
		if _, err := os.Stat(clone); err == nil {
			if err := tools.RmtreeReadonly(clone, false); err != nil {
				return removed, err
			}
			removed = true
		}
	}
	return removed, nil
}
