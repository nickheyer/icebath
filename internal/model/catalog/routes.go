package catalog

import (
	"context"
	"errors"
	"net/netip"
	"slices"
	"strconv"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/nickheyer/icebath/internal/config"
	"github.com/nickheyer/icebath/internal/model/providers"
	"github.com/nickheyer/icebath/internal/model/wire"
)

const (
	answerInReasoningCapability = "answer_in_reasoning"
	nemotronParserMarker        = "nemotron-3.5-lightning"
)

var errMalformedURL = errors.New("catalog: the URL authority is malformed")

type splitURL struct {
	scheme    string
	netloc    string
	path      string
	query     string
	hasNetloc bool
}

func schemeText(text string) bool {
	if text == "" || !((text[0] >= 'a' && text[0] <= 'z') || (text[0] >= 'A' && text[0] <= 'Z')) {
		return false
	}
	for i := 0; i < len(text); i++ {
		b := text[i]
		if !((b >= 'a' && b <= 'z') || (b >= 'A' && b <= 'Z') || (b >= '0' && b <= '9') || b == '+' || b == '-' || b == '.') {
			return false
		}
	}
	return true
}

func splitRawURL(raw string) splitURL {
	var s splitURL
	rest := raw
	if i := strings.IndexByte(rest, ':'); i > 0 && schemeText(rest[:i]) {
		s.scheme, rest = strings.ToLower(rest[:i]), rest[i+1:]
	}
	if strings.HasPrefix(rest, "//") {
		rest = rest[2:]
		end := strings.IndexAny(rest, "/?#")
		if end < 0 {
			end = len(rest)
		}
		s.netloc, rest, s.hasNetloc = rest[:end], rest[end:], true
	}
	if i := strings.IndexByte(rest, '#'); i >= 0 {
		rest = rest[:i]
	}
	if i := strings.IndexByte(rest, '?'); i >= 0 {
		s.query, rest = rest[i+1:], rest[:i]
	}
	s.path = rest
	return s
}

type authority struct {
	host    string
	port    int
	hasPort bool
}

func parseAuthority(netloc string) (authority, error) {
	var a authority
	if strings.Contains(netloc, "[") != strings.Contains(netloc, "]") {
		return a, errMalformedURL
	}
	hostinfo := netloc[strings.LastIndex(netloc, "@")+1:]
	host, port := "", ""
	if _, bracketed, ok := strings.Cut(hostinfo, "["); ok {
		inner, after, _ := strings.Cut(bracketed, "]")
		if !strings.HasPrefix(strings.ToLower(inner), "v") {
			if _, err := netip.ParseAddr(inner); err != nil {
				return a, errMalformedURL
			}
		}
		host = inner
		_, port, _ = strings.Cut(after, ":")
	} else {
		host, port, _ = strings.Cut(hostinfo, ":")
	}
	if address, zone, ok := strings.Cut(host, "%"); ok {
		a.host = strings.ToLower(address) + "%" + zone
	} else {
		a.host = strings.ToLower(host)
	}
	if port == "" {
		return a, nil
	}
	for i := 0; i < len(port); i++ {
		if port[i] < '0' || port[i] > '9' {
			return a, errMalformedURL
		}
	}
	n, err := strconv.Atoi(port)
	if err != nil || n > 65535 {
		return a, errMalformedURL
	}
	a.port, a.hasPort = n, true
	return a, nil
}

func shownURL(baseURL string) string {
	s := splitRawURL(baseURL)
	i := strings.LastIndex(s.netloc, "@")
	if !s.hasNetloc || i < 0 {
		return baseURL
	}
	return strings.Replace(baseURL, s.netloc[:i+1], "", 1)
}

func malformedURL(raw string) bool {
	s := splitRawURL(raw)
	if !s.hasNetloc {
		return false
	}
	_, err := parseAuthority(s.netloc)
	return err != nil
}

func NormalizeRouteBaseURL(raw string) string {
	if raw == "" {
		return ""
	}
	for _, r := range raw {
		if r <= 0x20 {
			return raw
		}
	}
	beforeFragment, _, _ := strings.Cut(raw, "#")
	hadQuery := strings.Contains(beforeFragment, "?")
	s := splitRawURL(raw)
	a, err := parseAuthority(s.netloc)
	if err != nil || s.scheme == "" || a.host == "" {
		return raw
	}
	host := a.host
	routeHost := s.netloc[strings.LastIndex(s.netloc, "@")+1:]
	if strings.HasPrefix(routeHost, "[") || strings.Contains(host, ":") {
		host = "[" + host + "]"
	}
	if a.hasPort && !(s.scheme == "http" && a.port == 80) && !(s.scheme == "https" && a.port == 443) {
		host += ":" + strconv.Itoa(a.port)
	}
	if i := strings.LastIndex(s.netloc, "@"); i >= 0 {
		host = s.netloc[:i] + "@" + host
	}
	path := s.path
	if strings.HasSuffix(path, "/") && !hadQuery {
		path = path[:len(path)-1]
	}
	if path != "" && !strings.HasPrefix(path, "/") {
		path = "/" + path
	}
	normalized := s.scheme + "://" + host + path
	if s.query != "" {
		normalized += "?" + s.query
	} else if hadQuery {
		normalized += "?"
	}
	return normalized
}

func (c *Catalog) customEndpoints(ctx context.Context, frozen []config.CustomEndpoint) []config.CustomEndpoint {
	if frozen != nil {
		return frozen
	}
	loaded, err := c.cfg.CustomEndpoints(ctx)
	if err != nil {
		return nil
	}
	return loaded
}

func (c *Catalog) routeEntries(ctx context.Context, baseURL string, frozen []config.CustomEndpoint) []config.CustomEndpoint {
	entries := c.customEndpoints(ctx, frozen)
	if baseURL == "" {
		return nil
	}
	target := NormalizeRouteBaseURL(baseURL)
	if target == "" {
		return nil
	}
	var out []config.CustomEndpoint
	for _, entry := range entries {
		if candidate := NormalizeRouteBaseURL(entry.Text("base_url")); candidate != "" && candidate == target {
			out = append(out, entry)
		}
	}
	return out
}

func (c *Catalog) RouteEntries(ctx context.Context, baseURL string, frozen []config.CustomEndpoint) []config.CustomEndpoint {
	return c.routeEntries(c.scoped(ctx), baseURL, frozen)
}

func (c *Catalog) customAPIMode(ctx context.Context, baseURL string, frozen []config.CustomEndpoint) string {
	for _, entry := range c.routeEntries(ctx, baseURL, frozen) {
		if mode := entry.APIMode(); mode != "" {
			return mode
		}
	}
	return ""
}

func (c *Catalog) customContextLength(ctx context.Context, model, baseURL string, frozen []config.CustomEndpoint) (int, bool) {
	if model == "" || baseURL == "" {
		return 0, false
	}
	entries := c.routeEntries(ctx, baseURL, frozen)
	for _, entry := range entries {
		if window, ok := entry.ModelContextLength(model); ok {
			return window, true
		}
	}
	for _, entry := range entries {
		if window, ok := entry.ContextLength(); ok {
			return window, true
		}
	}
	return 0, false
}

func entryCapability(entry config.CustomEndpoint, name string) (value, set bool) {
	caps := mappingChild(entry.Node(), "capabilities")
	if caps == nil || caps.Kind != yaml.MappingNode {
		return false, false
	}
	flag := mappingChild(caps, name)
	if flag == nil || flag.Kind != yaml.ScalarNode || flag.ShortTag() != "!!bool" {
		return false, false
	}
	return strings.EqualFold(flag.Value, "true"), true
}

type LiveRoute struct {
	Provider  string
	Requested string
	BaseURL   string
	Model     string
	Mode      wire.Mode
	Custom    []config.CustomEndpoint
}

func (c *Catalog) AnswerInReasoning(ctx context.Context, r LiveRoute) bool {
	ctx = c.scoped(ctx)
	provider := strings.ToLower(strings.TrimSpace(r.Provider))
	if provider == "openrouter" || providers.HostMatches(r.BaseURL, "openrouter.ai") {
		return false
	}
	if mode := strings.ToLower(strings.TrimSpace(string(r.Mode))); mode != "" && mode != string(wire.ModeChat) {
		return false
	}
	entries := c.routeEntries(ctx, r.BaseURL, r.Custom)
	if r.Model != "" && r.BaseURL != "" {
		for _, entry := range entries {
			if value, set := entry.ModelCapability(r.Model, answerInReasoningCapability); set {
				return value
			}
		}
	}
	requested := strings.ToLower(strings.TrimSpace(r.Requested))
	var ids []string
	for _, p := range []string{provider, requested} {
		if strings.HasPrefix(p, "custom:") {
			ids = append(ids, CustomProviderSlug(p, ""))
		}
	}
	var live []config.CustomEndpoint
	for _, entry := range entries {
		if slices.Contains(ids, CustomProviderSlug(entry.Text("name"), entry.Text("provider_key"))) {
			live = append(live, entry)
		}
	}
	if len(live) == 0 {
		live = entries
	}
	sawTrue, sawFalse := false, false
	for _, entry := range live {
		if value, set := entryCapability(entry, answerInReasoningCapability); set {
			sawTrue, sawFalse = sawTrue || value, sawFalse || !value
		}
	}
	if sawTrue || sawFalse {
		return sawTrue && !sawFalse
	}
	if !strings.Contains(strings.ToLower(r.Model), nemotronParserMarker) {
		return false
	}
	return IsLocalEndpoint(r.BaseURL)
}
