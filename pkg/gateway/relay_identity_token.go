package gateway

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/nickheyer/icebath/pkg/errchain"
	"github.com/nickheyer/icebath/pkg/internal/netpref"
	"github.com/nickheyer/icebath/pkg/internal/pyhttpx"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
	"github.com/nickheyer/icebath/pkg/internal/pyurl"
	"github.com/nickheyer/icebath/pkg/settings"
)

// JWT of three or more dotted segments or a long opaque base64url bearer, the ambient endpoint body shape
var relayAmbientTokenShape = regexp.MustCompile(`^(?:[A-Za-z0-9_-]+(?:\.[A-Za-z0-9_-]+){2,}|[A-Za-z0-9_-]{32,})$`)

const relayHTTPTimeout = 15 * time.Second

// RelayHTTPError is the urllib HTTPError of a relay request with its status, reason and error body
type RelayHTTPError struct {
	Code   int
	Reason string
	Header http.Header
	Body   []byte
}

func (e *RelayHTTPError) Error() string { return fmt.Sprintf("HTTP %d %s", e.Code, e.Reason) }

// RelayURLError is a relay request failure before any HTTP reply, carrying the transport reason
type RelayURLError struct{ Reason string }

func (e *RelayURLError) Error() string { return "could not reach the relay: " + e.Reason }

// urllib client with environment proxies, a per operation socket timeout and urllib redirect rules
func relayURLOpenClient(timeout time.Duration) *http.Client {
	dialer := &net.Dialer{Timeout: timeout}
	transport := &http.Transport{Proxy: pyurl.UrlopenProxy, DisableKeepAlives: true, DialContext: netpref.DialContext(dialer.DialContext)}
	return &http.Client{Transport: pyhttpx.TimeoutTransport(transport, timeout), CheckRedirect: func(req *http.Request, via []*http.Request) error {
		if via[0].Method == http.MethodPost && (req.Response.StatusCode == 307 || req.Response.StatusCode == 308) {
			return http.ErrUseLastResponse
		}
		target := req.URL.String()
		visits, distinct := 0, map[string]bool{}
		for _, prior := range via[1:] {
			distinct[prior.URL.String()] = true
			if prior.URL.String() == target {
				visits++
			}
		}
		if visits >= 4 || len(distinct) >= 10 {
			return http.ErrUseLastResponse
		}
		return nil
	}}
}

// Reason text of a urlopen transport failure, the str of the socket error it wraps
func relayURLErrorReason(err error) string {
	var ne net.Error
	if errors.Is(err, os.ErrDeadlineExceeded) || errors.Is(err, context.DeadlineExceeded) || (errors.As(err, &ne) && ne.Timeout()) {
		return "timed out"
	}
	var dnsErr *net.DNSError
	if errors.As(err, &dnsErr) && dnsErr.IsNotFound {
		return "host name could not be resolved"
	}
	var errno syscall.Errno
	if errors.As(err, &errno) {
		return errno.Error()
	}
	var urlErr *url.Error
	if errors.As(err, &urlErr) {
		return urlErr.Err.Error()
	}
	return err.Error()
}

// Body of a 2xx relay response, a non 2xx final status failing as RelayHTTPError and transport failures as RelayURLError
func RelayURLOpen(req *http.Request, timeout time.Duration) ([]byte, error) {
	resp, err := relayURLOpenResponse(req, timeout)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	return io.ReadAll(resp.Body)
}

// Open 2xx response of a urlopen call with identity encoding, other statuses as RelayHTTPError
func relayURLOpenResponse(req *http.Request, timeout time.Duration) (*http.Response, error) {
	if req.Header.Get("Accept-Encoding") == "" {
		req.Header.Set("Accept-Encoding", "identity")
	}
	resp, err := relayURLOpenClient(timeout).Do(req)
	if err != nil {
		return nil, &RelayURLError{Reason: relayURLErrorReason(err)}
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		defer resp.Body.Close()
		body, _ := io.ReadAll(resp.Body)
		reason := strings.TrimPrefix(resp.Status, strconv.Itoa(resp.StatusCode)+" ")
		return nil, &RelayHTTPError{Code: resp.StatusCode, Reason: reason, Header: resp.Header, Body: body}
	}
	return resp, nil
}

// Caller identity bearer the connector introspects, client credentials or an ambient endpoint of the configured IdP
func ResolveRelayIdentityToken(ctx context.Context) (string, error) {
	tokenURL, clientID := relaySetting(ctx, settings.GatewayIdpTokenURL), relaySetting(ctx, settings.GatewayIdpClientID)
	clientSecret, scope := relaySetting(ctx, settings.GatewayIdpClientSecret), relaySetting(ctx, settings.GatewayIdpScope)
	if tokenURL == "" {
		return "", errors.New("the relay needs a caller identity: set gateway.idp.token_url (or GATEWAY_RELAY_IDP_TOKEN_URL) " +
			"with client_id and client_secret for the OAuth2 client_credentials grant, or token_url alone for an ambient token endpoint.")
	}
	if clientID == "" && clientSecret == "" {
		return relayAmbientToken(ctx, tokenURL)
	}
	if clientID == "" || clientSecret == "" {
		missing := "client_id"
		if clientID != "" {
			missing = "client_secret"
		}
		return "", fmt.Errorf("gateway.idp.token_url is configured with a partial client credential "+
			"(%s missing). Configure both client_id and client_secret for "+
			"the OAuth2 client_credentials grant, or neither to treat token_url "+
			"as an ambient token endpoint (plain GET returning the token).", missing)
	}
	form := [][2]string{{"grant_type", "client_credentials"}, {"client_id", clientID}, {"client_secret", clientSecret}}
	if scope != "" {
		form = append(form, [2]string{"scope", scope})
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, tokenURL, strings.NewReader(pyurl.URLEncode(form)))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")
	raw, err := RelayURLOpen(req, relayHTTPTimeout)
	if err != nil {
		return "", err
	}
	text, err := pystr.DecodeUTF8(raw)
	if err != nil {
		return "", err
	}
	payload, err := pyjson.Loads(text)
	if err != nil {
		return "", err
	}
	var accessToken any
	if pyrepr.Truthy(payload) {
		obj, ok := payload.(*pyjson.Object)
		if !ok {
			return "", fmt.Errorf("the relay identity token reply is %s, expected a mapping", errchain.Kind(payload))
		}
		accessToken = obj.GetOr("access_token", nil)
	}
	s, ok := accessToken.(string)
	if !ok || strings.TrimSpace(s) == "" {
		return "", errors.New("IdP client_credentials response had no access_token")
	}
	return strings.TrimSpace(s), nil
}

// Token from a plain GET of a metadata style endpoint, a raw JWT or opaque body or a JSON envelope
func relayAmbientToken(ctx context.Context, tokenURL string) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, tokenURL, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("Accept", "application/json, text/plain")
	raw, err := RelayURLOpen(req, relayHTTPTimeout)
	if err != nil {
		return "", err
	}
	decoded, err := pystr.DecodeUTF8(raw)
	if err != nil {
		return "", err
	}
	body := strings.TrimSpace(decoded)
	token := ""
	if strings.HasPrefix(body, "{") {
		if parsed, err := pyjson.Loads(body); err == nil {
			if obj, ok := parsed.(*pyjson.Object); ok {
				if s, ok := obj.GetOr("access_token", nil).(string); ok {
					token = strings.TrimSpace(s)
				}
			}
		}
	} else if relayAmbientTokenShape.MatchString(body) {
		token = body
	}
	if token == "" {
		return "", errors.New("no client_id/client_secret configured, so gateway.idp.token_url was " +
			"treated as an ambient token endpoint (GET), but the response body " +
			"was not a token. For the OAuth2 client_credentials grant, configure " +
			"client_id and client_secret alongside token_url.")
	}
	return token, nil
}
