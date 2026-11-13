package tools

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"

	"github.com/nickheyer/icebath/pkg/internal/pydantic"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pyrequests"
)

// PortalDefaultTimeoutSeconds bounds every Portal connector metadata request
const PortalDefaultTimeoutSeconds = 30.0

var portalSlugRE = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]*$`)

var portalRouteWords = map[string]bool{"connectors": true, "tools": true, "catalog": true, "public": true, "accounts": true, "policy": true}

// Error kinds of the Portal connector errors, both of them ToolGatewayError subclasses upstream
const (
	PortalKindConnectorUnavailable = "portal_connector_unavailable"
	PortalKindToolsUnavailable     = "portal_tools_unavailable"
)

// InvalidConnectorSlug is the ValueError a malformed connector slug raises
type InvalidConnectorSlug struct{ Msg string }

func (e *InvalidConnectorSlug) Error() string { return e.Msg }

// PortalNotModified is the 304 answer to a conditional tools read
type PortalNotModified struct{}

// NousPortalBaseURL resolves the stored login's Portal origin, installed by pkg/mcp over pkg/provider
var NousPortalBaseURL func(ctx context.Context) (string, error)

// Reports whether err is a PortalConnectorUnavailable, the tools subclass included
func IsPortalConnectorUnavailable(err error) bool {
	ge, ok := err.(*ToolGatewayError)
	return ok && (ge.Kind == PortalKindConnectorUnavailable || ge.Kind == PortalKindToolsUnavailable)
}

// Refuses a connector name that is not a lowercase slug
func ValidateSlug(slug string) error {
	if !portalSlugRE.MatchString(slug) || strings.HasSuffix(slug, "\n") {
		return &InvalidConnectorSlug{Msg: "connector must be a slug"}
	}
	return nil
}

// PortalTransport sends one Portal request, the requests module seam tests replace
type PortalTransport func(ctx context.Context, method, rawURL string, headers map[string]string, body any, timeout time.Duration) (*http.Response, error)

// PortalOptions replaces the transport, endpoint resolver or header provider of a Portal client
type PortalOptions struct {
	Transport        PortalTransport
	EndpointResolver func() (string, error)
	HeaderProvider   func(rawURL string) map[string]string
}

// PortalConnectorClient reads and writes the Portal connector catalog, accounts and policy
type PortalConnectorClient struct {
	ctx       context.Context
	env       *Env
	transport PortalTransport
	endpoint  func() (string, error)
	headers   func(rawURL string) map[string]string
}

// Client over the default transport, the stored Portal origin and the Nous bearer token
func NewPortalConnectorClient(ctx context.Context, env *Env, opts PortalOptions) *PortalConnectorClient {
	c := &PortalConnectorClient{ctx: ctx, env: env, transport: opts.Transport, endpoint: opts.EndpointResolver, headers: opts.HeaderProvider}
	if c.transport == nil {
		c.transport = portalDefaultTransport
	}
	if c.endpoint == nil {
		c.endpoint = func() (string, error) { return portalDefaultEndpointResolver(ctx) }
	}
	if c.headers == nil {
		c.headers = func(string) map[string]string { return portalDefaultHeaderProvider(env) }
	}
	return c
}

// Portal origin of the stored Nous login
func portalDefaultEndpointResolver(ctx context.Context) (string, error) {
	return NousPortalBaseURL(ctx)
}

// Bearer header of the stored Nous access token for any Portal URL, none when no token is stored
func portalDefaultHeaderProvider(env *Env) map[string]string {
	if token := readNousAccessToken(env); strings.TrimSpace(token) != "" {
		return map[string]string{"Authorization": "Bearer " + token}
	}
	return map[string]string{}
}

func portalDefaultTransport(ctx context.Context, method, rawURL string, headers map[string]string, body any, timeout time.Duration) (*http.Response, error) {
	var reader io.Reader
	if body != nil {
		raw, err := pyjson.Dumps(body, pyjson.AllowNaN(false))
		if err != nil {
			return nil, err
		}
		reader = strings.NewReader(raw)
	}
	req, err := http.NewRequestWithContext(ctx, method, rawURL, reader)
	if err != nil {
		return nil, err
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	client := &http.Client{Timeout: timeout}
	if bundle := pyrequests.EnvCABundle(); bundle != "" {
		verify, err := pyrequests.VerifyTLSConfig(bundle)
		if err != nil {
			return nil, err
		}
		transport := http.DefaultTransport.(*http.Transport).Clone()
		transport.TLSClientConfig = verify
		client.Transport = transport
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	data, err := io.ReadAll(resp.Body)
	resp.Body.Close()
	if err != nil {
		return nil, err
	}
	resp.Body = io.NopCloser(bytes.NewReader(data))
	return resp, nil
}

// The Portal origin with no trailing slash
func (c *PortalConnectorClient) Origin() (string, error) {
	origin, err := c.endpoint()
	if err != nil {
		return "", err
	}
	return strings.TrimRight(origin, "/"), nil
}

// Fails with the NO_TOKEN auth error when no bearer token resolves
func (c *PortalConnectorClient) RequireAuthentication() error {
	origin, err := c.Origin()
	if err != nil {
		return err
	}
	_, err = c.authorizedHeaders(origin, nil)
	return err
}

// The bearer token the client sends, empty when the Authorization header is not a bearer
func (c *PortalConnectorClient) AuthorizationToken() (string, error) {
	origin, err := c.Origin()
	if err != nil {
		return "", err
	}
	headers, err := c.authorizedHeaders(origin, nil)
	if err != nil {
		return "", err
	}
	scheme, token, _ := strings.Cut(headers["Authorization"], " ")
	if strings.ToLower(scheme) == "bearer" && strings.TrimSpace(token) != "" {
		return strings.TrimSpace(token), nil
	}
	return "", nil
}

func (c *PortalConnectorClient) authorizedHeaders(rawURL string, extra map[string]string) (map[string]string, error) {
	identity, err := connectorHeaders(c.ctx, c.env, nil)
	if err != nil {
		return nil, err
	}
	headers := map[string]string{"Accept": "application/json"}
	for _, layer := range []map[string]string{extra, identity, c.headers(rawURL)} {
		for k, v := range layer {
			headers[k] = v
		}
	}
	if strings.TrimSpace(headers["Authorization"]) == "" {
		return nil, &ToolGatewayError{Message: "portal authorization required", Code: "NO_TOKEN", Status: 401, Kind: gatewayKindAuth}
	}
	return headers, nil
}

// The connector accounts of the caller's principal, each in the wire model's alias keyed dump
func (c *PortalConnectorClient) ListAccounts() ([]map[string]any, error) {
	resp, status, route, err := c.request("GET", "/api/v1/connectors/accounts", nil, nil, false)
	if err != nil {
		return nil, err
	}
	parsed, err := c.parse(resp, status, route, PortalKindConnectorUnavailable, "portal accounts unavailable", func(m map[string]any) (any, error) {
		inventory, verr := ConnectorAccountsResponse.Validate(m)
		if verr != nil {
			return nil, verr
		}
		rows := inventory.Get("accounts").([]any)
		accounts := make([]map[string]any, 0, len(rows))
		for _, account := range rows {
			accounts = append(accounts, wireDump(account.(*pydantic.Instance), true))
		}
		return accounts, nil
	})
	if err != nil {
		return nil, err
	}
	return parsed.([]map[string]any), nil
}

// Names one account alias, answering the renamed account
func (c *PortalConnectorClient) RenameAccount(connectionID, alias string) (map[string]any, error) {
	resp, status, route, err := c.request("PATCH", "/api/v1/connectors/accounts/"+url.PathEscape(connectionID), map[string]any{"alias": alias}, nil, false)
	if err != nil {
		return nil, err
	}
	parsed, err := c.parse(resp, status, route, PortalKindConnectorUnavailable, "portal accounts unavailable", func(m map[string]any) (any, error) {
		return wireModelDump(ConnectorAccount, m)
	})
	if err != nil {
		return nil, err
	}
	return parsed.(map[string]any), nil
}

// Removes one account, answering the removed row
func (c *PortalConnectorClient) DeleteAccount(connectionID string) (map[string]any, error) {
	resp, status, route, err := c.request("DELETE", "/api/v1/connectors/accounts/"+url.PathEscape(connectionID), nil, nil, false)
	if err != nil {
		return nil, err
	}
	parsed, err := c.parse(resp, status, route, PortalKindConnectorUnavailable, "portal accounts unavailable", func(m map[string]any) (any, error) {
		return wireModelDump(RemovedConnectorAccount, m)
	})
	if err != nil {
		return nil, err
	}
	return parsed.(map[string]any), nil
}

// Alias keyed dump of a body the model validates, the validation error otherwise
func wireModelDump(model *pydantic.Model, m map[string]any) (any, error) {
	inst, verr := model.Validate(m)
	if verr != nil {
		return nil, verr
	}
	return wireDump(inst, true), nil
}

// Sends one request, raising the parsed gateway error for a failed status and logging a 304
func (c *PortalConnectorClient) request(method, path string, body any, extra map[string]string, public bool) (*http.Response, int, string, error) {
	route := portalRouteKey(path)
	origin, err := c.Origin()
	if err != nil {
		return nil, 0, route, err
	}
	u := origin + path
	var headers map[string]string
	if public {
		identity, err := connectorHeaders(c.ctx, c.env, nil)
		if err != nil {
			return nil, 0, route, err
		}
		headers = map[string]string{"Accept": "application/json"}
		for _, layer := range []map[string]string{extra, identity} {
			for k, v := range layer {
				headers[k] = v
			}
		}
	} else if headers, err = c.authorizedHeaders(u, extra); err != nil {
		return nil, 0, route, err
	}
	resp, err := c.transport(c.ctx, method, u, headers, body, time.Duration(PortalDefaultTimeoutSeconds*float64(time.Second)))
	if err != nil {
		logConnectorOutcome(c.env, route, 0, false, "TRANSPORT_ERROR", "", nil)
		return nil, 0, route, &ToolGatewayError{Message: "portal connector metadata unavailable", Code: "TRANSPORT_ERROR", Retryable: true, Kind: PortalKindConnectorUnavailable}
	}
	status := resp.StatusCode
	requestID := responseRequestID(resp)
	if (status < 200 || status >= 300) && status != 304 {
		gerr, err := ParseGatewayError(status, portalSafeJSON(resp), resp.Header)
		if err != nil {
			return nil, status, route, err
		}
		if gerr.RequestID == "" {
			gerr.RequestID = requestID
		}
		logConnectorOutcome(c.env, route, status, true, gerr.Code, gerr.RequestID, nil)
		return nil, status, route, gerr
	}
	if status == 304 {
		logConnectorOutcome(c.env, route, status, true, "", requestID, nil)
	}
	return resp, status, route, nil
}

// Validates a 2xx body through model, raising kind's INVALID_RESPONSE error for any shape fault
func (c *PortalConnectorClient) parse(resp *http.Response, status int, route, kind, message string, model func(map[string]any) (any, error)) (any, error) {
	requestID := responseRequestID(resp)
	payload, err := portalJSON(resp)
	var parsed any
	if err == nil {
		m, ok := payload.(map[string]any)
		if !ok {
			err = &wireError{"response must be an object"}
		} else {
			parsed, err = model(m)
		}
	}
	if err != nil {
		logConnectorOutcome(c.env, route, status, true, "INVALID_RESPONSE", requestID, nil)
		return nil, &ToolGatewayError{Message: message, Code: "INVALID_RESPONSE", Status: status, RequestID: requestID, Kind: kind}
	}
	logConnectorOutcome(c.env, route, status, true, "", requestID, nil)
	return parsed, nil
}

func portalJSON(resp *http.Response) (any, error) {
	if resp == nil || resp.Body == nil {
		return nil, &wireError{"no body"}
	}
	data, err := io.ReadAll(resp.Body)
	resp.Body = io.NopCloser(bytes.NewReader(data))
	if err != nil {
		return nil, err
	}
	v, err := pyjson.LoadsBytes(data)
	if err != nil {
		return nil, err
	}
	return pyjson.WTF8Shape(v), nil
}

func portalSafeJSON(resp *http.Response) any {
	v, err := portalJSON(resp)
	if err != nil {
		return nil
	}
	return v
}

// Route key with no slugs or ids, /api/v1/connectors/gmail/tools becomes portal.connectors.tools
func portalRouteKey(path string) string {
	parts := strings.Split(strings.Trim(path, "/"), "/")
	if len(parts) > 2 {
		parts = parts[2:]
	} else {
		parts = nil
	}
	named := []string{"portal"}
	for _, p := range parts {
		if portalRouteWords[p] {
			named = append(named, p)
		}
	}
	return strings.Join(named, ".")
}
