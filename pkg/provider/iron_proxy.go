package provider

import (
	"bytes"
	"context"
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
	"os/signal"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/home"
	"gopkg.in/yaml.v3"
)

const (
	ironRunTimeout        = 30 * time.Second
	ironStartupGrace      = 5 * time.Second
	IronProxyMgmtKeyEnv   = "ICEBATH_IRON_PROXY_MGMT_KEY"
	ironMgmtPortOffset    = 2
	ironMgmtReloadTimeout = 15 * time.Second
	DefaultIronTunnelPort = 9090
	IronProxyNonceEnv     = "ICEBATH_IRON_PROXY_NONCE"
	ironNonceReadLimit    = 256
)

// Hosts allowed by default for AI inference traffic
var DefaultIronAllowedHosts = []string{
	"openrouter.ai", "*.openrouter.ai", "api.openai.com", "api.anthropic.com", "generativelanguage.googleapis.com",
	"api.x.ai", "api.mistral.ai", "api.groq.com", "api.together.xyz", "api.deepseek.com", "inference.nousresearch.com",
}

type bearerProvider struct {
	envName string
	hosts   []string
}

// Provider env var to the upstream hosts on which the bearer token is swapped
var ironBearerProviders = []bearerProvider{
	{"OPENROUTER_API_KEY", []string{"openrouter.ai", "*.openrouter.ai"}}, {"OPENAI_API_KEY", []string{"api.openai.com"}},
	{"GROQ_API_KEY", []string{"api.groq.com"}}, {"TOGETHER_API_KEY", []string{"api.together.xyz"}},
	{"DEEPSEEK_API_KEY", []string{"api.deepseek.com"}}, {"MISTRAL_API_KEY", []string{"api.mistral.ai"}},
	{"XAI_API_KEY", []string{"api.x.ai"}}, {"NOUS_API_KEY", []string{"inference.nousresearch.com"}},
}

type headerAuthProvider struct {
	envName      string
	hosts        []string
	matchHeaders []string
	aliases      []string
}

// Providers authenticating with a non Authorization header, aliases collapse into one mapping
var ironHeaderAuthProviders = []headerAuthProvider{
	{"ANTHROPIC_API_KEY", []string{"api.anthropic.com"}, []string{"x-api-key", "Authorization"}, nil},
	{"AZURE_OPENAI_API_KEY", []string{"*.openai.azure.com", "*.cognitiveservices.azure.com", "*.services.ai.azure.com"}, []string{"api-key", "Authorization"}, nil},
	{"GEMINI_API_KEY", []string{"generativelanguage.googleapis.com"}, []string{"x-goog-api-key"}, []string{"GOOGLE_API_KEY"}},
}

// Credentials static header replacement cannot swap
var ironNonBearerProviders = []string{"AWS_ACCESS_KEY_ID", "AWS_SECRET_ACCESS_KEY", "GOOGLE_APPLICATION_CREDENTIALS"}

// Default SSRF deny list refused regardless of the allowlist
var DefaultIronUpstreamDenyCIDRs = []string{
	"127.0.0.0/8", "::1/128", "169.254.0.0/16", "fe80::/10", "10.0.0.0/8", "172.16.0.0/12", "192.168.0.0/16", "fc00::/7",
	"::ffff:0:0/96", "100.64.0.0/10", "198.18.0.0/15",
}

var ironSubprocessEnvAllowlist = []string{"PATH", "HOME", "TMPDIR", "TZ", "LANG", "LC_ALL", "LC_CTYPE", "NO_COLOR", "SSL_CERT_DIR", "SSL_CERT_FILE", "SYSTEMROOT", "USERPROFILE"}

var ironSubprocessEnvStrip = []string{"HTTPS_PROXY", "https_proxy", "HTTP_PROXY", "http_proxy", "ALL_PROXY", "all_proxy", "NO_PROXY", "no_proxy"}

var (
	ironVersionMu    sync.Mutex
	ironVersionCache = map[string]string{}
	ironNonceMu      sync.Mutex
	ironProxyNonce   string
)

// IronProxyStatus is a snapshot of the proxy state
type IronProxyStatus struct {
	Enabled       bool
	BinaryPath    string
	BinaryVersion string
	ConfigPath    string
	CACertPath    string
	PID           int
	Listening     bool
	TunnelPort    int
	Warnings      []string
}

// Installed reports a binary on disk
func (s IronProxyStatus) Installed() bool { return s.BinaryPath != "" && fileExists(s.BinaryPath) }

// Configured reports both the config and the CA cert on disk
func (s IronProxyStatus) Configured() bool {
	return s.ConfigPath != "" && fileExists(s.ConfigPath) && s.CACertPath != "" && fileExists(s.CACertPath)
}

// TokenMapping is a sandbox visible proxy token to upstream credential lookup
type TokenMapping struct {
	ProxyToken    string
	RealEnvName   string
	UpstreamHosts []string
	MatchHeaders  []string
	AliasEnvNames []string
}

// IronProxy binds the iron-proxy integration to one icebath home
type IronProxy struct {
	Home string
	Log  *slog.Logger
}

func (p IronProxy) log() *slog.Logger {
	if p.Log != nil {
		return p.Log
	}
	return slog.Default()
}

func (p IronProxy) home() string {
	if p.Home != "" {
		return p.Home
	}
	return home.Process()
}

// State dir without creating it, for status probes and pidfile reads
func (p IronProxy) stateDirRO() string { return filepath.Join(p.home(), "proxy") }

// State dir created 0700, an unconditional chmod tightens a pre-existing slack umask dir
func (p IronProxy) stateDir() string {
	d := p.stateDirRO()
	_ = os.MkdirAll(d, 0o700)
	_ = os.Chmod(d, 0o700)
	return d
}

// FindIronProxy resolves the binary from PATH or the tool store, installing on a miss when asked
func (p IronProxy) FindIronProxy(installIfMissing bool) string {
	if system, err := exec.LookPath("iron-proxy"); err == nil {
		return system
	}
	if binary, ok := pmInstalledBinary("iron-proxy"); ok {
		return binary
	}
	if installIfMissing {
		binary, err := pmEnsure("iron-proxy", false)
		if err != nil {
			p.log().Warn(fmt.Sprintf("iron-proxy auto-install failed: %v", err))
			return ""
		}
		return binary
	}
	return ""
}

// InstallIronProxy is the explicit setup or repair install
func (p IronProxy) InstallIronProxy() (string, error) {
	target, err := pmEnsure("iron-proxy", true)
	if err != nil {
		return "", err
	}
	ironVersionMu.Lock()
	delete(ironVersionCache, target)
	ironVersionMu.Unlock()
	return target, nil
}

// VerifyChecksumsSignature checks checksums.txt with gpg in an ephemeral keyring, false when unavailable
func (p IronProxy) VerifyChecksumsSignature(tmp, checksumPath string) (bool, error) {
	gpg, err := exec.LookPath("gpg")
	if err != nil {
		p.log().Warn("gpg not found on PATH — skipping iron-proxy release-signature verification (SHA-256 checksum check still enforced).")
		return false, nil
	}
	sigPath, pubkeyPath := filepath.Join(tmp, "checksums.txt.asc"), filepath.Join(tmp, "public-key.asc")
	if !fileExists(sigPath) || !fileExists(pubkeyPath) {
		p.log().Warn("iron-proxy release signature assets unavailable — skipping GPG verification (SHA-256 checksum check still enforced).")
		return false, nil
	}
	gnupgHome, err := os.MkdirTemp("", "icebath-iron-signature-")
	if err != nil {
		return false, err
	}
	defer os.RemoveAll(gnupgHome)
	base := []string{gpg, "--homedir", gnupgHome, "--batch", "--no-tty"}
	imp := ironRun(append(base, "--import", pubkeyPath), 60*time.Second, nil)
	if imp.err != nil || imp.code != 0 {
		p.log().Warn(fmt.Sprintf("Could not import iron-proxy signing key — skipping GPG verification (SHA-256 still enforced): %s", truncateText(imp.stderr, 200)))
		return false, nil
	}
	verify := ironRun(append(base, "--verify", sigPath, checksumPath), 60*time.Second, nil)
	if verify.err != nil || verify.code != 0 {
		return false, fmt.Errorf("iron-proxy checksums.txt failed GPG signature verification — refusing to install (possible release-channel tampering). gpg: %s", truncateText(verify.stderr, 300))
	}
	p.log().Info("Verified iron-proxy checksums.txt GPG signature.")
	return true, nil
}

func truncateText(s string, n int) string {
	if len(s) > n {
		return s[:n]
	}
	return s
}

// IronAllowlistedEnv is the infrastructure only environment, never the operator's secrets
func IronAllowlistedEnv() map[string]string {
	env := map[string]string{}
	for _, n := range ironSubprocessEnvAllowlist {
		if v, ok := os.LookupEnv(n); ok {
			env[n] = v
		}
	}
	return env
}

type ironRunResult struct {
	stdout, stderr string
	code           int
	err            error
}

func ironRun(argv []string, timeout time.Duration, env map[string]string) ironRunResult {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
	if env != nil {
		cmd.Env = envList(env)
	}
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	err := cmd.Run()
	res := ironRunResult{stdout: stdout.String(), stderr: stderr.String()}
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		res.code = exitErr.ExitCode()
	} else if err != nil {
		res.err = err
	}
	return res
}

// IronProxyVersion is the version output stripped and cached by path, empty on failure
func (p IronProxy) IronProxyVersion(binary string) string {
	ironVersionMu.Lock()
	if v, ok := ironVersionCache[binary]; ok {
		ironVersionMu.Unlock()
		return v
	}
	ironVersionMu.Unlock()
	res := ironRun([]string{binary, "--version"}, ironRunTimeout, IronAllowlistedEnv())
	if res.err != nil {
		return ""
	}
	out := strings.TrimSpace(res.stdout)
	if out == "" {
		out = strings.TrimSpace(res.stderr)
	}
	if out != "" {
		ironVersionMu.Lock()
		ironVersionCache[binary] = out
		ironVersionMu.Unlock()
	}
	return out
}

// Creates or truncates the file 0600 from the first byte, refusing a planted symlink
func writePrivateFile(path string, data []byte) error {
	f, err := openNoFollow(path, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}
	defer f.Close()
	_ = f.Chmod(0o600)
	_, err = f.Write(data)
	return err
}

// True only when the open file is owned by the current user, a failed owner lookup counts as foreign
func fdOwnedByUs(f *os.File) bool {
	_, ours, err := fdOwner(f)
	return err == nil && ours
}

// EnsureCACert generates or returns the 10 year CA cert and key through the host openssl
func (p IronProxy) EnsureCACert(force bool) (string, string, error) {
	state := p.stateDir()
	caCrt, caKey := filepath.Join(state, "ca.crt"), filepath.Join(state, "ca.key")
	if fileExists(caCrt) && fileExists(caKey) && !force {
		return caCrt, caKey, nil
	}
	if _, err := exec.LookPath("openssl"); err != nil {
		return "", "", errors.New("openssl not found on PATH. Install OpenSSL (apt: `openssl`, brew: `openssl`) to generate the iron-proxy CA cert.")
	}
	tmpdir, err := os.MkdirTemp("", "icebath-proxy-ca-")
	if err != nil {
		return "", "", err
	}
	defer os.RemoveAll(tmpdir)
	tmpKey, tmpCrt := filepath.Join(tmpdir, "ca.key"), filepath.Join(tmpdir, "ca.crt")
	if res := ironRun([]string{"openssl", "genrsa", "-out", tmpKey, "4096"}, 60*time.Second, nil); res.err != nil || res.code != 0 {
		return "", "", fmt.Errorf("openssl genrsa failed: %s", ironErrText(res))
	}
	req := []string{"openssl", "req", "-x509", "-new", "-nodes", "-key", tmpKey, "-sha256", "-days", "3650", "-subj", "/CN=icebath iron-proxy CA",
		"-addext", "basicConstraints=critical,CA:TRUE", "-addext", "keyUsage=critical,keyCertSign", "-out", tmpCrt}
	if res := ironRun(req, 60*time.Second, nil); res.err != nil || res.code != 0 {
		return "", "", fmt.Errorf("openssl req failed: %s", ironErrText(res))
	}
	keyBytes, err := os.ReadFile(tmpKey)
	if err != nil {
		return "", "", err
	}
	staged := caKey + ".staged"
	_ = os.Remove(staged)
	if err := writePrivateFile(staged, keyBytes); err != nil {
		return "", "", err
	}
	if err := os.Rename(staged, caKey); err != nil {
		return "", "", err
	}
	crtBytes, err := os.ReadFile(tmpCrt)
	if err != nil {
		return "", "", err
	}
	if err := os.WriteFile(caCrt, crtBytes, 0o644); err != nil {
		return "", "", err
	}
	_ = os.Chmod(caCrt, 0o644)
	p.log().Info(fmt.Sprintf("Generated iron-proxy CA at %s", caCrt))
	return caCrt, caKey, nil
}

// MintProxyToken is an opaque token with a recognizable prefix and a 128 bit random hex suffix
func MintProxyToken(prefix string) string {
	raw := make([]byte, 32)
	fillRandom(raw)
	sum := sha256.Sum256(raw)
	return prefix + "-" + hex.EncodeToString(sum[:])[:32]
}

// Stripped file contents, empty when missing unreadable or empty
func readTextOrEmpty(path string) string {
	raw, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(strings.TrimPrefix(string(raw), "\xef\xbb\xbf"))
}

func (p IronProxy) managementTokenPath() string {
	return filepath.Join(p.stateDirRO(), "management.token")
}

// EnsureManagementToken returns the management API bearer key, minting it on first call
func (p IronProxy) EnsureManagementToken(force bool) (string, error) {
	p.stateDir()
	path := p.managementTokenPath()
	if !force {
		if existing := readTextOrEmpty(path); existing != "" {
			return existing, nil
		}
	}
	token := MintProxyToken("icebath-mgmt")
	if err := writePrivateFile(path, []byte(token)); err != nil {
		return "", err
	}
	return token, nil
}

// Host and port of a listen string, an empty host means loopback
func parseListen(listen string) (string, int, bool) {
	if !strings.Contains(listen, ":") {
		return "", 0, false
	}
	idx := strings.LastIndex(listen, ":")
	port, err := strconv.Atoi(listen[idx+1:])
	if err != nil {
		return "", 0, false
	}
	host := listen[:idx]
	if host == "" {
		host = "127.0.0.1"
	}
	return host, port, true
}

// Host and port from the first truthy key of a proxy.yaml section
func (p IronProxy) configListen(section string, configPath string, keys ...string) (string, int, bool) {
	if configPath == "" {
		configPath = filepath.Join(p.stateDirRO(), "proxy.yaml")
	}
	raw, err := os.ReadFile(configPath)
	if err != nil {
		return "", 0, false
	}
	var data map[string]any
	if yaml.Unmarshal(raw, &data) != nil {
		return "", 0, false
	}
	block, _ := data[section].(map[string]any)
	for _, k := range keys {
		if v := anyString(block[k]); v != "" {
			return parseListen(v)
		}
	}
	return "", 0, false
}

func (p IronProxy) readManagementListen(configPath string) (string, int, bool) {
	return p.configListen("management", configPath, "listen")
}

// Configured bind host and port to probe, the docker bridge on Linux
func (p IronProxy) probeTarget() (string, int) {
	if host, port, ok := p.configListen("proxy", "", "tunnel_listen", "http_listen"); ok {
		return host, port
	}
	return "127.0.0.1", DefaultIronTunnelPort
}

var ironReloadHTTPErrors = map[int]string{
	422: "iron-proxy rejected the new config (validation failed; the running ruleset is unchanged): {body}",
	401: "management API rejected our key (401).  The running daemon was started with a different management.token — run `icebath egress restart`.",
}

// ReloadProxy POSTs the reload endpoint, validation failures leave the running config untouched
func (p IronProxy) ReloadProxy() error {
	pid := p.readPID()
	if pid == 0 || !p.pidAlive(pid) {
		return errors.New("iron-proxy is not running — nothing to reload.  Run `icebath egress start`.")
	}
	host, port, ok := p.readManagementListen("")
	if !ok {
		return errors.New("The generated proxy.yaml has no management listener (written before reload support).  Re-run `icebath egress setup` and use `icebath egress restart` this one time.")
	}
	token := readTextOrEmpty(p.managementTokenPath())
	if token == "" {
		return errors.New("management.token is missing — re-run `icebath egress setup`, then `icebath egress restart`.")
	}
	req, err := http.NewRequest(http.MethodPost, fmt.Sprintf("http://%s:%d/v1/reload", host, port), bytes.NewReader(nil))
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	client := &http.Client{Timeout: ironMgmtReloadTimeout, Transport: &http.Transport{Proxy: nil}}
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("could not reach the management API at %s:%d (%v).  If the daemon was started before reload support, run `icebath egress restart` once.", host, port, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode == 200 {
		return nil
	}
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 500))
	if resp.StatusCode >= 400 {
		message, ok := ironReloadHTTPErrors[resp.StatusCode]
		if !ok {
			message = "management reload failed (HTTP {code}): {body}"
		}
		return errors.New(strings.NewReplacer("{code}", fmt.Sprint(resp.StatusCode), "{body}", string(body)).Replace(message))
	}
	return fmt.Errorf("management API returned unexpected status %d", resp.StatusCode)
}

// Single bind on the docker bridge on Linux or loopback elsewhere, never every interface
func (p IronProxy) defaultHTTPListen(tunnelPort int) []string {
	if runtime.GOOS == "linux" {
		if bridge := p.detectDockerBridgeIP(); bridge != "" && bridge != "127.0.0.1" {
			return []string{fmt.Sprintf("%s:%d", bridge, tunnelPort)}
		}
		p.log().Warn("No docker bridge (docker0) detected — binding iron-proxy to loopback only.  Docker sandboxes will NOT be able to reach the proxy until it is restarted with docker running.")
	}
	return []string{fmt.Sprintf("127.0.0.1:%d", tunnelPort)}
}

// docker0 IPv4 validated so a hostile ip shim cannot inject a bad bind address
func (p IronProxy) detectDockerBridgeIP() string {
	res := ironRun([]string{"ip", "-4", "-o", "addr", "show", "docker0"}, 2*time.Second, nil)
	if res.err != nil || res.code != 0 {
		return ""
	}
	candidate := ""
	for _, line := range strings.Split(res.stdout, "\n") {
		parts := strings.Fields(line)
		for i := 0; i+1 < len(parts); i++ {
			if parts[i] == "inet" {
				candidate = strings.SplitN(parts[i+1], "/", 2)[0]
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
	if addr.IsUnspecified() || addr.IsLoopback() || addr.IsMulticast() || addr.IsLinkLocalUnicast() || addr.IsGlobalUnicast() && !addr.IsPrivate() || isReservedV4(addr) {
		p.log().Warn(fmt.Sprintf("Refusing suspicious docker bridge IP %s reported by `ip`; skipping bridge bind.", candidate))
		return ""
	}
	return addr.String()
}

func isReservedV4(addr netip.Addr) bool {
	b := addr.As4()
	return b[0] >= 240
}

// IronProxyConfigOptions parameterize the generated proxy.yaml
type IronProxyConfigOptions struct {
	Mappings          []TokenMapping
	CACert            string
	CAKey             string
	TunnelPort        int
	AuditLog          string
	AllowedHosts      []string
	UpstreamDenyCIDRs []string
	DenyCIDRsSet      bool
	HTTPListen        []string
}

// BuildProxyConfig is the iron-proxy YAML document, real secrets come from the daemon's own env
func (p IronProxy) BuildProxyConfig(opts IronProxyConfigOptions) map[string]any {
	tunnelPort := opts.TunnelPort
	if tunnelPort == 0 {
		tunnelPort = DefaultIronTunnelPort
	}
	hosts := append([]string(nil), DefaultIronAllowedHosts...)
	if opts.AllowedHosts != nil {
		hosts = append([]string(nil), opts.AllowedHosts...)
	}
	for _, m := range opts.Mappings {
		for _, h := range m.UpstreamHosts {
			if !containsString(hosts, h) {
				hosts = append(hosts, h)
			}
		}
	}
	denyCIDRs := append([]string(nil), DefaultIronUpstreamDenyCIDRs...)
	if opts.DenyCIDRsSet {
		denyCIDRs = append([]string{}, opts.UpstreamDenyCIDRs...)
	}
	var secretRules []any
	for _, m := range opts.Mappings {
		headers := m.MatchHeaders
		if len(headers) == 0 {
			headers = []string{"Authorization"}
		}
		var rules []any
		for _, h := range m.UpstreamHosts {
			rules = append(rules, map[string]any{"host": h})
		}
		secretRules = append(secretRules, map[string]any{
			"source":  map[string]any{"type": "env", "var": m.RealEnvName},
			"replace": map[string]any{"proxy_value": m.ProxyToken, "match_headers": toAnyList(headers), "match_query": true, "match_body": false, "require": true},
			"rules":   rules,
		})
	}
	listens := opts.HTTPListen
	if len(listens) == 0 {
		listens = p.defaultHTTPListen(tunnelPort)
	}
	if len(listens) == 0 {
		listens = []string{fmt.Sprintf("127.0.0.1:%d", tunnelPort)}
	}
	primary := listens[0]
	bindHost := primary[:strings.LastIndex(primary, ":")]
	if bindHost == "" {
		bindHost = "127.0.0.1"
	}
	return map[string]any{
		"dns": map[string]any{"listen": "127.0.0.1:0", "proxy_ip": "127.0.0.1"},
		"proxy": map[string]any{
			"tunnel_listen": primary, "http_listen": fmt.Sprintf("%s:%d", bindHost, tunnelPort+1), "https_listen": "127.0.0.1:0",
			"max_request_body_bytes": 16 * 1024 * 1024, "max_response_body_bytes": 0,
			"upstream_response_header_timeout": "120s", "upstream_deny_cidrs": toAnyList(denyCIDRs),
		},
		"metrics":    map[string]any{"listen": "127.0.0.1:0"},
		"management": map[string]any{"listen": fmt.Sprintf("127.0.0.1:%d", tunnelPort+ironMgmtPortOffset), "api_key_env": IronProxyMgmtKeyEnv},
		"tls":        map[string]any{"ca_cert": opts.CACert, "ca_key": opts.CAKey, "cert_cache_size": 1000, "leaf_cert_expiry_hours": 168},
		"transforms": []any{
			map[string]any{"name": "allowlist", "config": map[string]any{"domains": toAnyList(hosts)}},
			map[string]any{"name": "secrets", "config": map[string]any{"secrets": secretRules}},
		},
		"log": map[string]any{"level": "info"},
	}
}

func toAnyList(items []string) []any {
	out := make([]any, 0, len(items))
	for _, s := range items {
		out = append(out, s)
	}
	return out
}

// Opens the path append only at 0600 refusing symlinks, the chmod failure fatal only when strict
func openPrivateAppend(path string, strictChmod bool) (*os.File, error) {
	f, err := openNoFollow(path, os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0o600)
	if err != nil {
		return nil, err
	}
	if err := f.Chmod(0o600); err != nil && strictChmod {
		f.Close()
		return nil, err
	}
	return f, nil
}

// EnsureAuditLog pre-creates the audit log 0600
func EnsureAuditLog(auditPath string) error {
	f, err := openPrivateAppend(auditPath, true)
	if err != nil {
		return fmt.Errorf("Refusing to start: could not pre-create audit log %s with restrictive permissions (%v).  Move or chmod any existing file at that path and retry.", auditPath, err)
	}
	return f.Close()
}

// WriteProxyConfig serializes the config to proxy.yaml under the home, written 0600 from creation
func (p IronProxy) WriteProxyConfig(cfg map[string]any) (string, error) {
	raw, err := yaml.Marshal(cfg)
	if err != nil {
		return "", err
	}
	path := filepath.Join(p.stateDir(), "proxy.yaml")
	if err := config.AtomicWrite(path, raw, 0o600); err != nil {
		return "", err
	}
	return path, nil
}

// WriteMappings persists sandbox visible tokens to mappings.json
func (p IronProxy) WriteMappings(mappings []TokenMapping) (string, error) {
	tokens := make([]any, 0, len(mappings))
	for _, m := range mappings {
		tokens = append(tokens, map[string]any{"proxy_token": m.ProxyToken, "env_name": m.RealEnvName, "upstream_hosts": toAnyList(m.UpstreamHosts),
			"match_headers": toAnyList(m.MatchHeaders), "alias_env_names": toAnyList(m.AliasEnvNames)})
	}
	raw, err := json.MarshalIndent(map[string]any{"version": 1, "tokens": tokens}, "", "  ")
	if err != nil {
		return "", err
	}
	path := filepath.Join(p.stateDir(), "mappings.json")
	if err := config.AtomicWrite(path, raw, 0o600); err != nil {
		return "", err
	}
	return path, nil
}

// LoadMappings reads mappings.json, empty on any error
func (p IronProxy) LoadMappings() []TokenMapping {
	path := filepath.Join(p.stateDir(), "mappings.json")
	raw, err := os.ReadFile(path)
	if err != nil {
		if !errors.Is(err, os.ErrNotExist) {
			p.log().Warn(fmt.Sprintf("Failed to read iron-proxy mappings.json: %v", err))
		}
		return nil
	}
	var payload map[string]any
	if err := json.Unmarshal(bytes.TrimPrefix(raw, []byte("\xef\xbb\xbf")), &payload); err != nil {
		p.log().Warn(fmt.Sprintf("Failed to read iron-proxy mappings.json: %v", err))
		return nil
	}
	items, _ := payload["tokens"].([]any)
	var out []TokenMapping
	for _, item := range items {
		m, ok := item.(map[string]any)
		if !ok {
			continue
		}
		token, tokenOK := m["proxy_token"].(string)
		envName, envOK := m["env_name"].(string)
		if !tokenOK || !envOK {
			continue
		}
		headers := anyStrings(m["match_headers"])
		if len(headers) == 0 {
			headers = []string{"Authorization"}
		}
		out = append(out, TokenMapping{ProxyToken: token, RealEnvName: envName, UpstreamHosts: anyStrings(m["upstream_hosts"]), MatchHeaders: headers, AliasEnvNames: anyStrings(m["alias_env_names"])})
	}
	return out
}

func anyStrings(v any) []string {
	list, ok := v.([]any)
	if !ok {
		return nil
	}
	var out []string
	for _, item := range list {
		if s, isStr := item.(string); isStr {
			out = append(out, s)
		}
	}
	return out
}

func envNamesPresent(available []string) map[string]bool {
	names := map[string]bool{}
	if available != nil {
		for _, n := range available {
			names[n] = true
		}
		return names
	}
	for _, kv := range os.Environ() {
		if k, v, ok := strings.Cut(kv, "="); ok && v != "" {
			names[k] = true
		}
	}
	return names
}

// DiscoverProviderMappings mints one mapping per known provider whose env var or alias is set
func DiscoverProviderMappings(availableEnvNames []string) []TokenMapping {
	names := envNamesPresent(availableEnvNames)
	var out []TokenMapping
	emit := func(envName string, hosts, headers, aliases []string) {
		present := names[envName]
		for _, a := range aliases {
			if names[a] {
				present = true
			}
		}
		if !present {
			return
		}
		prefix := strings.ReplaceAll(strings.ToLower(envName), "_api_key", "")
		out = append(out, TokenMapping{ProxyToken: MintProxyToken(prefix), RealEnvName: envName, UpstreamHosts: append([]string(nil), hosts...),
			MatchHeaders: append([]string(nil), headers...), AliasEnvNames: append([]string(nil), aliases...)})
	}
	for _, b := range ironBearerProviders {
		emit(b.envName, b.hosts, []string{"Authorization"}, nil)
	}
	for _, h := range ironHeaderAuthProviders {
		emit(h.envName, h.hosts, h.matchHeaders, h.aliases)
	}
	return out
}

// DiscoverUncoveredProviders lists recognized providers the proxy cannot swap
func DiscoverUncoveredProviders(availableEnvNames []string) []string {
	names := envNamesPresent(availableEnvNames)
	var out []string
	for _, n := range ironNonBearerProviders {
		if names[n] {
			out = append(out, n)
		}
	}
	return out
}

// MergeMappings keeps existing tokens and refreshes hosts from discovered, rotate re-mints every token
func MergeMappings(existing, discovered []TokenMapping, rotate bool) []TokenMapping {
	byName := map[string]TokenMapping{}
	if !rotate {
		for _, m := range existing {
			byName[m.RealEnvName] = m
		}
	}
	out := make([]TokenMapping, 0, len(discovered))
	for _, d := range discovered {
		if prev, ok := byName[d.RealEnvName]; ok {
			d.ProxyToken = prev.ProxyToken
		}
		out = append(out, d)
	}
	return out
}

func (p IronProxy) pidfile() string { return filepath.Join(p.stateDir(), "iron-proxy.pid") }

func (p IronProxy) readPID() int {
	raw, err := os.ReadFile(filepath.Join(p.stateDirRO(), "iron-proxy.pid"))
	if err != nil {
		return 0
	}
	pid, err := strconv.Atoi(strings.TrimSpace(strings.TrimPrefix(string(raw), "\xef\xbb\xbf")))
	if err != nil || pid <= 0 {
		return 0
	}
	return pid
}

// Process start time from the Linux stat file, empty elsewhere
func pidProcStartTime(pid int) string {
	raw, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
	if err != nil {
		return ""
	}
	text := string(raw)
	rparen := strings.LastIndex(text, ")")
	if rparen < 0 {
		return ""
	}
	fields := strings.Fields(text[rparen+1:])
	if len(fields) > 19 {
		return fields[19]
	}
	return ""
}

func (p IronProxy) persistedNoncePath() string {
	return filepath.Join(p.stateDirRO(), "iron-proxy.nonce")
}

// Nonce from disk, empty when missing unreadable or not owned by us
func (p IronProxy) readPersistedNonce() string {
	f, err := openNoFollow(p.persistedNoncePath(), os.O_RDONLY, 0)
	if err != nil {
		return ""
	}
	defer f.Close()
	if !fdOwnedByUs(f) {
		return ""
	}
	buf := make([]byte, ironNonceReadLimit)
	n, _ := f.Read(buf)
	return strings.TrimSpace(strings.ToValidUTF8(string(buf[:n]), ""))
}

func currentIronNonce() string {
	ironNonceMu.Lock()
	defer ironNonceMu.Unlock()
	return ironProxyNonce
}

// True only when the pid is alive and an iron-proxy process, defended against PID reuse
func (p IronProxy) pidAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	if !psPidExists(pid) {
		return false
	}
	var candidates []string
	for _, n := range []string{currentIronNonce(), p.readPersistedNonce()} {
		if n != "" && !containsString(candidates, n) {
			candidates = append(candidates, n)
		}
	}
	if len(candidates) > 0 {
		if envBytes, err := os.ReadFile(fmt.Sprintf("/proc/%d/environ", pid)); err == nil {
			for _, n := range candidates {
				if bytes.Contains(envBytes, []byte(IronProxyNonceEnv+"="+n)) {
					return true
				}
			}
		}
	}
	if fileExists("/proc/self/cmdline") {
		cmdline, err := os.ReadFile(fmt.Sprintf("/proc/%d/cmdline", pid))
		if err != nil {
			if errors.Is(err, os.ErrNotExist) || errors.Is(err, syscall.ESRCH) {
				return false
			}
		} else {
			argv0 := string(bytes.SplitN(cmdline, []byte{0}, 2)[0])
			return strings.HasPrefix(filepath.Base(argv0), "iron-proxy")
		}
	}
	res := ironRun([]string{"ps", "-p", fmt.Sprint(pid), "-o", "comm="}, 2*time.Second, nil)
	if res.err == nil && res.code == 0 {
		return strings.HasPrefix(filepath.Base(strings.TrimSpace(res.stdout)), "iron-proxy")
	}
	return true
}

// IronProxyStartOptions parameterize a daemon start
type IronProxyStartOptions struct {
	Binary                      string
	ConfigPath                  string
	ExtraEnv                    map[string]string
	InstallIfMissing            bool
	RefreshSecretsFromBitwarden bool
	BitwardenConfig             map[string]any
}

// StartProxy spawns iron-proxy as a managed background daemon, idempotent when already running
func (p IronProxy) StartProxy(opts IronProxyStartOptions) (IronProxyStatus, error) {
	if existing := p.readPID(); existing != 0 && p.pidAlive(existing) {
		return p.GetStatus(), nil
	}
	binPath := opts.Binary
	if binPath == "" {
		binPath = p.FindIronProxy(opts.InstallIfMissing)
	}
	if binPath == "" {
		return IronProxyStatus{}, errors.New("iron-proxy binary not available — run `icebath egress install`.")
	}
	cfg := opts.ConfigPath
	if cfg == "" {
		cfg = filepath.Join(p.stateDir(), "proxy.yaml")
	}
	if !fileExists(cfg) {
		return IronProxyStatus{}, fmt.Errorf("iron-proxy config not found at %s. Run `icebath egress setup` first.", cfg)
	}
	env, err := p.buildProxySubprocessEnv(opts.ExtraEnv, opts.RefreshSecretsFromBitwarden, opts.BitwardenConfig)
	if err != nil {
		return IronProxyStatus{}, err
	}
	if _, _, ok := p.readManagementListen(cfg); ok {
		token, err := p.EnsureManagementToken(false)
		if err != nil {
			return IronProxyStatus{}, err
		}
		env[IronProxyMgmtKeyEnv] = token
	}
	raw := make([]byte, 16)
	fillRandom(raw)
	sum := sha256.Sum256(raw)
	nonce := hex.EncodeToString(sum[:])
	ironNonceMu.Lock()
	ironProxyNonce = nonce
	ironNonceMu.Unlock()
	env[IronProxyNonceEnv] = nonce
	logPath := filepath.Join(p.stateDir(), "iron-proxy.log")
	cmd, err := p.spawnDaemon(binPath, cfg, env, logPath)
	if err != nil {
		return IronProxyStatus{}, err
	}
	pidfile := p.pidfile()
	if err := p.writePidfileSafely(pidfile, cmd.Process.Pid); err != nil {
		killAndWait(cmd, 2*time.Second)
		return IronProxyStatus{}, err
	}
	exited := make(chan error, 1)
	go func() { exited <- cmd.Wait() }()
	abort := func(msg string, kill bool) error {
		tail := tailLog(logPath, 20)
		if kill {
			killAndWait(cmd, 2*time.Second)
		}
		_ = os.Remove(pidfile)
		return fmt.Errorf("%sLast log lines:\n%s", msg, tail)
	}
	interrupts := make(chan os.Signal, 1)
	if runtime.GOOS != "windows" {
		signal.Notify(interrupts, syscall.SIGINT, syscall.SIGTERM)
		defer signal.Stop(interrupts)
	}
	probeHost, tunnelPort := p.probeTarget()
	deadline := time.Now().Add(ironStartupGrace)
	listening := false
	for {
		select {
		case <-exited:
			return IronProxyStatus{}, abort(fmt.Sprintf("iron-proxy exited immediately (code %d). ", cmd.ProcessState.ExitCode()), false)
		case <-interrupts:
			killAndWait(cmd, 2*time.Second)
			_ = os.Remove(pidfile)
			return IronProxyStatus{}, errors.New("interrupted while waiting for iron-proxy to start")
		default:
		}
		if portListening(probeHost, tunnelPort) {
			listening = true
			break
		}
		if time.Now().After(deadline) {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	select {
	case <-exited:
		return IronProxyStatus{}, abort(fmt.Sprintf("iron-proxy exited immediately (code %d). ", cmd.ProcessState.ExitCode()), false)
	default:
	}
	if !listening {
		return IronProxyStatus{}, abort(fmt.Sprintf("iron-proxy did not bind %s:%d within %.0fs.  Process was killed.  ", probeHost, tunnelPort, ironStartupGrace.Seconds()), true)
	}
	p.log().Info(fmt.Sprintf("Started iron-proxy pid=%d config=%s", cmd.Process.Pid, cfg))
	return p.GetStatus(), nil
}

// Spawns the daemon with output appended to the private log, our handle closes after the start
func (p IronProxy) spawnDaemon(binPath, cfg string, env map[string]string, logPath string) (*exec.Cmd, error) {
	logFile, err := openPrivateAppend(logPath, false)
	if err != nil {
		return nil, fmt.Errorf("Refusing to write iron-proxy log %s: %v.  Remove that path manually and retry.", logPath, err)
	}
	defer logFile.Close()
	owner, ours, err := fdOwner(logFile)
	if err != nil {
		return nil, err
	}
	if !ours {
		return nil, fmt.Errorf("iron-proxy log %s has unexpected owner uid=%s; refusing to write.", logPath, owner)
	}
	cmd := exec.Command(binPath, "-config", cfg)
	cmd.Env = envList(env)
	cmd.Stdin = nil
	cmd.Stdout, cmd.Stderr = logFile, logFile
	detachDaemon(cmd)
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("failed to spawn iron-proxy: %v", err)
	}
	return cmd, nil
}

// Writes the pidfile exclusively refusing symlinks, a stale leftover is replaced once
func (p IronProxy) writePidfileSafely(pidfile string, pid int) error {
	flags := os.O_WRONLY | os.O_CREATE | os.O_EXCL
	f, err := openNoFollow(pidfile, flags, 0o600)
	if errors.Is(err, os.ErrExist) {
		if existing := p.readPID(); existing != 0 && p.pidAlive(existing) {
			return fmt.Errorf("Another iron-proxy start appears to be in progress (pidfile %s -> pid %d).  Run `icebath egress stop` if that proxy is stuck.", pidfile, existing)
		}
		_ = os.Remove(pidfile)
		f, err = openNoFollow(pidfile, flags, 0o600)
	}
	if err != nil {
		return fmt.Errorf("Refusing to write pidfile %s: %v.  Remove that path manually and retry.", pidfile, err)
	}
	owner, ours, err := fdOwner(f)
	if err != nil {
		f.Close()
		return err
	}
	if !ours {
		f.Close()
		return fmt.Errorf("pidfile %s has unexpected owner uid=%s", pidfile, owner)
	}
	_, err = f.WriteString(fmt.Sprint(pid))
	f.Close()
	if err != nil {
		return err
	}
	if nonce := currentIronNonce(); nonce != "" {
		_ = writePrivateFile(strings.TrimSuffix(pidfile, filepath.Ext(pidfile))+".nonce", []byte(nonce))
	}
	return nil
}

func killAndWait(cmd *exec.Cmd, grace time.Duration) {
	if cmd.Process == nil {
		return
	}
	if err := terminateChild(cmd.Process); err != nil {
		return
	}
	done := make(chan struct{})
	go func() {
		_, _ = cmd.Process.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(grace):
		_ = cmd.Process.Kill()
		select {
		case <-done:
		case <-time.After(grace):
		}
	}
}

// Allowlisted infra vars plus the secrets named in mappings, with the proxy recursion vars stripped
func (p IronProxy) buildProxySubprocessEnv(extraEnv map[string]string, refreshFromBitwarden bool, bitwardenConfig map[string]any) (map[string]string, error) {
	env := IronAllowlistedEnv()
	mappings := p.LoadMappings()
	needed := map[string]bool{}
	aliasSources := map[string][]string{}
	for _, m := range mappings {
		needed[m.RealEnvName] = true
		if len(m.AliasEnvNames) > 0 {
			aliasSources[m.RealEnvName] = m.AliasEnvNames
		}
	}
	for name := range needed {
		if v, ok := os.LookupEnv(name); ok {
			env[name] = v
			continue
		}
		for _, a := range aliasSources[name] {
			if v := os.Getenv(a); v != "" {
				env[name] = v
				break
			}
		}
	}
	if refreshFromBitwarden && len(bitwardenConfig) > 0 {
		if err := p.refreshSecretsFromBitwarden(env, needed, bitwardenConfig, truthyAny(bitwardenConfig["allow_env_fallback"])); err != nil {
			return nil, err
		}
	}
	for k, v := range extraEnv {
		env[k] = v
	}
	for _, n := range ironSubprocessEnvStrip {
		delete(env, n)
	}
	if _, ok := env["NO_COLOR"]; !ok {
		env["NO_COLOR"] = "1"
	}
	return env, nil
}

// Errors unless the operator opted into the legacy host env fallback, then logs the warning
func (p IronProxy) bitwardenShortfall(allowEnvFallback bool, errText, warning string) error {
	if !allowEnvFallback {
		return errors.New(errText)
	}
	p.log().Warn(warning)
	return nil
}

// Overwrites the needed names with fresh uncached BWS values, only mapped names are injected
func (p IronProxy) refreshSecretsFromBitwarden(env map[string]string, needed map[string]bool, bitwardenConfig map[string]any, allowEnvFallback bool) error {
	tokenEnv := anyString(bitwardenConfig["access_token_env"])
	if tokenEnv == "" {
		tokenEnv = "BWS_ACCESS_TOKEN"
	}
	accessToken := strings.TrimSpace(os.Getenv(tokenEnv))
	projectID := anyString(bitwardenConfig["project_id"])
	if accessToken == "" || projectID == "" {
		return p.bitwardenShortfall(allowEnvFallback,
			"credential_source=bitwarden but the access-token env or project_id is empty.  Either set both, switch to credential_source: env, or set `proxy.allow_env_fallback: true` to opt into the legacy fallback behaviour.",
			"credential_source=bitwarden but access-token env or project_id is empty — proxy will fall back to parent env (allow_env_fallback=true).")
	}
	secrets, warnings, err := FetchBitwardenSecrets(BitwardenFetchOptions{AccessToken: accessToken, ProjectID: projectID, CacheTTL: 0, UseCache: false})
	if err != nil {
		return err
	}
	var missing []string
	for name := range needed {
		if v, ok := secrets[name]; ok {
			env[name] = v
		} else {
			missing = append(missing, name)
		}
	}
	sort.Strings(missing)
	if len(missing) > 0 {
		if err := p.bitwardenShortfall(allowEnvFallback,
			fmt.Sprintf("Bitwarden refresh did not return secrets for %v.  Either add the secrets to your BWS project, switch to credential_source: env via `icebath egress setup --no-bitwarden`, or set `proxy.allow_env_fallback: true` in config.yaml to opt into the legacy host-env fallback.", missing),
			fmt.Sprintf("Bitwarden refresh did not return secrets for %v — falling back to host env for those names (allow_env_fallback=true).", missing)); err != nil {
			return err
		}
	}
	if len(warnings) > 0 {
		p.log().Warn(fmt.Sprintf("Bitwarden refresh produced %d warning(s); run `icebath secrets bitwarden status` for detail.", len(warnings)))
	}
	return nil
}

func (p IronProxy) forgetDaemon() {
	_ = os.Remove(p.pidfile())
	_ = os.Remove(p.persistedNoncePath())
	ironNonceMu.Lock()
	ironProxyNonce = ""
	ironNonceMu.Unlock()
}

// StopProxy terminates the daemon, true when it was running
func (p IronProxy) StopProxy() bool {
	pid := p.readPID()
	if pid == 0 || !p.pidAlive(pid) {
		p.forgetDaemon()
		return false
	}
	startBefore := pidProcStartTime(pid)
	if err := psSignal(pid, false); err != nil {
		p.forgetDaemon()
		return false
	}
	deadline := time.Now().Add(5 * time.Second)
	exited := false
	for time.Now().Before(deadline) {
		if !p.pidAlive(pid) {
			exited = true
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	if !exited {
		startAfter := pidProcStartTime(pid)
		if (startBefore != "" && startAfter != "" && startBefore != startAfter) || !p.pidAlive(pid) {
			p.log().Warn(fmt.Sprintf("iron-proxy pid=%d appears recycled before SIGKILL; not killing.", pid))
		} else {
			_ = psSignal(pid, true)
		}
	}
	p.forgetDaemon()
	p.log().Info(fmt.Sprintf("Stopped iron-proxy pid=%d", pid))
	return true
}

// GetStatus snapshots the proxy state without side effects
func (p IronProxy) GetStatus() IronProxyStatus {
	status := IronProxyStatus{}
	probeHost, port := p.probeTarget()
	status.TunnelPort = port
	if binary := p.FindIronProxy(false); binary != "" {
		status.BinaryPath, status.BinaryVersion = binary, p.IronProxyVersion(binary)
	}
	state := p.stateDirRO()
	if cfg := filepath.Join(state, "proxy.yaml"); fileExists(cfg) {
		status.ConfigPath = cfg
	}
	if ca := filepath.Join(state, "ca.crt"); fileExists(ca) {
		status.CACertPath = ca
	}
	if pid := p.readPID(); pid != 0 && p.pidAlive(pid) {
		status.PID, status.Listening = pid, portListening(probeHost, port)
	}
	return status
}

func portListening(host string, port int) bool {
	conn, err := net.DialTimeout("tcp", net.JoinHostPort(host, fmt.Sprint(port)), 500*time.Millisecond)
	if err != nil {
		return false
	}
	conn.Close()
	return true
}

func tailLog(path string, lines int) string {
	raw, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return "(no log file)"
		}
		return fmt.Sprintf("(could not read log: %v)", err)
	}
	if len(raw) > 8192 {
		raw = raw[len(raw)-8192:]
	}
	all := strings.Split(strings.ToValidUTF8(string(raw), "�"), "\n")
	if len(all) > 0 && all[len(all)-1] == "" {
		all = all[:len(all)-1]
	}
	if len(all) > lines {
		all = all[len(all)-lines:]
	}
	return strings.Join(all, "\n")
}

// ResetIronProxyForTests clears the version cache and the start nonce
func ResetIronProxyForTests() {
	ironVersionMu.Lock()
	ironVersionCache = map[string]string{}
	ironVersionMu.Unlock()
	ironNonceMu.Lock()
	ironProxyNonce = ""
	ironNonceMu.Unlock()
}

func ironErrText(res ironRunResult) string {
	if strings.TrimSpace(res.stderr) != "" {
		return res.stderr
	}
	return fmt.Sprint(res.err)
}
