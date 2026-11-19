package mcp

import (
	"context"
	"fmt"
	"net"
	"net/url"
	"strings"
	"time"

	"github.com/nickheyer/icebath/pkg/tools"
	"golang.org/x/net/idna"
	"golang.org/x/net/publicsuffix"
)

// Web link of a URL mode elicitation, an invalid params error for anything but an absolute http or https URL with a host
func elicitationURLOf(raw string) (*url.URL, *RPCError) {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Hostname() == "" || u.Opaque != "" {
		return nil, &RPCError{Code: CodeInvalidParams,
			Message: fmt.Sprintf("Invalid params: a url mode elicitation needs an absolute http or https url with a host, got %q", raw)}
	}
	return u, nil
}

// Registrable domain of a host, the host itself for an address, a single label or an unlisted suffix
func registrableDomain(host string) string {
	if net.ParseIP(host) != nil {
		return host
	}
	if domain, err := publicsuffix.EffectiveTLDPlusOne(host); err == nil {
		return domain
	}
	return host
}

// Consent text showing the whole link, its domain and the warnings a spoofed or unsafe link earns
func formatURLElicitation(u *url.URL, serverName string) string {
	host := strings.ToLower(u.Hostname())
	ascii, asciiErr := idna.Lookup.ToASCII(host)
	if asciiErr != nil {
		ascii = host
	}
	lines := []string{
		fmt.Sprintf("MCP server '%s' asks you to open this link:", serverName),
		"  " + u.String(),
		"Domain: " + registrableDomain(ascii),
	}
	if ascii != registrableDomain(ascii) {
		lines[2] += " (host " + ascii + ")"
	}
	if strings.Contains(ascii, "xn--") || ascii != host {
		shown, err := idna.Display.ToUnicode(ascii)
		if err != nil {
			shown = host
		}
		lines = append(lines, fmt.Sprintf("Warning: the domain uses international letters (punycode %s, shown as %s). Check it is the site you expect.", ascii, shown))
	}
	if u.User != nil {
		lines = append(lines, fmt.Sprintf("Warning: the link carries a user name before the host; the site it opens is %s.", ascii))
	}
	if u.Scheme == "http" && !isLoopbackHost(ascii) {
		lines = append(lines, "Warning: the link is not encrypted (http).")
	}
	lines = append(lines, "Accept, then finish the step on that page. What you enter there goes to the site and never through icebath.")
	return strings.Join(lines, "\n")
}

// Reports whether a host names this machine
func isLoopbackHost(host string) bool {
	if host == "localhost" || strings.HasSuffix(host, ".localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// Asks consent to open a URL mode elicitation's link, the accept opening it and tracking its id until the server reports it done
func (h *elicitationHandler) elicitURL(ctx context.Context, params *ElicitParams) (*ElicitResult, *RPCError) {
	link, rpcErr := elicitationURLOf(params.URL)
	if rpcErr != nil {
		h.count("errors")
		pkgLog().Warn(fmt.Sprintf("MCP server '%s' sent a url mode elicitation with an unusable url: %s", h.serverName, truncateRunes(sanitizeError(params.URL), 200)))
		return nil, rpcErr
	}
	message := params.Message
	if message == "" {
		message = fmt.Sprintf("MCP server '%s' asks you to open a link", h.serverName)
	}
	pkgLog().Info(fmt.Sprintf("MCP server '%s' url elicitation request for %s: %s", h.serverName, link.Hostname(), truncateRunes(sanitizeError(message), 200)))
	var done chan struct{}
	if params.ElicitationID != "" {
		done = make(chan struct{})
		h.mu.Lock()
		h.pending[params.ElicitationID] = done
		h.mu.Unlock()
	}
	answer, metric := h.ask(ctx, message, formatURLElicitation(link, h.serverName),
		tools.ElicitationOptions{Title: "Open this link?", URL: link.String()})
	h.count(metric)
	if answer != "accept" && params.ElicitationID != "" {
		h.mu.Lock()
		if h.pending[params.ElicitationID] == done {
			delete(h.pending, params.ElicitationID)
		}
		h.mu.Unlock()
	}
	return &ElicitResult{Action: answer}, nil
}

// Marks a URL mode elicitation the server reports complete, ignoring unknown and finished ids
func (h *elicitationHandler) ElicitationComplete(id string) {
	h.mu.Lock()
	done, ok := h.pending[id]
	delete(h.pending, id)
	h.mu.Unlock()
	if !ok {
		pkgLog().Debug(fmt.Sprintf("MCP server '%s' reported unknown or finished elicitation %s; ignored", h.serverName, truncateRunes(id, 80)))
		return
	}
	close(done)
	pkgLog().Info(fmt.Sprintf("MCP server '%s' reports the browser step of elicitation %s finished", h.serverName, truncateRunes(id, 80)))
}

// Channel closed once the server reports the elicitation finished, nil when the id is not waiting
func (h *elicitationHandler) ElicitationDone(id string) <-chan struct{} {
	h.mu.Lock()
	defer h.mu.Unlock()
	if done, ok := h.pending[id]; ok {
		return done
	}
	return nil
}

// Time the user gets to finish a browser step before a request waiting on it gives up
func (h *elicitationHandler) ElicitationWait() time.Duration {
	return time.Duration(h.timeout * float64(time.Second))
}

// urlElicitationTracker is an elicitor that follows accepted URL mode elicitations until the server reports them finished
type urlElicitationTracker interface {
	ElicitationComplete(id string)
	ElicitationDone(id string) <-chan struct{}
	ElicitationWait() time.Duration
}
