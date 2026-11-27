package models

import (
	"context"
	"maps"
	"net/netip"
	"net/url"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/nickheyer/icebath/internal/config"
	"github.com/nickheyer/icebath/internal/provider"
)

const (
	// How long a kind of server found at an address is trusted, and the finding that none answered
	serverTTL     = time.Hour
	serverMissTTL = 5 * time.Minute
	// How long probes skip a host after one timed out connecting
	blackholeTTL = 30 * time.Second
	// How long another process trusts what a local server said, and this one a window it reported
	probeDiskTTL = 5 * time.Minute
	probeTTL     = 30 * time.Second
)

const probeCache = "local_endpoint_probes.json"

var tailscale = netip.MustParsePrefix("100.64.0.0/10")

// Whether the address is on this machine or its network: loopback, a container's or a .local name, a bare host name, a private, link local
// or Tailscale address
func Local(base string) bool {
	parsed := parse(base, "http")
	if parsed == nil {
		return false
	}
	host := strings.ToLower(parsed.Hostname())
	switch {
	case host == "localhost" || host == "0.0.0.0" || host == "::1" || strings.HasSuffix(host, ".local"):
		return true
	case host != "" && !strings.Contains(host, ".") && !strings.Contains(host, ":"):
		return true
	case strings.HasSuffix(host, ".docker.internal") || strings.HasSuffix(host, ".containers.internal") || strings.HasSuffix(host, ".lima.internal"):
		return true
	}
	if addr, err := netip.ParseAddr(host); err == nil {
		return addr.IsPrivate() || addr.IsLoopback() || addr.IsLinkLocalUnicast() || addr.Is4() && tailscale.Contains(addr)
	}
	return false
}

// Address with a scheme put in front when it has none, nil when empty or malformed
func parse(base, scheme string) *url.URL {
	normalized := strings.TrimRight(strings.TrimSpace(base), "/")
	if normalized == "" {
		return nil
	}
	if !strings.Contains(normalized, "://") {
		normalized = scheme + "://" + normalized
	}
	parsed, err := url.Parse(normalized)
	if err != nil {
		return nil
	}
	return parsed
}

var localhost = regexp.MustCompile(`^(https?://)localhost([:/]|$)`)

// Address with localhost spelled as the IPv4 loopback, which skips the wait for IPv6 on hosts that have both
func ipv4(address string) string {
	return localhost.ReplaceAllString(address, "${1}127.0.0.1${2}")
}

// Root of a local server, without its v1 path
func serverRoot(base string) string {
	return strings.TrimSuffix(ipv4(strings.TrimRight(base, "/")), "/v1")
}

// Root of an LM Studio server for its own API, its /api/v1, /api or /v1 taken off
func LMStudioRoot(base string) string {
	root := strings.TrimRight(strings.TrimSpace(base), "/")
	for _, suffix := range []string{"/api/v1", "/api", "/v1"} {
		if strings.HasSuffix(root, suffix) {
			return strings.TrimRight(strings.TrimSuffix(root, suffix), "/")
		}
	}
	return root
}

// Host and port of an address, so every probe of one server counts as one
func hostKey(base string) string {
	parsed := parse(base, "http")
	if parsed == nil || parsed.Hostname() == "" {
		return ""
	}
	port := parsed.Port()
	if port == "" {
		port = "80"
		if parsed.Scheme == "https" {
			port = "443"
		}
	}
	return parsed.Hostname() + ":" + port
}

func (s *state) blackhole(base string) {
	if key := hostKey(base); key != "" {
		s.mu.Lock()
		s.blackholed[key] = time.Now()
		s.mu.Unlock()
	}
}

// Whether a probe of the host timed out connecting a moment ago
func (s *state) blackholedHost(base string) bool {
	key := hostKey(base)
	s.mu.Lock()
	defer s.mu.Unlock()
	seen, ok := s.blackholed[key]
	if ok && time.Since(seen) >= blackholeTTL {
		delete(s.blackholed, key)
		return false
	}
	return ok
}

// What a local server said lately, as another process may have heard it
func (s *state) probeDisk(kind, key string) any {
	entry, ok := readJSON(s.file(probeCache))[kind+":"+key].(map[string]any)
	if !ok {
		return nil
	}
	if ts, ok := entry["ts"].(float64); !ok || float64(time.Now().Unix())-ts >= probeDiskTTL.Seconds() {
		return nil
	}
	return entry["value"]
}

func (s *state) saveProbe(kind, key string, value any) {
	s.mu.Lock()
	defer s.mu.Unlock()
	path := s.file(probeCache)
	now := float64(time.Now().UnixNano()) / 1e9
	data := map[string]any{}
	for name, raw := range readJSON(path) {
		if entry, ok := raw.(map[string]any); ok {
			if ts, ok := entry["ts"].(float64); ok && now-ts < probeDiskTTL.Seconds() {
				data[name] = entry
			}
		}
	}
	data[kind+":"+key] = map[string]any{"value": value, "ts": now}
	writeJSON(path, data)
}

type verdict struct {
	kind string
	at   time.Time
}

// Kind of local server at the address, found by the endpoints only it answers: lm-studio, ollama, llamacpp or vllm; empty for a known
// provider's host or when none answers
func (m *Models) ServerType(ctx context.Context, cfg *config.Config, base, key string) string {
	normalized := ipv4(strings.TrimRight(strings.TrimSpace(base), "/"))
	if kind := m.Kind(normalized); kind != "" && kind != "ollama-cloud" {
		return ""
	}
	s := m.of(cfg)
	root := serverRoot(normalized)
	s.mu.Lock()
	held, ok := s.servers[root]
	s.mu.Unlock()
	if ok && (held.kind != "" && time.Since(held.at) < serverTTL || held.kind == "" && time.Since(held.at) < serverMissTTL) {
		return held.kind
	}
	if s.blackholedHost(root) {
		return ""
	}
	if found, ok := s.probeDisk("server_type", root).(string); ok {
		s.mu.Lock()
		s.servers[root] = verdict{found, time.Now()}
		s.mu.Unlock()
		return found
	}
	legs := []struct {
		kind    string
		targets []string
		check   func(*answer) bool
	}{
		{"lm-studio", []string{LMStudioRoot(normalized) + "/api/v1/models"}, func(*answer) bool { return true }},
		{"ollama", []string{root + "/api/tags"}, func(a *answer) bool { _, ok := a.object()["models"]; return ok }},
		{"llamacpp", []string{root + "/v1/props", root + "/props"}, func(a *answer) bool { return strings.Contains(string(a.body), "default_generation_settings") }},
		{"vllm", []string{root + "/version"}, func(a *answer) bool { _, ok := a.object()["version"]; return ok }},
	}
	found := ""
probing:
	for _, leg := range legs {
		var last *answer
		for _, target := range leg.targets {
			reply, err := send(ctx, "GET", target, bearer(key), nil, 2*time.Second, false)
			if err != nil {
				if connectTimeout(err) {
					s.blackhole(root)
					break probing
				}
				last = nil
				break
			}
			if last = reply; reply.status == 200 {
				break
			}
		}
		if last != nil && last.status == 200 && leg.check(last) {
			found = leg.kind
			break
		}
	}
	s.mu.Lock()
	s.servers[root] = verdict{found, time.Now()}
	s.mu.Unlock()
	if found != "" {
		s.saveProbe("server_type", root, found)
	}
	return found
}

type probeKey struct{ kind, model, base string }

type probeHit struct {
	window int
	at     time.Time
}

// Window a probe found, kept half a minute when it found one
func (s *state) memoProbe(key probeKey, probe func() int) int {
	s.mu.Lock()
	hit, ok := s.probes[key]
	s.mu.Unlock()
	if ok && time.Since(hit.at) < probeTTL {
		return hit.window
	}
	n := probe()
	if n > 0 {
		s.mu.Lock()
		s.probes[key] = probeHit{n, time.Now()}
		s.mu.Unlock()
	}
	return n
}

// What an Ollama server's show answer says of the model, nil when it doesn't answer
func show(ctx context.Context, root, key, model string, timeout time.Duration) (map[string]any, error) {
	reply, err := send(ctx, "POST", root+"/api/show", bearer(key), []byte(`{"name":`+strconv.Quote(model)+`}`), timeout, false)
	if err != nil || reply.status != 200 {
		return nil, err
	}
	return reply.object(), nil
}

// Window an Ollama show answer gives: the num_ctx it runs with, or the most its weights were trained for, the latter first when asked;
// the first at least the minimum
func showWindow(data map[string]any, trainedFirst bool, minimum int) (int, bool) {
	var running, trained []int
	parameters, _ := data["parameters"].(string)
	for _, line := range strings.Split(parameters, "\n") {
		fields := strings.Fields(line)
		if strings.Contains(line, "num_ctx") && len(fields) >= 2 {
			if n, err := strconv.Atoi(fields[len(fields)-1]); err == nil {
				running = append(running, n)
			}
		}
	}
	info, _ := data["model_info"].(map[string]any)
	for _, key := range slices.Sorted(maps.Keys(info)) {
		if n, ok := info[key].(float64); ok && strings.Contains(key, "context_length") {
			trained = append(trained, int(n))
		}
	}
	order := [][]int{running, trained}
	if trainedFirst {
		order = [][]int{trained, running}
	}
	for _, values := range order {
		for _, n := range values {
			if minimum <= 0 || n >= minimum {
				return n, true
			}
		}
	}
	return 0, false
}

// Window any server answering Ollama's show for the model reports, from its weights first as a hosted one runs them
func (m *Models) ollamaShow(ctx context.Context, cfg *config.Config, model, base, key string) int {
	s := m.of(cfg)
	return s.memoProbe(probeKey{"ollama_show", bareModel(m, model), strings.TrimRight(base, "/")}, func() int {
		root := serverRoot(base)
		if s.blackholedHost(root) {
			return 0
		}
		data, err := show(ctx, root, key, model, 5*time.Second)
		if err != nil && connectTimeout(err) {
			s.blackhole(root)
		}
		n, _ := showWindow(data, true, 1024)
		return n
	})
}

// Window an Ollama server on this machine runs the model with, its Modelfile's num_ctx before its weights' most; false when the server
// is no Ollama or doesn't answer
func (m *Models) OllamaWindow(ctx context.Context, cfg *config.Config, model, base, key string) (int, bool) {
	bare, root := bareModel(m, model), serverRoot(base)
	if m.ServerType(ctx, cfg, base, key) != "ollama" {
		return 0, false
	}
	s := m.of(cfg)
	if hit, ok := s.probeDisk("ollama_num_ctx", root+"|"+bare).(float64); ok && hit > 0 {
		return int(hit), true
	}
	data, _ := show(ctx, root, key, bare, 3*time.Second)
	if data == nil {
		return 0, false
	}
	n, ok := showWindow(data, false, 0)
	if ok {
		s.saveProbe("ollama_num_ctx", root+"|"+bare, n)
	}
	return n, ok
}

// Whether a server answering Ollama's show reports thinking for the model, and whether it said: a model it lists without thinking is
// known not to think, and the answer is kept half a minute either way
func (m *Models) OllamaThinks(ctx context.Context, cfg *config.Config, model, base, key string) (bool, bool) {
	bare := strings.TrimSuffix(strings.TrimSuffix(strings.TrimSpace(model), ":cloud"), "-cloud")
	root := serverRoot(base)
	if root == "" || bare == "" {
		return false, false
	}
	// 2 for a model that thinks, 1 for one that doesn't, 0 when the server said nothing
	answer := m.of(cfg).memoProbe(probeKey{"ollama_thinking", bare, root}, func() int {
		data, _ := show(ctx, root, key, bare, 5*time.Second)
		capabilities, ok := data["capabilities"].([]any)
		if !ok {
			return 0
		}
		for _, capability := range capabilities {
			if text(capability) == "thinking" {
				return 2
			}
		}
		return 1
	})
	return answer == 2, answer > 0
}

// Whether an Ollama server's model sees images, and whether the server said: its capabilities, else a vision block in its weights
func (m *Models) OllamaVision(ctx context.Context, cfg *config.Config, route provider.Route) (bool, bool) {
	bare := bareModel(m, route.Model)
	if bare == "" || route.BaseURL == "" || m.ServerType(ctx, cfg, route.BaseURL, route.APIKey) != "ollama" {
		return false, false
	}
	data, _ := show(ctx, serverRoot(route.BaseURL), route.APIKey, bare, 3*time.Second)
	if data == nil {
		return false, false
	}
	if capabilities, ok := data["capabilities"].([]any); ok && len(capabilities) > 0 {
		for _, capability := range capabilities {
			if strings.EqualFold(text(capability), "vision") {
				return true, true
			}
		}
		return false, true
	}
	info, _ := data["model_info"].(map[string]any)
	for key := range info {
		if strings.Contains(strings.ToLower(key), "vision.block_count") {
			return true, true
		}
	}
	return false, false
}

// Window a server on this machine runs the model with, asked the way its kind answers and then the ways any OpenAI style one does; kept
// half a minute
func (m *Models) localWindow(ctx context.Context, cfg *config.Config, model, base, key string) int {
	s := m.of(cfg)
	return s.memoProbe(probeKey{"", bareModel(m, model), strings.TrimRight(base, "/")}, func() int {
		model := bareModel(m, model)
		root := serverRoot(base)
		if s.blackholedHost(root) {
			return 0
		}
		kind := m.ServerType(ctx, cfg, base, key)
		get := func(target string) (*answer, error) {
			return send(ctx, "GET", target, bearer(key), nil, 3*time.Second, false)
		}
		var probes []func() (int, bool, error)
		switch kind {
		case "ollama":
			probes = append(probes, func() (int, bool, error) {
				data, err := show(ctx, root, key, model, 3*time.Second)
				n, ok := showWindow(data, false, 0)
				return n, ok, err
			})
		case "lm-studio":
			probes = append(probes, func() (int, bool, error) {
				reply, err := get(ipv4(LMStudioRoot(base)) + "/api/v1/models")
				if err != nil || reply.status != 200 {
					return 0, false, err
				}
				listed, _ := reply.object()["models"].([]any)
				for _, item := range listed {
					entry, _ := item.(map[string]any)
					if names(text(entry["key"]), model) || names(text(entry["id"]), model) {
						n := loadedWindow(entry)
						return n, n > 0, nil
					}
				}
				return 0, false, nil
			})
		case "llamacpp":
			probes = append(probes, func() (int, bool, error) { return llamacppWindow(get, root, model) })
		}
		probes = append(probes, func() (int, bool, error) {
			if kind == "" {
				return 0, false, nil
			}
			reply, err := get(root + "/v1/models/" + model)
			if err != nil || reply.status != 200 {
				return 0, false, err
			}
			n, ok := payloadWindow(reply.object())
			return n, ok, nil
		}, func() (int, bool, error) {
			reply, err := get(root + "/v1/models")
			if err != nil || reply.status != 200 {
				return 0, false, err
			}
			listed, _ := reply.object()["data"].([]any)
			var matched map[string]any
			for _, item := range listed {
				if entry, ok := item.(map[string]any); ok && names(text(entry["id"]), model) {
					matched = entry
					break
				}
			}
			if matched == nil && len(listed) == 1 {
				matched, _ = listed[0].(map[string]any)
			}
			if matched == nil {
				return 0, false, nil
			}
			sources := []map[string]any{matched}
			if meta, ok := matched["meta"].(map[string]any); ok {
				sources = append(sources, meta)
			}
			for _, source := range sources {
				if n := positive(source["n_ctx"]); n > 0 {
					return n, true, nil
				}
			}
			for _, source := range sources {
				if n, ok := payloadWindow(source); ok {
					return n, true, nil
				}
			}
			return 0, false, nil
		})
		for _, probe := range probes {
			n, ok, err := probe()
			if err != nil {
				if connectTimeout(err) {
					s.blackhole(root)
				}
				return 0
			}
			if ok {
				return n
			}
		}
		return 0
	})
}

// Window llama.cpp allocated, from its props for the model or as a whole, else the --ctx-size its router launched the model with
func llamacppWindow(get func(string) (*answer, error), root, model string) (int, bool, error) {
	for _, path := range []string{"/props?model=" + url.QueryEscape(model) + "&autoload=false", "/props"} {
		reply, err := get(root + path)
		if err != nil {
			return 0, false, nil
		}
		if reply.status != 200 {
			continue
		}
		settings, _ := reply.object()["default_generation_settings"].(map[string]any)
		if n := positive(settings["n_ctx"]); n > 0 {
			return n, true, nil
		}
	}
	reply, err := get(root + "/models")
	if err != nil || reply.status != 200 {
		return 0, false, nil
	}
	data, _ := reply.object()["data"].([]any)
	for _, item := range data {
		entry, _ := item.(map[string]any)
		if !names(text(entry["id"]), model) {
			continue
		}
		status, _ := entry["status"].(map[string]any)
		args := texts(status["args"])
		for i := 0; i+1 < len(args); i++ {
			if args[i] == "--ctx-size" || args[i] == "-c" {
				if n, err := strconv.Atoi(args[i+1]); err == nil && n > 0 {
					return n, true, nil
				}
			}
		}
	}
	return 0, false, nil
}

// Whether a listed id is the model: the same, or a publisher's slug ending in it
func names(id, model string) bool {
	return id == model || strings.Contains(id, "/") && id[strings.LastIndex(id, "/")+1:] == model
}

// Tags Ollama puts after a colon, which a provider's name before one is not followed by
var ollamaTag = regexp.MustCompile(`(?i)^(\d+\.?\d*b|latest|stable|q\d|fp?\d|instruct|chat|coder|vision|text)`)

// Model without a provider's name put before it with a colon, an Ollama tag after one kept
func bareModel(m *Models, model string) string {
	if !strings.Contains(model, ":") || strings.HasPrefix(model, "http") {
		return model
	}
	prefix, suffix, _ := strings.Cut(model, ":")
	if _, known := m.providers.Spec(prefix); known && !ollamaTag.MatchString(strings.TrimSpace(suffix)) {
		return suffix
	}
	return model
}

// Kind of provider an address belongs to: the kind claiming its host, else the kind whose base URL has that host; empty for none
func (m *Models) Kind(base string) string {
	parsed := parse(base, "https")
	if parsed == nil {
		return ""
	}
	host := strings.ToLower(cmpHost(parsed))
	specs := m.providers.Specs()
	for _, spec := range specs {
		for _, claimed := range spec.Hostnames {
			if claimed = strings.ToLower(claimed); host == claimed || strings.HasSuffix(host, "."+claimed) {
				return spec.Name
			}
		}
		for _, part := range spec.MetadataHostnames {
			if strings.Contains(host, strings.ToLower(part)) {
				return spec.Name
			}
		}
	}
	for _, spec := range specs {
		if own := provider.BaseHost(spec.BaseURL); own != "" && !Local(spec.BaseURL) && strings.Contains(host, own) {
			return spec.Name
		}
	}
	return ""
}

func cmpHost(parsed *url.URL) string {
	if parsed.Host != "" {
		return parsed.Hostname()
	}
	return parsed.Path
}
