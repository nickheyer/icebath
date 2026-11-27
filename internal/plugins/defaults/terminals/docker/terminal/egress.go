package terminal

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strconv"
	"strings"

	"github.com/nickheyer/icebath/internal/terminal"
	"github.com/nickheyer/icebath/pkg/fsx"
)

const (
	// Where a container finds the egress proxy's certificate authority
	containerCA = "/etc/ssl/certs/icebath-egress-ca.crt"
	// Stands for the NODE_OPTIONS flag the proxy needs, put into NODE_OPTIONS once merged
	nodeOptionsAppend = "_ICEBATH_EGRESS_NODE_OPTIONS_APPEND"
)

// Variables that route a sandbox's traffic and trust, which the proxy sets
var proxyControl = []string{"HTTPS_PROXY", "https_proxy", "HTTP_PROXY", "http_proxy", "NO_PROXY", "no_proxy", "REQUESTS_CA_BUNDLE", "SSL_CERT_FILE",
	"CURL_CA_BUNDLE", "NODE_EXTRA_CA_CERTS"}

// NODE_OPTIONS flags choosing Node's certificate store, of which one holds
var caModes = []string{"--use-openssl-ca", "--use-bundled-ca"}

// What the egress proxy puts into a container
type egress struct {
	// Mount of the certificate authority, variables pointing traffic and trust at the proxy and swapping keys for tokens, and the host
	// entry reaching the proxy
	volumes   []string
	overrides map[string]string
	hosts     []string
	// Provider keys the proxy swaps
	keys []string
	// A sandbox refuses to start while the proxy is not ready, and its settings may not undo the proxy
	enforce bool
}

// What the proxy puts into the profile's containers; nothing when it is off or, not enforced, not ready
func egressOf(proxy terminal.Proxy) (egress, error) {
	if !proxy.Enabled {
		return egress{enforce: true}, nil
	}
	degraded := func(text string) (egress, error) {
		if proxy.Enforce {
			return egress{}, errors.New(text)
		}
		slog.Warn(text + " — continuing without proxy (enforce_on_docker=false).")
		return egress{}, nil
	}
	switch {
	case !proxy.Configured:
		return degraded("The sandbox proxy is on but not set up. Run `icebath egress setup` to make its tokens and write proxy.yaml.")
	case !proxy.Running:
		return degraded(fmt.Sprintf("The sandbox proxy is on but not running on port %d. Start it with `icebath egress start`.", proxy.Port))
	case proxy.CA == "" || !fsx.IsFile(proxy.CA):
		return degraded(fmt.Sprintf("The CA certificate of the sandbox proxy is missing from %s. Run `icebath egress setup` again to make a new one.", proxy.CA))
	case len(proxy.Tokens) == 0:
		return degraded("The sandbox proxy is set up, but its mappings.json is empty or damaged. Run `icebath egress setup` again to make provider " +
			"tokens before a sandbox starts.")
	}
	secure, plain := fmt.Sprintf("http://host.docker.internal:%d", proxy.Port), fmt.Sprintf("http://host.docker.internal:%d", proxy.Port+1)
	overrides := map[string]string{
		"HTTPS_PROXY": secure, "https_proxy": secure, "HTTP_PROXY": plain, "http_proxy": plain,
		"NO_PROXY": "127.0.0.1,localhost,::1", "no_proxy": "127.0.0.1,localhost,::1",
		"REQUESTS_CA_BUNDLE": containerCA, "SSL_CERT_FILE": containerCA, "CURL_CA_BUNDLE": containerCA, "NODE_EXTRA_CA_CERTS": containerCA,
		"ICEBATH_EGRESS_PROXY": "1", nodeOptionsAppend: "--use-openssl-ca",
	}
	var keys []string
	for _, token := range proxy.Tokens {
		overrides[token.Name] = token.Token
		overrides["ICEBATH_PROXY_TOKEN_"+token.Name] = token.Token
		for _, alias := range token.Aliases {
			overrides[alias] = token.Token
		}
		keys = append(keys, token.Name)
	}
	return egress{volumes: []string{"-v", proxy.CA + ":" + containerCA + ":ro"}, overrides: overrides, hosts: []string{"--add-host", "host.docker.internal:host-gateway"},
		keys: keys, enforce: proxy.Enforce}, nil
}

// Label value telling containers made with the proxy, and with which of its setups, from those made without it
func (e egress) label() string {
	if len(e.volumes) == 0 && len(e.overrides) == 0 && len(e.hosts) == 0 {
		return "off"
	}
	payload, _ := json.Marshal(map[string]any{"env_overrides": e.overrides, "host_args": e.hosts, "volume_args": e.volumes})
	sum := sha256.Sum256(payload)
	return hex.EncodeToString(sum[:])[:24]
}

// Names the proxy depends on, upper cased: its own controls, NODE_OPTIONS and every variable it sets
func (e egress) critical() map[string]bool {
	out := map[string]bool{"NODE_OPTIONS": true}
	for _, name := range proxyControl {
		out[strings.ToUpper(name)] = true
	}
	for name := range e.overrides {
		out[strings.ToUpper(name)] = true
	}
	return out
}

// A collision of the settings with the proxy: refused when it is enforced, warned of otherwise
func collision(text string, enforce bool, remedy, consequence string) error {
	if enforce {
		return errors.New(text + " " + remedy)
	}
	slog.Warn(text+" Required for Docker is off under Sandbox proxy. "+consequence, "enforce_on_docker", false)
	return nil
}

// Refuses forwarded variables that would hand the sandbox the real values the proxy keeps out
func (e egress) checkForward(forward []string) error {
	critical := e.critical()
	var hit []string
	for _, name := range forward {
		if critical[strings.ToUpper(name)] {
			hit = append(hit, name)
		}
	}
	if len(hit) == 0 {
		return nil
	}
	slices.Sort(hit)
	return collision(fmt.Sprintf("Passed-in variables in /settings under Terminal › Docker would hand the sandbox the real values of %s, which the "+
		"sandbox proxy keeps out.", listOf(hit)), e.enforce,
		"Remove them from Passed-in variables, or turn off Required for Docker under Sandbox proxy to let the sandbox have them.",
		"The values passed in win over the tokens of the proxy.")
}

// Refuses variables of the settings that would undo what the proxy sets
func (e egress) checkEnv(env map[string]string) error {
	keys := map[string]bool{}
	for _, name := range e.keys {
		keys[name] = true
	}
	collides := func(name string) bool {
		value, set := env[name]
		if !set {
			return false
		}
		override, proxied := e.overrides[name]
		if keys[name] {
			return !proxied || value != override
		}
		return proxied && value != override
	}
	var hit []string
	for _, name := range proxyControl {
		if collides(name) {
			hit = append(hit, name)
		}
	}
	for name := range keys {
		if !slices.Contains(proxyControl, name) && collides(name) {
			hit = append(hit, name)
		}
	}
	if len(hit) == 0 {
		return nil
	}
	slices.Sort(hit)
	return collision(fmt.Sprintf("Variables in /settings under Terminal › Docker set %s, which the sandbox proxy sets itself.", listOf(hit)), e.enforce,
		"Remove them from Variables, or turn off Required for Docker under Sandbox proxy to let your values win.",
		"Your values win, so sandbox traffic does not go through the proxy.")
}

// Shorthand flags of docker run taking no value, which may lead a cluster ending in e
const boolShorthands = "ditPq"

// Kind of an extra flag that sets variables, env or env-file, with the value it carries inline
func envFlag(arg string) (kind string, inline *string, ok bool) {
	if strings.HasPrefix(arg, "--") {
		flag, value, hasValue := strings.Cut(arg, "=")
		if flag != "--env" && flag != "--env-file" {
			return "", nil, false
		}
		if hasValue {
			return flag[2:], &value, true
		}
		return flag[2:], nil, true
	}
	if !strings.HasPrefix(arg, "-") {
		return "", nil, false
	}
	for i, ch := range arg[1:] {
		if strings.ContainsRune(boolShorthands, ch) {
			continue
		}
		if ch != 'e' {
			return "", nil, false
		}
		rest := strings.TrimPrefix(arg[i+2:], "=")
		if rest == "" {
			return "env", nil, true
		}
		return "env", &rest, true
	}
	return "", nil, false
}

// Refuses extra flags that set what the proxy depends on or pick another network
func (e egress) checkExtra(extra []string) error {
	critical := e.critical()
	seen := map[string]bool{}
	for i := 0; i < len(extra); {
		arg := extra[i]
		if arg == "--" {
			break
		}
		if kind, inline, ok := envFlag(arg); ok {
			if kind == "env-file" {
				seen["--env-file"] = true
			} else {
				value := ""
				if inline != nil {
					value = *inline
				} else if i+1 < len(extra) {
					value = extra[i+1]
				}
				if name, _, _ := strings.Cut(value, "="); critical[strings.ToUpper(name)] {
					seen[name] = true
				}
			}
			if inline != nil {
				i++
			} else {
				i += 2
			}
			continue
		}
		if flag, _, _ := strings.Cut(arg, "="); flag == "--network" || flag == "--net" {
			seen[arg] = true
		}
		i++
	}
	if len(seen) == 0 {
		return nil
	}
	hit := make([]string, 0, len(seen))
	for name := range seen {
		hit = append(hit, name)
	}
	slices.Sort(hit)
	return collision(fmt.Sprintf("Extra flags in /settings under Terminal › Docker set %s, which the sandbox proxy controls.", listOf(hit)), e.enforce,
		"Remove those flags, or turn off Required for Docker under Sandbox proxy to let them through.",
		"The extra flags may send sandbox traffic around the proxy.")
}

// Variables of the settings merged with the proxy's, the proxy's winning when it is enforced, and the NODE_OPTIONS flag it needs put in
func (e egress) merge(env map[string]string) map[string]string {
	merged := map[string]string{}
	first, second := e.overrides, env
	if e.enforce && len(e.overrides) > 0 {
		first, second = env, e.overrides
	}
	for name, value := range first {
		merged[name] = value
	}
	for name, value := range second {
		merged[name] = value
	}
	required, ok := merged[nodeOptionsAppend]
	delete(merged, nodeOptionsAppend)
	if required = strings.TrimSpace(required); !ok || required == "" {
		return merged
	}
	tokens := strings.Fields(merged["NODE_OPTIONS"])
	if slices.Contains(caModes, required) {
		var dropped, kept []string
		for _, token := range tokens {
			if slices.Contains(caModes, token) && token != required {
				dropped = append(dropped, token)
				continue
			}
			kept = append(kept, token)
		}
		if len(dropped) > 0 {
			slog.Warn("Overriding conflicting NODE_OPTIONS CA-mode flag(s) with the egress-required flag to keep Node routed through the egress CA store.",
				"dropped", listOf(dropped), "required", required)
		}
		tokens = kept
	}
	if !slices.Contains(tokens, required) {
		tokens = append(tokens, required)
	}
	if joined := strings.Join(tokens, " "); joined != "" {
		merged["NODE_OPTIONS"] = joined
	} else {
		delete(merged, "NODE_OPTIONS")
	}
	return merged
}

// A list of names as messages show it, each quoted, the last after "and"
func listOf(items []string) string {
	quoted := make([]string, len(items))
	for i, item := range items {
		quoted[i] = strconv.Quote(item)
	}
	if len(quoted) < 2 {
		return strings.Join(quoted, "")
	}
	return strings.Join(quoted[:len(quoted)-1], ", ") + " and " + quoted[len(quoted)-1]
}
