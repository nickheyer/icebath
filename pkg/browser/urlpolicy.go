package browser

import (
	"context"
	"net"
	"net/netip"
	"regexp"
	"strings"
	"time"

	"github.com/dlclark/regexp2/v2"
	"github.com/nickheyer/icebath/pkg/internal/pyipaddress"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pyurl"
	"github.com/nickheyer/icebath/pkg/tools"
)

var privateHostSuffixes = []string{".localhost", ".local", ".lan", ".internal"}

var cgnatPrefix = netip.MustParsePrefix("100.64.0.0/10")

const urlBody = "(?=[^\\s'\"`)\\]<>])(?:[^\\s'\"`)\\[\\]<>]*\\[[^\\s'\"`)\\[\\]<>]*\\])?[^\\s'\"`)\\]<>]*"

var (
	jsURLLiteralRE          = regexp2.MustCompile(`https?://`+urlBody, regexp2.IgnoreCase)
	urlOrSchemeRelativeRE   = regexp2.MustCompile(`(?:https?://|(?<![\w:/\\])[/\\]{2,}(?=[^/\\\s]))`+urlBody, regexp2.IgnoreCase)
	urlBodyAtRE             = regexp2.MustCompile(`\G`+urlBody, regexp2.None)
	userinfoTailAtRE        = regexp2.MustCompile("\\G[^\\s'\"`/?#\\\\]*@", regexp2.None)
	literalHeadRE           = regexp.MustCompile(`^(?i:(?:https?:)?[/\\]+)`)
	numericHostRE           = regexp.MustCompile(`^[0-9a-fx.]+$`)
	urlPrivacyLookupTimeout = 10 * time.Second
)

// Seams the policy reads through so a caller can pin backend facts, the SSRF guard and the website policy
var (
	policyIsLocalBackend     = func(env *tools.Env) bool { return isLocalBackend(env) }
	policyAllowPrivateURLs   = func(env *tools.Env) bool { return cfgOf(env).allowPrivateURLs() }
	policyIsSafeURL          = tools.IsSafeURL
	policyCheckWebsiteAccess = func(env *tools.Env, rawURL string) *tools.WebsiteBlock {
		block, _ := tools.CheckWebsiteAccess(env, rawURL, "")
		return block
	}
	policyLookupHost = func(host string) ([]string, error) {
		ctx, cancel := context.WithTimeout(context.Background(), urlPrivacyLookupTimeout)
		defer cancel()
		return net.DefaultResolver.LookupHost(ctx, host)
	}
)

func policyErr(msg string, extra ...any) *pyjson.Object {
	out := pyjson.NewObject()
	out.Set("success", false)
	out.Set("error", msg)
	for i := 0; i+1 < len(extra); i += 2 {
		out.Set(extra[i].(string), extra[i+1])
	}
	return out
}

// Private classification of an IP literal host, ok false when the host is not an IP literal
func privateIP(env *tools.Env, host string) (private, ok bool) {
	ip, ok := pyipaddress.IPAddress(host)
	if !ok {
		return false, false
	}
	if tools.IsDeclaredFakeIP(env, ip) {
		return false, true
	}
	return pyipaddress.IsPrivate(ip) || pyipaddress.IsLoopback(ip) || pyipaddress.IsLinkLocal(ip) || (ip.Is4() && cgnatPrefix.Contains(ip)), true
}

// True when the host is or resolves to a private, LAN, loopback or CGNAT address, DNS failure is not private
func urlIsPrivate(env *tools.Env, rawURL string) bool {
	parsed, err := pyurl.Parse(rawURL)
	if err != nil {
		cfgOf(env).logger().Debug("URL-privacy check failed", "url", rawURL, "error", err)
		return false
	}
	hostname := strings.TrimRight(strings.ToLower(strings.TrimSpace(parsed.Hostname())), ".")
	if hostname == "" {
		return false
	}
	if private, ok := privateIP(env, hostname); ok {
		return private
	}
	if hostname == "localhost" {
		return true
	}
	for _, suffix := range privateHostSuffixes {
		if strings.HasSuffix(hostname, suffix) {
			return true
		}
	}
	addrs, err := policyLookupHost(hostname)
	if err != nil {
		return false
	}
	for _, addr := range addrs {
		if private, _ := privateIP(env, addr); private {
			return true
		}
	}
	return false
}

// True when a cloud provider is configured, private auto local routing is on, no CDP override exists and the url is private
func hybridRoutesLocally(env *tools.Env, rawURL string) (bool, error) {
	s := cfgOf(env)
	if s.cdpOverrideRaw() != "" {
		return false, nil
	}
	provider, err := getCloudProvider(env)
	if err != nil {
		return false, err
	}
	return provider != nil && s.autoLocalForPrivateURLs() && urlIsPrivate(env, rawURL), nil
}

// Refuses urls embedding an API key or token in raw or percent decoded form
func secretURLError(rawURL string) *pyjson.Object {
	if tools.RedactPrefixSearch(rawURL) || tools.RedactPrefixSearch(pyurl.Unquote(rawURL)) {
		return policyErr("Blocked: URL contains what appears to be an API key or token. Secrets must not be sent in URLs.")
	}
	return nil
}

// Metadata floor, then the private address floor for non local backends, then the website policy
func urlPolicyError(env *tools.Env, rawURL string, autoLocal bool) *pyjson.Object {
	if tools.IsAlwaysBlockedURL(env, rawURL) {
		return policyErr("Blocked: URL targets a cloud metadata endpoint")
	}
	if !policyIsLocalBackend(env) && !autoLocal && !policyAllowPrivateURLs(env) && !policyIsSafeURL(env, rawURL) {
		return policyErr("Blocked: URL targets a private or internal address")
	}
	if blocked := policyCheckWebsiteAccess(env, rawURL); blocked != nil {
		byPolicy := pyjson.NewObject()
		byPolicy.Set("host", blocked.Host)
		byPolicy.Set("rule", blocked.Rule)
		byPolicy.Set("source", blocked.Source)
		return policyErr(blocked.Message, "blocked_by_policy", byPolicy)
	}
	return nil
}

// Nil when the url is safe to open, autoLocal relaxes only the private address floor
func evaluateURLSafety(env *tools.Env, rawURL string, autoLocal bool) *pyjson.Object {
	err := secretURLError(rawURL)
	if err == nil {
		rawURL = tools.NormalizeURLForRequest(rawURL)
		err = secretURLError(rawURL)
	}
	if err != nil {
		return err
	}
	return urlPolicyError(env, rawURL, autoLocal)
}

// True when page driven private network access needs guarding on a non local backend without allow_private_urls
func evalSSRFGuardActive(env *tools.Env) bool {
	return !policyIsLocalBackend(env) && !policyAllowPrivateURLs(env)
}

// True when the url hits the metadata floor or fails the SSRF guard
func urlBlocked(env *tools.Env, rawURL string) bool {
	return tools.IsAlwaysBlockedURL(env, rawURL) || !policyIsSafeURL(env, rawURL)
}

// Every http or https literal in text, scheme relative ones too on request, plus the host read after a userinfo cut
func urlLiterals(text string, protocolRelative bool) []string {
	re := jsURLLiteralRE
	if protocolRelative {
		re = urlOrSchemeRelativeRE
	}
	runes := []rune(text)
	var found []string
	m, _ := re.FindRunesMatch(runes)
	for m != nil {
		lit := m.String()
		found = append(found, lit)
		head := literalHeadRE.FindString(lit)
		end := m.RuneIndex + m.RuneLength
		if !strings.ContainsAny(lit[len(head):], "/?#\\") {
			if tail, _ := userinfoTailAtRE.FindRunesMatchStartingAt(runes, end); tail != nil {
				if host, _ := urlBodyAtRE.FindRunesMatchStartingAt(runes, tail.RuneIndex+tail.RuneLength); host != nil {
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

// True when a literal hits the metadata floor, scheme relative ones judged by their literal host without DNS
func literalTargetsMetadata(env *tools.Env, literal string) bool {
	if !strings.HasPrefix(literal, "/") && !strings.HasPrefix(literal, "\\") {
		return tools.IsAlwaysBlockedURL(env, literal)
	}
	u := "http://" + strings.ReplaceAll(strings.TrimLeft(literal, "/\\"), "\\", "/")
	ambiguity, err := tools.AuthorityAmbiguity(u, true)
	if err != nil {
		return false
	}
	if ambiguity != "" {
		return true
	}
	host, err := tools.URLHostname(u, true)
	if err != nil {
		return false
	}
	ip, ok := pyipaddress.IPAddress(host)
	if !ok && numericHostRE.MatchString(host) {
		ip, ok = pyipaddress.InetAton(host)
	}
	return tools.IsBlockedHostname(host) || (ok && tools.IsAlwaysBlockedIP(ip))
}

// The first http or https literal in a JS expression that blocked refuses, empty when none
func expressionTargetsPrivateURL(expression string, blocked func(string) bool) string {
	for _, candidate := range urlLiterals(expression, false) {
		if blocked(candidate) {
			return candidate
		}
	}
	return ""
}
