// The iron-proxy egress firewall of a profile: the program, its certificate authority, the tokens it swaps for the real keys, the
// proxy.yaml it runs with and the daemon itself, kept under the profile's proxy folder
package terminal

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/netip"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"go.yaml.in/yaml/v3"

	"github.com/nickheyer/icebath/internal/config"
	"github.com/nickheyer/icebath/internal/packages"
	ironpackages "github.com/nickheyer/icebath/internal/plugins/defaults/egress/ironproxy/packages"
	"github.com/nickheyer/icebath/internal/secrets"
	"github.com/nickheyer/icebath/pkg/fsx"
)

const (
	runTimeout = 30 * time.Second
	// Longest a started daemon may take to listen
	startupGrace = 5 * time.Second
	// Variable the daemon reads its management API key from
	ManagementKeyEnv = "ICEBATH_IRON_PROXY_MGMT_KEY"
	// Port of the management API after the tunnel's
	managementOffset = 2
	reloadTimeout    = 15 * time.Second
	// Variable stamped on a daemon this process started, which tells it from another process that took its pid
	NonceEnv  = "ICEBATH_IRON_PROXY_NONCE"
	nonceRead = 256
)

// Hosts sandboxes may reach for inference unless the settings add others
var DefaultHosts = []string{"openrouter.ai", "*.openrouter.ai", "api.openai.com", "api.anthropic.com", "generativelanguage.googleapis.com",
	"api.x.ai", "api.mistral.ai", "api.groq.com", "api.together.xyz", "api.deepseek.com"}

// Provider whose key the proxy swaps: its variable, the hosts it is swapped on, the headers it is found in and the other variables that
// name the same key
type provider struct {
	env     string
	hosts   []string
	headers []string
	aliases []string
}

// Providers whose key goes as a bearer token
var bearers = []provider{
	{env: "OPENROUTER_API_KEY", hosts: []string{"openrouter.ai", "*.openrouter.ai"}}, {env: "OPENAI_API_KEY", hosts: []string{"api.openai.com"}},
	{env: "GROQ_API_KEY", hosts: []string{"api.groq.com"}}, {env: "TOGETHER_API_KEY", hosts: []string{"api.together.xyz"}},
	{env: "DEEPSEEK_API_KEY", hosts: []string{"api.deepseek.com"}}, {env: "MISTRAL_API_KEY", hosts: []string{"api.mistral.ai"}},
	{env: "XAI_API_KEY", hosts: []string{"api.x.ai"}},
}

// Providers whose key goes in another header
var headered = []provider{
	{env: "ANTHROPIC_API_KEY", hosts: []string{"api.anthropic.com"}, headers: []string{"x-api-key", "Authorization"}},
	{env: "AZURE_OPENAI_API_KEY", hosts: []string{"*.openai.azure.com", "*.cognitiveservices.azure.com", "*.services.ai.azure.com"},
		headers: []string{"api-key", "Authorization"}},
	{env: "GEMINI_API_KEY", hosts: []string{"generativelanguage.googleapis.com"}, headers: []string{"x-goog-api-key"}, aliases: []string{"GOOGLE_API_KEY"}},
}

// Credentials no header swap can stand in for: signed requests and service account files
var Uncoverable = []string{"AWS_ACCESS_KEY_ID", "AWS_SECRET_ACCESS_KEY", "GOOGLE_APPLICATION_CREDENTIALS"}

// Networks the proxy refuses to connect to unless the settings name others, whatever the allowed hosts resolve to
var DefaultDenyCIDRs = []string{"127.0.0.0/8", "::1/128", "169.254.0.0/16", "fe80::/10", "10.0.0.0/8", "172.16.0.0/12", "192.168.0.0/16", "fc00::/7",
	"::ffff:0:0/96", "100.64.0.0/10", "198.18.0.0/15"}

// Variables of the proxy's own process that would send its traffic through another proxy
var recursion = []string{"HTTPS_PROXY", "https_proxy", "HTTP_PROXY", "http_proxy", "ALL_PROXY", "all_proxy", "NO_PROXY", "no_proxy"}

// Names of the bearer providers' variables, sorted
func BearerNames() []string {
	names := make([]string, len(bearers))
	for i, p := range bearers {
		names[i] = p.env
	}
	slices.Sort(names)
	return names
}

// How the proxy of a profile stands
type Status struct {
	Binary, Version string
	// proxy.yaml and the certificate, empty while not generated
	Config, Authority string
	PID               int
	Listening         bool
	Port              int
}

// proxy.yaml and the certificate are on disk
func (s Status) Configured() bool {
	return s.Config != "" && fsx.IsFile(s.Config) && s.Authority != "" && fsx.IsFile(s.Authority)
}

// One provider key the proxy swaps for a token of its own
type Mapping struct {
	Token   string   `json:"proxy_token"`
	Env     string   `json:"env_name"`
	Hosts   []string `json:"upstream_hosts"`
	Headers []string `json:"match_headers"`
	Aliases []string `json:"alias_env_names"`
}

// The proxies of every profile, and the stamps of the daemons this process started
type Egress struct {
	packages *packages.Registry
	secrets  *secrets.Registry
	mu       sync.Mutex
	nonces   map[string]string
	versions map[string]string
}

func newEgress(pk *packages.Registry, sources *secrets.Registry) *Egress {
	return &Egress{packages: pk, secrets: sources, nonces: map[string]string{}, versions: map[string]string{}}
}

// Folder of the profile's proxy, made 0700 when asked
func (e *Egress) dir(cfg *config.Config, make bool) (string, error) {
	dir := filepath.Join(cfg.ProfileDir(cfg.Profile()), "proxy")
	if !make {
		return dir, nil
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
	_ = os.Chmod(dir, 0o700)
	return dir, nil
}

// Folder of the profile's proxy
func (e *Egress) Dir(cfg *config.Config) string {
	dir, _ := e.dir(cfg, false)
	return dir
}

// The iron-proxy program: on the search path, else in the package store, else installed now when asked and the profile allows it
func (e *Egress) Find(ctx context.Context, cfg *config.Config, install bool) string {
	return e.packages.Host().Program(ctx, cfg, ironpackages.IronProxy, "iron-proxy", install)
}

// Installs the pinned program as a person asked, its entry hashed again and repaired, and answers it
func (e *Egress) Install(ctx context.Context, cfg *config.Config) (string, error) {
	binary, err := e.packages.Host().Provide(ctx, cfg, ironpackages.IronProxy)
	if err != nil {
		return "", err
	}
	e.mu.Lock()
	delete(e.versions, binary)
	e.mu.Unlock()
	return binary, nil
}

// What the program says of its version, kept by its path; empty when it says nothing
func (e *Egress) Version(ctx context.Context, binary string) string {
	e.mu.Lock()
	known, ok := e.versions[binary]
	e.mu.Unlock()
	if ok {
		return known
	}
	out, err := secrets.Run(ctx, []string{binary, "--version"}, ironpackages.Infrastructure(), runTimeout, "iron-proxy", "iron-proxy --version timed out", nil)
	if err != nil {
		return ""
	}
	text := strings.TrimSpace(out.Stdout)
	if text == "" {
		text = strings.TrimSpace(out.Stderr)
	}
	if text != "" {
		e.mu.Lock()
		e.versions[binary] = text
		e.mu.Unlock()
	}
	return text
}

// Output of a quick command and how it ended
func quick(ctx context.Context, timeout time.Duration, argv ...string) (secrets.Output, error) {
	return secrets.Run(ctx, argv, nil, timeout, argv[0], strings.Join(argv, " ")+" timed out", nil)
}

func failed(out secrets.Output, err error) string {
	if err != nil {
		return err.Error()
	}
	if text := strings.TrimSpace(out.Stderr); text != "" {
		return text
	}
	return fmt.Sprintf("exit status %d", out.Code)
}

// Certificate and key of the authority the proxy signs with, made for ten years through the host's openssl when there are none or when forced
func (e *Egress) Authority(ctx context.Context, cfg *config.Config, force bool) (string, string, error) {
	dir, err := e.dir(cfg, true)
	if err != nil {
		return "", "", err
	}
	crt, key := filepath.Join(dir, "ca.crt"), filepath.Join(dir, "ca.key")
	if fsx.IsFile(crt) && fsx.IsFile(key) && !force {
		return crt, key, nil
	}
	if _, err := exec.LookPath("openssl"); err != nil {
		return "", "", errors.New("openssl not found on PATH. Install OpenSSL (apt: `openssl`, brew: `openssl`) to generate the iron-proxy CA cert.")
	}
	scratch, err := os.MkdirTemp("", "icebath-proxy-ca-")
	if err != nil {
		return "", "", err
	}
	defer os.RemoveAll(scratch)
	tmpKey, tmpCrt := filepath.Join(scratch, "ca.key"), filepath.Join(scratch, "ca.crt")
	if out, err := quick(ctx, time.Minute, "openssl", "genrsa", "-out", tmpKey, "4096"); err != nil || out.Code != 0 {
		return "", "", fmt.Errorf("openssl genrsa failed: %s", failed(out, err))
	}
	if out, err := quick(ctx, time.Minute, "openssl", "req", "-x509", "-new", "-nodes", "-key", tmpKey, "-sha256", "-days", "3650", "-subj",
		"/CN=icebath iron-proxy CA", "-addext", "basicConstraints=critical,CA:TRUE", "-addext", "keyUsage=critical,keyCertSign", "-out", tmpCrt); err != nil || out.Code != 0 {
		return "", "", fmt.Errorf("openssl req failed: %s", failed(out, err))
	}
	keyBytes, err := os.ReadFile(tmpKey)
	if err != nil {
		return "", "", err
	}
	if err := fsx.Write(key, keyBytes, 0o600); err != nil {
		return "", "", err
	}
	crtBytes, err := os.ReadFile(tmpCrt)
	if err != nil {
		return "", "", err
	}
	if err := os.WriteFile(crt, crtBytes, 0o644); err != nil {
		return "", "", err
	}
	_ = os.Chmod(crt, 0o644)
	slog.Info("Generated iron-proxy CA at " + crt)
	return crt, key, nil
}

// Opaque token with a prefix a person recognizes and 128 random bits in hex
func Mint(prefix string) string {
	raw := make([]byte, 32)
	rand.Read(raw)
	sum := sha256.Sum256(raw)
	return prefix + "-" + hex.EncodeToString(sum[:])[:32]
}

// Contents of a file without a byte order mark or surrounding space, empty when it can't be read
func textOf(path string) string {
	data, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(strings.TrimPrefix(string(data), "\xef\xbb\xbf"))
}

func (e *Egress) managementPath(cfg *config.Config) string {
	return filepath.Join(e.Dir(cfg), "management.token")
}

// Bearer key of the daemon's management API, minted the first time or when forced
func (e *Egress) Management(cfg *config.Config, force bool) (string, error) {
	if _, err := e.dir(cfg, true); err != nil {
		return "", err
	}
	path := e.managementPath(cfg)
	if !force {
		if existing := textOf(path); existing != "" {
			return existing, nil
		}
	}
	token := Mint("icebath-mgmt")
	return token, writePrivate(path, []byte(token))
}

// Host and port of a listen address, an empty host meaning loopback
func listen(address string) (string, int, bool) {
	at := strings.LastIndex(address, ":")
	if at < 0 {
		return "", 0, false
	}
	port, err := strconv.Atoi(address[at+1:])
	if err != nil {
		return "", 0, false
	}
	host := address[:at]
	if host == "" {
		host = "127.0.0.1"
	}
	return host, port, true
}

func (e *Egress) configPath(cfg *config.Config) string {
	return filepath.Join(e.Dir(cfg), "proxy.yaml")
}

// Host and port of the first of the keys a section of a proxy.yaml sets
func configListen(path, section string, keys ...string) (string, int, bool) {
	data, err := os.ReadFile(path)
	if err != nil {
		return "", 0, false
	}
	var doc map[string]any
	if yaml.Unmarshal(data, &doc) != nil {
		return "", 0, false
	}
	block, _ := doc[section].(map[string]any)
	for _, key := range keys {
		if value, _ := block[key].(string); value != "" {
			return listen(value)
		}
	}
	return "", 0, false
}

// Where the daemon listens for sandboxes: what proxy.yaml says, else loopback on the default port
func (e *Egress) target(cfg *config.Config) (string, int) {
	if host, port, ok := configListen(e.configPath(cfg), "proxy", "tunnel_listen", "http_listen"); ok {
		return host, port
	}
	return "127.0.0.1", 9090
}

// Answers of the management API a reload explains
var reloadErrors = map[int]string{
	422: "iron-proxy rejected the new config (validation failed; the running ruleset is unchanged): {body}",
	401: "management API rejected our key (401).  The running daemon was started with a different management.token — run `icebath egress restart`.",
}

// Reloads the running daemon's rules from proxy.yaml through its management API, a config it refuses leaving the running one in place
func (e *Egress) Reload(ctx context.Context, cfg *config.Config) error {
	if pid := e.pid(cfg); pid == 0 || !e.alive(cfg, pid) {
		return errors.New("iron-proxy is not running — nothing to reload.  Run `icebath egress start`.")
	}
	host, port, ok := configListen(e.configPath(cfg), "management", "listen")
	if !ok {
		return errors.New("The generated proxy.yaml has no management listener (written before reload support).  Re-run `icebath egress setup` and use `icebath egress restart` this one time.")
	}
	token := textOf(e.managementPath(cfg))
	if token == "" {
		return errors.New("management.token is missing — re-run `icebath egress setup`, then `icebath egress restart`.")
	}
	ctx, cancel := context.WithTimeout(ctx, reloadTimeout)
	defer cancel()
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, fmt.Sprintf("http://%s/v1/reload", net.JoinHostPort(host, strconv.Itoa(port))), nil)
	if err != nil {
		return err
	}
	request.Header.Set("Authorization", "Bearer "+token)
	client := &http.Client{Transport: &http.Transport{Proxy: nil}}
	response, err := client.Do(request)
	if err != nil {
		return fmt.Errorf("could not reach the management API at %s:%d (%v).  If the daemon was started before reload support, run `icebath egress restart` once.", host, port, err)
	}
	defer response.Body.Close()
	if response.StatusCode == http.StatusOK {
		return nil
	}
	body, _ := io.ReadAll(io.LimitReader(response.Body, 500))
	if response.StatusCode >= 400 {
		message, ok := reloadErrors[response.StatusCode]
		if !ok {
			message = "management reload failed (HTTP {code}): {body}"
		}
		return errors.New(strings.NewReplacer("{code}", strconv.Itoa(response.StatusCode), "{body}", string(body)).Replace(message))
	}
	return fmt.Errorf("management API returned unexpected status %d", response.StatusCode)
}

// Where the daemon listens by default: the Docker bridge on Linux, so sandboxes reach it, else loopback; never every interface
func (e *Egress) defaultListen(ctx context.Context, port int) string {
	if runtime.GOOS == "linux" {
		if bridge := bridgeAddress(ctx); bridge != "" {
			return fmt.Sprintf("%s:%d", bridge, port)
		}
		slog.Warn("No docker bridge (docker0) detected — binding iron-proxy to loopback only.  Docker sandboxes will NOT be able to reach the proxy until it is restarted with docker running.")
	}
	return fmt.Sprintf("127.0.0.1:%d", port)
}

// Addresses no bridge has
var (
	sharedSpace = netip.MustParsePrefix("100.64.0.0/10")
	reserved    = []netip.Prefix{netip.MustParsePrefix("0.0.0.0/8"), netip.MustParsePrefix("240.0.0.0/4")}
)

// IPv4 address of docker0, checked to be a private one so a hostile ip on the path can't pick where the proxy binds
func bridgeAddress(ctx context.Context) string {
	out, err := quick(ctx, 2*time.Second, "ip", "-4", "-o", "addr", "show", "docker0")
	if err != nil || out.Code != 0 {
		return ""
	}
	candidate := ""
	for line := range strings.Lines(out.Stdout) {
		parts := strings.Fields(line)
		for i := 0; i+1 < len(parts); i++ {
			if parts[i] == "inet" {
				candidate, _, _ = strings.Cut(parts[i+1], "/")
				break
			}
		}
		if candidate != "" {
			break
		}
	}
	addr, err := netip.ParseAddr(candidate)
	if err != nil || !addr.Is4() {
		return ""
	}
	global := !sharedSpace.Contains(addr) && !addr.IsPrivate()
	odd := slices.ContainsFunc(reserved, func(p netip.Prefix) bool { return p.Contains(addr) })
	if addr.IsUnspecified() || addr.IsLoopback() || addr.IsMulticast() || addr.IsLinkLocalUnicast() || odd || global {
		slog.Warn(fmt.Sprintf("Refusing suspicious docker bridge IP %s reported by `ip`; skipping bridge bind.", candidate))
		return ""
	}
	return addr.String()
}

// What proxy.yaml is made of
type Plan struct {
	Mappings  []Mapping
	CA, Key   string
	Port      int
	Hosts     []string
	DenyCIDRs []string
	// The settings hold their own list of networks to refuse, an empty one refusing none
	DenySet bool
}

// proxy.yaml: the allowlist of hosts, the token swaps, the listeners and the certificate; the real keys come from the daemon's own
// environment, never this file
func (e *Egress) Build(ctx context.Context, plan Plan) map[string]any {
	port := cmpPort(plan.Port)
	hosts := slices.Clone(DefaultHosts)
	if plan.Hosts != nil {
		hosts = slices.Clone(plan.Hosts)
	}
	for _, m := range plan.Mappings {
		for _, h := range m.Hosts {
			if !slices.Contains(hosts, h) {
				hosts = append(hosts, h)
			}
		}
	}
	deny := slices.Clone(DefaultDenyCIDRs)
	if plan.DenySet {
		deny = append([]string{}, plan.DenyCIDRs...)
	}
	var rules []any
	for _, m := range plan.Mappings {
		headers := m.Headers
		if len(headers) == 0 {
			headers = []string{"Authorization"}
		}
		var on []any
		for _, h := range m.Hosts {
			on = append(on, map[string]any{"host": h})
		}
		rules = append(rules, map[string]any{
			"source":  map[string]any{"type": "env", "var": m.Env},
			"replace": map[string]any{"proxy_value": m.Token, "match_headers": headers, "match_query": true, "match_body": false, "require": true},
			"rules":   on,
		})
	}
	primary := e.defaultListen(ctx, port)
	bind := primary[:strings.LastIndex(primary, ":")]
	return map[string]any{
		"dns": map[string]any{"listen": "127.0.0.1:0", "proxy_ip": "127.0.0.1"},
		"proxy": map[string]any{
			"tunnel_listen": primary, "http_listen": fmt.Sprintf("%s:%d", bind, port+1), "https_listen": "127.0.0.1:0",
			"max_request_body_bytes": 16 * 1024 * 1024, "max_response_body_bytes": 0, "upstream_response_header_timeout": "120s", "upstream_deny_cidrs": deny,
		},
		"metrics":    map[string]any{"listen": "127.0.0.1:0"},
		"management": map[string]any{"listen": fmt.Sprintf("127.0.0.1:%d", port+managementOffset), "api_key_env": ManagementKeyEnv},
		"tls":        map[string]any{"ca_cert": plan.CA, "ca_key": plan.Key, "cert_cache_size": 1000, "leaf_cert_expiry_hours": 168},
		"transforms": []any{
			map[string]any{"name": "allowlist", "config": map[string]any{"domains": hosts}},
			map[string]any{"name": "secrets", "config": map[string]any{"secrets": rules}},
		},
		"log": map[string]any{"level": "info"},
	}
}

func cmpPort(port int) int {
	if port == 0 {
		return 9090
	}
	return port
}

// Makes the audit log beforehand, 0600
func Audit(path string) error {
	f, err := appendPrivate(path, true)
	if err != nil {
		return fmt.Errorf("Refusing to start: could not pre-create audit log %s with restrictive permissions (%v).  Move or chmod any existing file at that path and retry.", path, err)
	}
	return f.Close()
}

// Writes proxy.yaml, 0600 from the start
func (e *Egress) WriteConfig(cfg *config.Config, doc map[string]any) (string, error) {
	data, err := yaml.Marshal(doc)
	if err != nil {
		return "", err
	}
	if _, err := e.dir(cfg, true); err != nil {
		return "", err
	}
	path := e.configPath(cfg)
	return path, fsx.Write(path, data, 0o600)
}

func (e *Egress) mappingsPath(cfg *config.Config) string {
	return filepath.Join(e.Dir(cfg), "mappings.json")
}

// Keeps the tokens sandboxes see in mappings.json
func (e *Egress) WriteMappings(cfg *config.Config, mappings []Mapping) (string, error) {
	for i := range mappings {
		for _, list := range []*[]string{&mappings[i].Hosts, &mappings[i].Headers, &mappings[i].Aliases} {
			if *list == nil {
				*list = []string{}
			}
		}
	}
	if mappings == nil {
		mappings = []Mapping{}
	}
	data, err := json.MarshalIndent(map[string]any{"version": 1, "tokens": mappings}, "", "  ")
	if err != nil {
		return "", err
	}
	if _, err := e.dir(cfg, true); err != nil {
		return "", err
	}
	path := e.mappingsPath(cfg)
	return path, fsx.Write(path, data, 0o600)
}

// Tokens of mappings.json, none when it can't be read
func (e *Egress) Mappings(cfg *config.Config) []Mapping {
	data, err := os.ReadFile(e.mappingsPath(cfg))
	if err != nil {
		if !errors.Is(err, os.ErrNotExist) {
			slog.Warn(fmt.Sprintf("Failed to read iron-proxy mappings.json: %v", err))
		}
		return nil
	}
	var doc struct {
		Tokens []json.RawMessage `json:"tokens"`
	}
	if err := json.Unmarshal(bytes.TrimPrefix(data, []byte("\xef\xbb\xbf")), &doc); err != nil {
		slog.Warn(fmt.Sprintf("Failed to read iron-proxy mappings.json: %v", err))
		return nil
	}
	var out []Mapping
	for _, raw := range doc.Tokens {
		var m Mapping
		if json.Unmarshal(raw, &m) != nil || m.Token == "" || m.Env == "" {
			continue
		}
		if len(m.Headers) == 0 {
			m.Headers = []string{"Authorization"}
		}
		out = append(out, m)
	}
	return out
}

// Names set to something in the environment given, the profile's own for none
func present(cfg *config.Config, names []string) map[string]bool {
	out := map[string]bool{}
	if names != nil {
		for _, name := range names {
			out[name] = true
		}
		return out
	}
	for name, value := range cfg.Environ() {
		if value != "" {
			out[name] = true
		}
	}
	return out
}

// A fresh token for every known provider whose variable, or one of its aliases, is set
func Discover(cfg *config.Config, names []string) []Mapping {
	set := present(cfg, names)
	var out []Mapping
	for _, p := range append(slices.Clone(bearers), headered...) {
		if !set[p.env] && !slices.ContainsFunc(p.aliases, func(a string) bool { return set[a] }) {
			continue
		}
		headers := p.headers
		if headers == nil {
			headers = []string{"Authorization"}
		}
		out = append(out, Mapping{Token: Mint(strings.ReplaceAll(strings.ToLower(p.env), "_api_key", "")), Env: p.env, Hosts: slices.Clone(p.hosts),
			Headers: slices.Clone(headers), Aliases: slices.Clone(p.aliases)})
	}
	return out
}

// Provider credentials set that no header swap can stand in for
func Uncovered(cfg *config.Config, names []string) []string {
	set := present(cfg, names)
	var out []string
	for _, name := range Uncoverable {
		if set[name] {
			out = append(out, name)
		}
	}
	return out
}

// Discovered mappings with the tokens of those there were kept, their hosts refreshed; a rotation mints every token anew
func Merge(existing, discovered []Mapping, rotate bool) []Mapping {
	kept := map[string]Mapping{}
	if !rotate {
		for _, m := range existing {
			kept[m.Env] = m
		}
	}
	out := make([]Mapping, 0, len(discovered))
	for _, d := range discovered {
		if prev, ok := kept[d.Env]; ok {
			d.Token = prev.Token
		}
		out = append(out, d)
	}
	return out
}

// Copies mappings.json beside itself before a rotation, so the tokens can be recovered by hand; empty when there is none to copy
func (e *Egress) Backup(cfg *config.Config) (string, error) {
	source := e.mappingsPath(cfg)
	if !fsx.IsFile(source) {
		return "", nil
	}
	backup := source + ".rotated-" + time.Now().Format("20060102T150405")
	return backup, fsx.Copy(source, backup)
}

func (e *Egress) pidPath(cfg *config.Config) string {
	return filepath.Join(e.Dir(cfg), "iron-proxy.pid")
}

func (e *Egress) noncePath(cfg *config.Config) string {
	return filepath.Join(e.Dir(cfg), "iron-proxy.nonce")
}

// Pid of the pidfile, 0 when there is none
func (e *Egress) pid(cfg *config.Config) int {
	pid, err := strconv.Atoi(textOf(e.pidPath(cfg)))
	if err != nil || pid <= 0 {
		return 0
	}
	return pid
}

// Stamp of the daemon kept on disk, empty when missing, unreadable or not this user's
func (e *Egress) persistedNonce(cfg *config.Config) string {
	f, err := openNoFollow(e.noncePath(cfg), os.O_RDONLY, 0)
	if err != nil {
		return ""
	}
	defer f.Close()
	if _, ours, err := owner(f); err != nil || !ours {
		return ""
	}
	buf := make([]byte, nonceRead)
	n, _ := f.Read(buf)
	return strings.TrimSpace(strings.ToValidUTF8(string(buf[:n]), ""))
}

// Whether the pid is a live iron-proxy, the stamp of the daemon telling it from a process that took its pid since
func (e *Egress) alive(cfg *config.Config, pid int) bool {
	if pid <= 0 || !exists(pid) {
		return false
	}
	e.mu.Lock()
	candidates := []string{e.nonces[e.Dir(cfg)]}
	e.mu.Unlock()
	candidates = append(candidates, e.persistedNonce(cfg))
	if env, err := os.ReadFile(fmt.Sprintf("/proc/%d/environ", pid)); err == nil {
		for _, nonce := range candidates {
			if nonce != "" && bytes.Contains(env, []byte(NonceEnv+"="+nonce)) {
				return true
			}
		}
	}
	if fsx.IsFile("/proc/self/cmdline") {
		cmdline, err := os.ReadFile(fmt.Sprintf("/proc/%d/cmdline", pid))
		if err != nil {
			return !errors.Is(err, os.ErrNotExist)
		}
		argv0, _, _ := bytes.Cut(cmdline, []byte{0})
		return strings.HasPrefix(filepath.Base(string(argv0)), "iron-proxy")
	}
	if out, err := quick(context.Background(), 2*time.Second, "ps", "-p", strconv.Itoa(pid), "-o", "comm="); err == nil && out.Code == 0 {
		return strings.HasPrefix(filepath.Base(strings.TrimSpace(out.Stdout)), "iron-proxy")
	}
	return true
}

// Whether something accepts connections at the address
func listening(host string, port int) bool {
	conn, err := net.DialTimeout("tcp", net.JoinHostPort(host, strconv.Itoa(port)), 500*time.Millisecond)
	if err != nil {
		return false
	}
	conn.Close()
	return true
}

// How the profile's proxy stands, nothing changed to read it
func (e *Egress) Status(ctx context.Context, cfg *config.Config) Status {
	host, port := e.target(cfg)
	status := Status{Port: port}
	if status.Binary = e.Find(ctx, cfg, false); status.Binary != "" {
		status.Version = e.Version(ctx, status.Binary)
	}
	if path := e.configPath(cfg); fsx.IsFile(path) {
		status.Config = path
	}
	if path := filepath.Join(e.Dir(cfg), "ca.crt"); fsx.IsFile(path) {
		status.Authority = path
	}
	if pid := e.pid(cfg); pid != 0 && e.alive(cfg, pid) {
		status.PID, status.Listening = pid, listening(host, port)
	}
	return status
}

// Last lines of the daemon's log
func tail(path string, lines int) string {
	data, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return "(no log file)"
		}
		return fmt.Sprintf("(could not read log: %v)", err)
	}
	if len(data) > 8192 {
		data = data[len(data)-8192:]
	}
	all := strings.Split(strings.TrimSuffix(strings.ToValidUTF8(string(data), "�"), "\n"), "\n")
	if len(all) > lines {
		all = all[len(all)-lines:]
	}
	return strings.Join(all, "\n")
}

// How a daemon is started
type Start struct {
	Install bool
	// Fetch the mapped keys from Bitwarden fresh, missing ones coming from the profile's environment only when Fallback allows it
	Bitwarden, Fallback bool
}

// Starts iron-proxy as a daemon of its own session with proxy.yaml, nothing done while one runs; the context ending stops the wait for it
func (e *Egress) Start(ctx context.Context, cfg *config.Config, start Start) (Status, error) {
	if pid := e.pid(cfg); pid != 0 && e.alive(cfg, pid) {
		return e.Status(ctx, cfg), nil
	}
	binary := e.Find(ctx, cfg, start.Install)
	if binary == "" {
		return Status{}, errors.New("iron-proxy binary not available — run `icebath egress install`.")
	}
	path := e.configPath(cfg)
	if !fsx.IsFile(path) {
		return Status{}, fmt.Errorf("iron-proxy config not found at %s. Run `icebath egress setup` first.", path)
	}
	env, err := e.daemonEnv(ctx, cfg, start)
	if err != nil {
		return Status{}, err
	}
	if _, _, ok := configListen(path, "management", "listen"); ok {
		token, err := e.Management(cfg, false)
		if err != nil {
			return Status{}, err
		}
		env[ManagementKeyEnv] = token
	}
	stamp := make([]byte, 16)
	rand.Read(stamp)
	sum := sha256.Sum256(stamp)
	nonce := hex.EncodeToString(sum[:])
	dir, err := e.dir(cfg, true)
	if err != nil {
		return Status{}, err
	}
	e.mu.Lock()
	e.nonces[dir] = nonce
	e.mu.Unlock()
	env[NonceEnv] = nonce
	logPath := filepath.Join(dir, "iron-proxy.log")
	cmd, err := spawn(binary, path, env, logPath)
	if err != nil {
		return Status{}, err
	}
	pidfile := e.pidPath(cfg)
	if err := e.writePid(cfg, pidfile, cmd.Process.Pid, nonce); err != nil {
		stopChild(cmd, 2*time.Second)
		return Status{}, err
	}
	exited := make(chan struct{})
	go func() {
		cmd.Wait()
		close(exited)
	}()
	abort := func(text string, kill bool) error {
		log := tail(logPath, 20)
		if kill {
			stopChild(cmd, 2*time.Second)
		}
		_ = os.Remove(pidfile)
		return fmt.Errorf("%sLast log lines:\n%s", text, log)
	}
	host, port := e.target(cfg)
	deadline := time.Now().Add(startupGrace)
	for {
		select {
		case <-exited:
			return Status{}, abort(fmt.Sprintf("iron-proxy exited immediately (code %d). ", cmd.ProcessState.ExitCode()), false)
		case <-ctx.Done():
			stopChild(cmd, 2*time.Second)
			_ = os.Remove(pidfile)
			return Status{}, errors.New("interrupted while waiting for iron-proxy to start")
		default:
		}
		if listening(host, port) {
			slog.Info(fmt.Sprintf("Started iron-proxy pid=%d config=%s", cmd.Process.Pid, path))
			return e.Status(ctx, cfg), nil
		}
		if !time.Now().Before(deadline) {
			return Status{}, abort(fmt.Sprintf("iron-proxy did not bind %s:%d within %.0fs.  Process was killed.  ", host, port, startupGrace.Seconds()), true)
		}
		time.Sleep(100 * time.Millisecond)
	}
}

// Writes the pidfile only if there is none, refusing a link or a file of another user, a stale one replaced once, and the stamp beside it
func (e *Egress) writePid(cfg *config.Config, pidfile string, pid int, nonce string) error {
	flags := os.O_WRONLY | os.O_CREATE | os.O_EXCL
	f, err := openNoFollow(pidfile, flags, 0o600)
	if errors.Is(err, os.ErrExist) {
		if existing := e.pid(cfg); existing != 0 && e.alive(cfg, existing) {
			return fmt.Errorf("Another iron-proxy start appears to be in progress (pidfile %s -> pid %d).  Run `icebath egress stop` if that proxy is stuck.", pidfile, existing)
		}
		_ = os.Remove(pidfile)
		f, err = openNoFollow(pidfile, flags, 0o600)
	}
	if err != nil {
		return fmt.Errorf("Refusing to write pidfile %s: %v.  Remove that path manually and retry.", pidfile, err)
	}
	uid, ours, err := owner(f)
	if err != nil {
		f.Close()
		return err
	}
	if !ours {
		f.Close()
		return fmt.Errorf("pidfile %s has unexpected owner uid=%s", pidfile, uid)
	}
	_, err = f.WriteString(strconv.Itoa(pid))
	f.Close()
	if err != nil {
		return err
	}
	_ = writePrivate(e.noncePath(cfg), []byte(nonce))
	return nil
}

// The daemon's environment: infrastructure variables, the real keys the mappings name from the profile's environment or fresh from
// Bitwarden, never anything that would send its traffic through another proxy
func (e *Egress) daemonEnv(ctx context.Context, cfg *config.Config, start Start) (map[string]string, error) {
	env := ironpackages.Infrastructure()
	needed := map[string]bool{}
	aliases := map[string][]string{}
	for _, m := range e.Mappings(cfg) {
		needed[m.Env] = true
		if len(m.Aliases) > 0 {
			aliases[m.Env] = m.Aliases
		}
	}
	for name := range needed {
		if value, ok := cfg.LookupEnv(name); ok {
			env[name] = value
			continue
		}
		for _, alias := range aliases[name] {
			if value := cfg.Getenv(alias); value != "" {
				env[name] = value
				break
			}
		}
	}
	if start.Bitwarden {
		if err := e.refresh(ctx, cfg, env, needed, start.Fallback); err != nil {
			return nil, err
		}
	}
	for _, name := range recursion {
		delete(env, name)
	}
	if _, set := env["NO_COLOR"]; !set {
		env["NO_COLOR"] = "1"
	}
	return env, nil
}

// An error unless the profile opted into the environment fallback, a warning then
func shortfall(fallback bool, text, warning string) error {
	if !fallback {
		return errors.New(text)
	}
	slog.Warn(warning)
	return nil
}

// Overwrites the needed keys with values fetched from Bitwarden now, uncached; only the mapped names go in
func (e *Egress) refresh(ctx context.Context, cfg *config.Config, env map[string]string, needed map[string]bool, fallback bool) error {
	host := e.secrets.Host()
	source, ok := host.Source(Bitwarden)
	if !ok {
		return shortfall(fallback, "Bitwarden is not available to refresh the proxy's keys (credential_source=bitwarden with proxy.allow_env_fallback: false).  "+
			"Either switch to credential_source: env, or set `proxy.allow_env_fallback: true` to opt into the legacy fallback behaviour.",
			"Bitwarden is not available to refresh the proxy's keys, falling back to the profile's environment (allow_env_fallback=true).")
	}
	token := strings.TrimSpace(host.Existing(cfg)[host.TokenEnv(cfg, source.Spec())])
	if configured, _ := host.Configured(cfg, source); token == "" || !configured {
		return shortfall(fallback, "credential_source=bitwarden but the access-token env or project_id is empty.  Either set both, switch to credential_source: env, "+
			"or set `proxy.allow_env_fallback: true` to opt into the legacy fallback behaviour.",
			"credential_source=bitwarden but access-token env or project_id is empty — proxy will fall back to parent env (allow_env_fallback=true).")
	}
	result := host.Fetch(ctx, cfg, source, true)
	if !result.OK() {
		return errors.New(result.Error)
	}
	var missing []string
	for name := range needed {
		if value, ok := result.Secrets[name]; ok {
			env[name] = value
		} else {
			missing = append(missing, name)
		}
	}
	slices.Sort(missing)
	if len(missing) > 0 {
		if err := shortfall(fallback, fmt.Sprintf("Bitwarden refresh did not return secrets for %v.  Either add the secrets to your BWS project, switch to "+
			"credential_source: env via `icebath egress setup --no-bitwarden`, or set `proxy.allow_env_fallback: true` in the settings to opt into the legacy host-env fallback.", missing),
			fmt.Sprintf("Bitwarden refresh did not return secrets for %v — falling back to host env for those names (allow_env_fallback=true).", missing)); err != nil {
			return err
		}
	}
	if len(result.Warnings) > 0 {
		slog.Warn(fmt.Sprintf("Bitwarden refresh produced %d warning(s); run `icebath secrets bitwarden status` for detail.", len(result.Warnings)))
	}
	return nil
}

// Forgets the pidfile and the stamp of the daemon
func (e *Egress) forget(cfg *config.Config) error {
	if err := os.Remove(e.pidPath(cfg)); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	_ = os.Remove(e.noncePath(cfg))
	e.mu.Lock()
	delete(e.nonces, e.Dir(cfg))
	e.mu.Unlock()
	return nil
}

// Stops the daemon, true when it was running: a terminate, then a kill after five seconds unless the pid went to another process
func (e *Egress) Stop(cfg *config.Config) (bool, error) {
	pid := e.pid(cfg)
	if pid == 0 || !e.alive(cfg, pid) {
		return false, e.forget(cfg)
	}
	before := started(pid)
	if err := signal(pid, false); err != nil {
		if !gone(err) {
			return false, err
		}
		return false, e.forget(cfg)
	}
	deadline := time.Now().Add(5 * time.Second)
	exited := false
	for time.Now().Before(deadline) {
		if !e.alive(cfg, pid) {
			exited = true
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	if !exited {
		after := started(pid)
		if (before != "" && after != "" && before != after) || !e.alive(cfg, pid) {
			slog.Warn(fmt.Sprintf("iron-proxy pid=%d appears recycled before SIGKILL; not killing.", pid))
		} else if err := signal(pid, true); err != nil && !gone(err) {
			return false, err
		}
	}
	if err := e.forget(cfg); err != nil {
		return false, err
	}
	slog.Info(fmt.Sprintf("Stopped iron-proxy pid=%d", pid))
	return true, nil
}

// Start time of a process from its Linux stat file, empty elsewhere
func started(pid int) string {
	data, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
	if err != nil {
		return ""
	}
	text := string(data)
	at := strings.LastIndex(text, ")")
	if at < 0 {
		return ""
	}
	if fields := strings.Fields(text[at+1:]); len(fields) > 19 {
		return fields[19]
	}
	return ""
}

// Writes a file 0600 from its first byte, refusing a link planted at the path
func writePrivate(path string, data []byte) error {
	f, err := openNoFollow(path, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}
	defer f.Close()
	_ = f.Chmod(0o600)
	_, err = f.Write(data)
	return err
}

// Opens the path to append at 0600, refusing a link; a failed chmod fails only when strict
func appendPrivate(path string, strict bool) (*os.File, error) {
	f, err := openNoFollow(path, os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0o600)
	if err != nil {
		return nil, err
	}
	if err := f.Chmod(0o600); err != nil && strict {
		f.Close()
		return nil, err
	}
	return f, nil
}

// Starts the daemon in a session of its own with its output appended to its private log, which must be this user's
func spawn(binary, path string, env map[string]string, logPath string) (*exec.Cmd, error) {
	log, err := appendPrivate(logPath, false)
	if err != nil {
		return nil, fmt.Errorf("Refusing to write iron-proxy log %s: %v.  Remove that path manually and retry.", logPath, err)
	}
	defer log.Close()
	uid, ours, err := owner(log)
	if err != nil {
		return nil, err
	}
	if !ours {
		return nil, fmt.Errorf("iron-proxy log %s has unexpected owner uid=%s; refusing to write.", logPath, uid)
	}
	cmd := exec.Command(binary, "-config", path)
	cmd.Env = secrets.Pairs(env)
	cmd.Stdout, cmd.Stderr = log, log
	detach(cmd)
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("failed to spawn iron-proxy: %v", err)
	}
	return cmd, nil
}

// Terminates a child this process started, killing it after the grace
func stopChild(cmd *exec.Cmd, grace time.Duration) {
	if cmd.Process == nil || terminate(cmd.Process) != nil {
		return
	}
	done := make(chan struct{})
	go func() {
		cmd.Process.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(grace):
		cmd.Process.Kill()
		select {
		case <-done:
		case <-time.After(grace):
		}
	}
}
