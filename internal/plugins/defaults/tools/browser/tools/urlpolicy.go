package tools

import (
	"context"
	"log/slog"
	"net"
	"net/netip"
	"net/url"
	"regexp"
	"strings"
	"time"

	"github.com/dlclark/regexp2/v2"

	"github.com/nickheyer/icebath/internal/config"
	terminalconfig "github.com/nickheyer/icebath/internal/terminal/config"
	"github.com/nickheyer/icebath/internal/web"
)

var privateSuffixes = []string{".localhost", ".local", ".lan", ".internal"}

var cgnat = netip.MustParsePrefix("100.64.0.0/10")

// Body of a URL literal in code, up to the quote, bracket or space that ends it
const urlBody = "(?=[^\\s'\"`)\\]<>])(?:[^\\s'\"`)\\[\\]<>]*\\[[^\\s'\"`)\\[\\]<>]*\\])?[^\\s'\"`)\\]<>]*"

var (
	jsURLLiteral   = regexp2.MustCompile(`https?://`+urlBody, regexp2.IgnoreCase)
	urlOrRelative  = regexp2.MustCompile(`(?:https?://|(?<![\w:/\\])[/\\]{2,}(?=[^/\\\s]))`+urlBody, regexp2.IgnoreCase)
	urlBodyAt      = regexp2.MustCompile(`\G`+urlBody, regexp2.None)
	userinfoTailAt = regexp2.MustCompile("\\G[^\\s'\"`/?#\\\\]*@", regexp2.None)
	literalHead    = regexp.MustCompile(`^(?i:(?:https?:)?[/\\]+)`)
)

// Error result of a refused URL, the keys given after it
func policyError(message string, extra map[string]any) map[string]any {
	out := map[string]any{"success": false, "error": message}
	for k, v := range extra {
		out[k] = v
	}
	return out
}

// Whether an address host is private, loopback, link local or carrier NAT space, one a local DNS proxy answers with excepted; false
// when the host is no address
func privateIP(cfg *config.Config, host string) (private, ok bool) {
	ip, err := netip.ParseAddr(strings.Trim(host, "[]"))
	if err != nil {
		return false, false
	}
	ip = ip.Unmap()
	if web.FakeIP(cfg, ip) {
		return false, true
	}
	return ip.IsPrivate() || ip.IsLoopback() || ip.IsLinkLocalUnicast() || ip.Is4() && cgnat.Contains(ip), true
}

// Whether the URL's host is or resolves to a private, LAN, loopback or carrier NAT address; a name that does not resolve is not private
func urlPrivate(cfg *config.Config, raw string) bool {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil {
		slog.Debug("URL-privacy check failed", "url", raw, "error", err)
		return false
	}
	hostname := strings.TrimRight(strings.ToLower(u.Hostname()), ".")
	if hostname == "" {
		return false
	}
	if private, ok := privateIP(cfg, hostname); ok {
		return private
	}
	if hostname == "localhost" {
		return true
	}
	for _, suffix := range privateSuffixes {
		if strings.HasSuffix(hostname, suffix) {
			return true
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	addresses, err := net.DefaultResolver.LookupHost(ctx, hostname)
	if err != nil {
		return false
	}
	for _, address := range addresses {
		if private, _ := privateIP(cfg, address); private {
			return true
		}
	}
	return false
}

// Whether the browser and the terminal both run on this machine: no attached endpoint, no cloud service, a local terminal backend
func (r *registrant) localBackend(cfg *config.Config) bool {
	if r.overrideRaw(cfg) != "" {
		return false
	}
	if cloud, err := r.browser.Cloud(cfg); err != nil || cloud != nil {
		return false
	}
	backend := strings.ToLower(strings.TrimSpace(cfg.GetString(terminalconfig.Backend)))
	return backend == "local" || backend == ""
}

// Whether a private URL goes to the local browser beside the cloud one: a cloud service chosen, private routing on, no endpoint attached
func (r *registrant) routesLocally(cfg *config.Config, raw string) (bool, error) {
	if r.overrideRaw(cfg) != "" {
		return false, nil
	}
	cloud, err := r.browser.Cloud(cfg)
	if err != nil {
		return false, err
	}
	return cloud != nil && localForPrivate(cfg) && urlPrivate(cfg, raw), nil
}

// Refusal of a URL that carries an API key or token, plain or percent decoded
func secretURL(raw string) map[string]any {
	decoded, err := url.PathUnescape(raw)
	if err != nil {
		decoded = raw
	}
	if web.CarriesSecret(raw) || web.CarriesSecret(decoded) {
		return policyError("Blocked: URL contains what appears to be an API key or token. Secrets must not be sent in URLs.", nil)
	}
	return nil
}

// The cloud metadata floor, then the private address floor outside a local backend, then the website policy
func (r *registrant) policyError(cfg *config.Config, raw string, local bool) map[string]any {
	if web.AlwaysBlocked(raw) {
		return policyError("Blocked: URL targets a cloud metadata endpoint", nil)
	}
	if !r.localBackend(cfg) && !local && !privateURLs(cfg) && !web.SafeFor(cfg, raw, false) {
		return policyError("Blocked: URL targets a private or internal address", nil)
	}
	if block := web.Blocked(cfg, raw); block != nil {
		return policyError(block.Message, map[string]any{"blocked_by_policy": map[string]any{"host": block.Host, "rule": block.Rule, "source": block.Source}})
	}
	return nil
}

// Refusal of a URL the browser may not open, nil when it may; local relaxes only the private address floor
func (r *registrant) urlSafety(cfg *config.Config, raw string, local bool) map[string]any {
	refused := secretURL(raw)
	if refused == nil {
		raw = web.NormalizeURL(raw)
		refused = secretURL(raw)
	}
	if refused != nil {
		return refused
	}
	return r.policyError(cfg, raw, local)
}

// Whether a page could reach private networks the profile keeps it from: a backend off this machine without private addresses allowed
func (r *registrant) guardActive(cfg *config.Config) bool {
	return !r.localBackend(cfg) && !privateURLs(cfg)
}

// Whether the URL hits the metadata floor or fails the private address guard
func blocked(cfg *config.Config, raw string) bool {
	return web.AlwaysBlocked(raw) || !web.SafeFor(cfg, raw, privateURLs(cfg))
}

// Every http or https literal of the text, scheme relative ones too when asked, and the host read past a userinfo cut
func urlLiterals(text string, relative bool) []string {
	re := jsURLLiteral
	if relative {
		re = urlOrRelative
	}
	runes := []rune(text)
	var found []string
	m, _ := re.FindRunesMatch(runes)
	for m != nil {
		lit := m.String()
		found = append(found, lit)
		head := literalHead.FindString(lit)
		end := m.RuneIndex + m.RuneLength
		if !strings.ContainsAny(lit[len(head):], `/?#\`) {
			if tail, _ := userinfoTailAt.FindRunesMatchStartingAt(runes, end); tail != nil {
				if host, _ := urlBodyAt.FindRunesMatchStartingAt(runes, tail.RuneIndex+tail.RuneLength); host != nil {
					found = append(found, head+host.String())
				}
			}
		}
		m, _ = re.FindNextMatch(m)
	}
	out := make([]string, len(found))
	for i, lit := range found {
		out[i] = strings.TrimRight(lit, ".,;")
	}
	return out
}

// Whether a literal reaches cloud metadata, a scheme relative one judged by its literal host without DNS
func metadataLiteral(literal string) bool {
	if !strings.HasPrefix(literal, "/") && !strings.HasPrefix(literal, `\`) {
		return web.AlwaysBlocked(literal)
	}
	return web.MetadataLiteral(literal)
}

// First http or https literal of a script that blocked refuses, empty for none
func scriptTargets(expression string, refused func(string) bool) string {
	for _, candidate := range urlLiterals(expression, false) {
		if refused(candidate) {
			return candidate
		}
	}
	return ""
}
