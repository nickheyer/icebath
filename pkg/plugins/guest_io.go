package plugins

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/nickheyer/icebath/pkg/tools"
)

const (
	guestHTTPDefaultTimeout = 30 * time.Second
	guestHTTPMaxBody        = 64 << 20
)

// Directories a grant or request path may name by variable
type guestPathVars struct {
	home       string
	pluginDir  string
	pluginData string
}

// Files, hosts and environment variables a manifest lets its guest reach through the host
type guestGrants struct {
	vars  guestPathVars
	read  []string
	write []string
	net   []string
	env   map[string]bool
}

// Grants of a manifest with every path root expanded and resolved
func parseGuestGrants(raw map[string]any, vars guestPathVars) guestGrants {
	g := guestGrants{vars: vars, env: map[string]bool{}}
	roots := func(v any) []string {
		var out []string
		for _, item := range stringList(v) {
			if p, err := vars.resolve(item); err == nil {
				out = append(out, p)
			}
		}
		return out
	}
	g.read, g.write = roots(raw["read"]), roots(raw["write"])
	for _, host := range stringList(raw["net"]) {
		if host = strings.ToLower(strings.TrimSpace(host)); host != "" {
			g.net = append(g.net, host)
		}
	}
	for _, name := range stringList(raw["env"]) {
		g.env[strings.TrimSpace(name)] = true
	}
	return g
}

// Absolute path of a grant or request path with its variable expanded and its symbolic links resolved
func (v guestPathVars) resolve(p string) (string, error) {
	p = strings.TrimSpace(p)
	for prefix, dir := range map[string]string{"$ICEBATH_HOME": v.home, "$PLUGIN_DIR": v.pluginDir, "$PLUGIN_DATA": v.pluginData} {
		if p == prefix || strings.HasPrefix(p, prefix+"/") {
			p = dir + strings.TrimPrefix(p, prefix)
		}
	}
	if p == "~" || strings.HasPrefix(p, "~/") {
		userHome, err := os.UserHomeDir()
		if err != nil {
			return "", err
		}
		p = userHome + strings.TrimPrefix(p, "~")
	}
	if !filepath.IsAbs(p) {
		return "", fmt.Errorf("path %q must be absolute or start with ~, $ICEBATH_HOME, $PLUGIN_DIR or $PLUGIN_DATA", p)
	}
	return resolveExisting(filepath.Clean(p)), nil
}

// Path with the symbolic links of its longest existing ancestor resolved
func resolveExisting(p string) string {
	rest := ""
	for cur := p; ; {
		if real, err := filepath.EvalSymlinks(cur); err == nil {
			return filepath.Join(real, rest)
		}
		parent := filepath.Dir(cur)
		if parent == cur {
			return p
		}
		rest = filepath.Join(filepath.Base(cur), rest)
		cur = parent
	}
}

// Resolved request path when a root of the grant covers it
func (g guestGrants) path(p string, write bool) (string, error) {
	resolved, err := g.vars.resolve(p)
	if err != nil {
		return "", err
	}
	roots := g.write
	if !write {
		roots = append(append([]string{}, g.read...), g.write...)
	}
	for _, root := range roots {
		if resolved == root || strings.HasPrefix(resolved, strings.TrimSuffix(root, string(os.PathSeparator))+string(os.PathSeparator)) {
			return resolved, nil
		}
	}
	kind := "read"
	if write {
		kind = "write"
	}
	return "", &PluginHostUnsupported{Msg: fmt.Sprintf("the plugin manifest grants no %s access to %s", kind, p)}
}

// Reports whether the grant names the host of a URL, by exact name or by a *.suffix pattern
func (g guestGrants) host(name string) bool {
	name = strings.ToLower(name)
	for _, pattern := range g.net {
		if pattern == name || pattern == "*" {
			return true
		}
		if suffix, ok := strings.CutPrefix(pattern, "*."); ok && (name == suffix || strings.HasSuffix(name, "."+suffix)) {
			return true
		}
	}
	return false
}

// Serves one file, network or environment request of a guest under the grants of its manifest
func (h *PluginHost) serveIO(ctx context.Context, params map[string]any) (any, error) {
	if _, err := h.pluginCtx(params); err != nil {
		return nil, err
	}
	h.mu.Lock()
	spec := h.guest
	h.mu.Unlock()
	if spec == nil {
		return nil, &PluginHostUnsupported{Msg: "granted input and output serve WebAssembly and program plugins"}
	}
	grants := spec.grants
	switch op := wireStr(params["op"]); op {
	case "read_file":
		p, err := grants.path(wireStr(params["path"]), false)
		if err != nil {
			return nil, err
		}
		data, err := os.ReadFile(p)
		if err != nil {
			return nil, err
		}
		return map[string]any{"data": data}, nil
	case "write_file":
		p, err := grants.path(wireStr(params["path"]), true)
		if err != nil {
			return nil, err
		}
		data, err := wireDecodeBytes(asMap(params["data"])["__bytes__"])
		if err != nil {
			return nil, err
		}
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			return nil, err
		}
		return map[string]any{}, os.WriteFile(p, data.([]byte), 0o644)
	case "list_dir":
		p, err := grants.path(wireStr(params["path"]), false)
		if err != nil {
			return nil, err
		}
		entries, err := os.ReadDir(p)
		if err != nil {
			return nil, err
		}
		out := make([]any, 0, len(entries))
		for _, e := range entries {
			var size int64
			if info, err := e.Info(); err == nil && !e.IsDir() {
				size = info.Size()
			}
			out = append(out, map[string]any{"name": e.Name(), "dir": e.IsDir(), "size": size})
		}
		return map[string]any{"entries": out}, nil
	case "remove":
		p, err := grants.path(wireStr(params["path"]), true)
		if err != nil {
			return nil, err
		}
		return map[string]any{}, os.Remove(p)
	case "env":
		name := wireStr(params["name"])
		if !grants.env[name] {
			return nil, &PluginHostUnsupported{Msg: fmt.Sprintf("the plugin manifest grants no access to the environment variable %s", name)}
		}
		env, err := tools.ServedProfileChildEnv(h.baseCtx, nil, nil, h.home, true)
		if err != nil {
			return nil, err
		}
		for k, v := range h.envOverride {
			env[k] = v
		}
		value, ok := env[name]
		return map[string]any{"value": value, "ok": ok}, nil
	case "http":
		if h.networkDenied() {
			return nil, doctorDenyNetwork()
		}
		return guestHTTP(ctx, grants, params)
	default:
		return nil, &HostBuiltinError{Type: "ValueError", Message: fmt.Sprintf("unknown plugin io operation %q", op)}
	}
}

// Performs one HTTP request for a guest, every hop checked against the granted hosts
func guestHTTP(ctx context.Context, grants guestGrants, params map[string]any) (any, error) {
	target, err := url.Parse(wireStr(params["url"]))
	if err != nil {
		return nil, err
	}
	allowed := func(u *url.URL) error {
		if u.Scheme != "http" && u.Scheme != "https" {
			return &PluginHostUnsupported{Msg: fmt.Sprintf("plugin HTTP requests take http and https URLs, not %s", u.Scheme)}
		}
		if !grants.host(u.Hostname()) {
			return &PluginHostUnsupported{Msg: fmt.Sprintf("the plugin manifest grants no network access to %s", u.Hostname())}
		}
		return nil
	}
	if err := allowed(target); err != nil {
		return nil, err
	}
	var body io.Reader
	if raw, ok := asMap(params["body"])["__bytes__"]; ok {
		data, err := wireDecodeBytes(raw)
		if err != nil {
			return nil, err
		}
		body = bytes.NewReader(data.([]byte))
	}
	method := strings.ToUpper(wireStr(params["method"]))
	if method == "" {
		method = http.MethodGet
	}
	timeout := guestHTTPDefaultTimeout
	if seconds := guestNumber(params["timeout"], 0); seconds > 0 {
		timeout = time.Duration(seconds * float64(time.Second))
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, method, target.String(), body)
	if err != nil {
		return nil, err
	}
	for name, value := range asMap(params["headers"]) {
		req.Header.Set(name, wireStr(value))
	}
	client := &http.Client{CheckRedirect: func(next *http.Request, via []*http.Request) error {
		if len(via) >= 10 {
			return errors.New("stopped after 10 redirects")
		}
		return allowed(next.URL)
	}}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, guestHTTPMaxBody+1))
	if err != nil {
		return nil, err
	}
	if len(data) > guestHTTPMaxBody {
		return nil, fmt.Errorf("the response of %s is larger than %d bytes", target.Host, guestHTTPMaxBody)
	}
	headers := map[string]any{}
	for name, values := range resp.Header {
		headers[name] = strings.Join(values, ", ")
	}
	return map[string]any{"status": int64(resp.StatusCode), "headers": headers, "body": data}, nil
}
