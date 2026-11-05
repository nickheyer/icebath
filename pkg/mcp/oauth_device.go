package mcp

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net"
	"net/http"
	"strings"
	"time"
)

// deviceRuntimeError is a device login failure whose text is safe to show
type deviceRuntimeError struct{ msg string }

func (e *deviceRuntimeError) Error() string { return e.msg }

// deviceFieldError is a missing or malformed field of a device login response
type deviceFieldError struct{ err error }

func (e *deviceFieldError) Error() string { return e.err.Error() }

// deviceNetworkError is a device login request that never got an answer
type deviceNetworkError struct{ err error }

func (e *deviceNetworkError) Error() string { return e.err.Error() }

var (
	deviceNow       = time.Now
	deviceSleep     = sleepCtx
	deviceSetTokens = (*TokenStorage).SetTokens
)

// Sleeps for d, returning early with the ctx error when ctx ends
func sleepCtx(ctx context.Context, d time.Duration) error {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-timer.C:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// Seconds as a duration
func deviceSeconds(s float64) time.Duration { return time.Duration(s * float64(time.Second)) }

// Reports whether an error is a malformed field the login reports as invalid fields
func isDeviceFieldError(err error) bool {
	var fe *deviceFieldError
	var ve *ValidationError
	return errors.As(err, &fe) || errors.As(err, &ve)
}

// Sends one device login request and reads its answer, network failures marked as such
func deviceDo(ctx context.Context, client *http.Client, req *http.Request) (*flowResponse, error) {
	resp, err := client.Do(req.WithContext(ctx))
	if err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, &deviceNetworkError{err}
	}
	defer resp.Body.Close()
	data, err := readCapped(resp.Body, MaxHTTPBodyBytes, req.URL.String())
	if err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, &deviceNetworkError{err}
	}
	return &flowResponse{StatusCode: resp.StatusCode, Header: resp.Header, Body: data, Request: req}, nil
}

// Plain GET of a discovery URL
func deviceGet(ctx context.Context, client *http.Client, rawURL string) (*flowResponse, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return nil, &deviceNetworkError{err}
	}
	return deviceDo(ctx, client, req)
}

// JSON object of a 2xx answer, refusing invalid JSON, non objects and error statuses without echoing them
func devicePayload(r *flowResponse, label string) (map[string]any, error) {
	doc, err := decodeJSONDocument(r.Body)
	if err != nil {
		return nil, &deviceRuntimeError{label + ": invalid JSON response"}
	}
	m, ok := documentMap(doc)
	if !ok {
		return nil, &deviceRuntimeError{label + ": expected a JSON object"}
	}
	if r.StatusCode < 200 || r.StatusCode >= 300 {
		return nil, &deviceRuntimeError{fmt.Sprintf("%s failed (HTTP %d)", label, r.StatusCode)}
	}
	return m, nil
}

// Finds the protected resource metadata, then the first advertised authorization server that supports device login
func deviceDiscover(ctx context.Context, client *http.Client, p *OAuthProvider) error {
	resp, err := deviceGet(ctx, client, p.serverURL)
	if err != nil {
		return err
	}
	challenge := extractResourceMetadataFromWWWAuth(resp.StatusCode, resp.Header)
	var prm *ProtectedResourceMetadata
	for _, u := range buildProtectedResourceMetadataDiscoveryURLs(challenge, p.serverURL) {
		r, err := deviceGet(ctx, client, u)
		if err != nil {
			return err
		}
		if prm = handleProtectedResourceResponse(r); prm != nil {
			if err := p.validateResourceMatch(prm); err != nil {
				return err
			}
			p.protectedResourceMetadata = prm
			break
		}
	}
	servers := []string{""}
	if prm != nil {
		servers = prm.AuthorizationServers
	}
	var failures []string
	var lastErr error
	for _, authServerURL := range servers {
		md, err := deviceMetadata(ctx, client, p.serverURL, authServerURL)
		if err != nil {
			var flowErr *OAuthFlowError
			var runErr *deviceRuntimeError
			if !errors.As(err, &runErr) && !errors.As(err, &flowErr) && !isDeviceFieldError(err) {
				return err
			}
			label := authServerURL
			if label == "" {
				label = "None"
			}
			failures = append(failures, label+": "+err.Error())
			lastErr = err
			continue
		}
		p.authServerURL = authServerURL
		p.oauthMetadata = md
		return nil
	}
	if len(failures) == 1 {
		return lastErr
	}
	return &deviceRuntimeError{"No advertised authorization server supports device login: " + strings.Join(failures, "; ")}
}

// Issuer bound device metadata of one authorization server, failing when it cannot run a device login
func deviceMetadata(ctx context.Context, client *http.Client, serverURL, authServerURL string) (*OAuthMetadata, error) {
	for _, u := range buildOAuthAuthorizationServerMetadataDiscoveryURLs(authServerURL, serverURL) {
		r, err := deviceGet(ctx, client, u)
		if err != nil {
			return nil, err
		}
		if r.StatusCode == http.StatusNotFound {
			continue
		}
		data, err := devicePayload(r, "OAuth metadata")
		if err != nil {
			return nil, err
		}
		if !jsonTruthy(data["device_authorization_endpoint"]) {
			return nil, &deviceRuntimeError{"Server does not advertise device authorization; use --flow browser if supported"}
		}
		md, err := validateOAuthMetadata(data, true)
		if err != nil {
			return nil, err
		}
		expected := strings.TrimRight(authServerURL, "/")
		if expected != "" && !metadataIssuedByOrigin(md, expected, r) && strings.TrimRight(md.Issuer, "/") != expected {
			return nil, &OAuthFlowError{fmt.Sprintf("Authorization server metadata issuer mismatch: %s != %s", md.Issuer, expected)}
		}
		if md.present["grant_types_supported"] && !containsString(md.GrantTypesSupported, deviceCodeGrant) {
			return nil, &deviceRuntimeError{"Server does not advertise the device_code grant"}
		}
		return md, nil
	}
	return nil, &deviceRuntimeError{"No OAuth authorization server metadata found"}
}

// Registers a device client or adopts the configured one, binding it to the authorization server
func deviceRegister(ctx context.Context, client *http.Client, p *OAuthProvider, cfg map[string]any) error {
	dump := p.clientMetadata.dump()
	dump.set("grant_types", []any{deviceCodeGrant, "refresh_token"})
	dump.set("response_types", []any{})
	body, err := dump.MarshalJSON()
	if err != nil {
		return err
	}
	var data map[string]any
	if pyTruthy(cfg["client_id"]) {
		doc, err := decodeJSONDocument(body)
		if err != nil {
			return err
		}
		data, _ = documentMap(doc)
		data["client_id"] = cfg["client_id"]
		if pyTruthy(cfg["client_secret"]) {
			data["client_secret"] = cfg["client_secret"]
		}
	} else {
		endpoint := derefStr(p.oauthMetadata.RegistrationEndpoint)
		if endpoint == "" {
			return &deviceRuntimeError{"Server has no registration endpoint; configure oauth.client_id (and client_secret if required)"}
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
		if err != nil {
			return &deviceNetworkError{err}
		}
		req.Header.Set("Content-Type", "application/json")
		r, err := deviceDo(ctx, client, req)
		if err != nil {
			return err
		}
		if data, err = devicePayload(r, "Client registration"); err != nil {
			return err
		}
	}
	issuer := p.authServerURL
	if issuer == "" {
		issuer = p.oauthMetadata.Issuer
	}
	data["issuer"] = issuer
	info, err := validateClientInfo(data)
	if err != nil {
		return err
	}
	p.clientInfo = info
	p.coerceClientSecretPost()
	if err := checkRegistrationUsable(p.clientInfo); err != nil {
		var regErr *OAuthRegistrationError
		if errors.As(err, &regErr) {
			return &deviceRuntimeError{"Device OAuth client has unsupported or incomplete token endpoint authentication"}
		}
		return err
	}
	return nil
}

// Positive finite seconds of a response or config value
func devicePositiveSeconds(v any, label string) (float64, error) {
	if n, ok := v.(json.Number); ok {
		v = n.String()
	}
	f, err := pyFloatStrict(v)
	if err != nil {
		return 0, &deviceFieldError{err}
	}
	if math.IsNaN(f) || math.IsInf(f, 0) || f <= 0 {
		return 0, &deviceRuntimeError{"Device authorization has invalid " + label}
	}
	return f, nil
}

// Starts the device authorization, prints the code and polls the token endpoint until approval, denial or expiry
func deviceAuthorize(ctx context.Context, client *http.Client, p *OAuthProvider, cfg map[string]any) (*OAuthToken, error) {
	resource := p.getResourceURL()
	data := form{{"client_id", p.clientInfo.ClientID}, {"resource", resource}}
	if p.clientMetadata.Scope != nil && *p.clientMetadata.Scope != "" {
		data.set("scope", *p.clientMetadata.Scope)
	}
	for _, kv := range p.googleOfflineAccessParams() {
		data.set(kv[0], kv[1])
	}
	headers := http.Header{}
	if err := p.prepareTokenAuth(&data, headers); err != nil {
		return nil, err
	}
	req, _, err := newFormRequest(ctx, derefStr(p.oauthMetadata.DeviceAuthorizationEndpoint), data, headers)
	if err != nil {
		return nil, &deviceNetworkError{err}
	}
	r, err := deviceDo(ctx, client, req)
	if err != nil {
		return nil, err
	}
	authorization, err := devicePayload(r, "Device authorization")
	if err != nil {
		return nil, err
	}
	for _, key := range []string{"device_code", "user_code", "verification_uri"} {
		if s, ok := authorization[key].(string); !ok || s == "" {
			return nil, &deviceRuntimeError{"Device authorization is missing " + key}
		}
	}
	verification, err := normalizeURL(authorization["verification_uri"].(string), true)
	if err != nil {
		return nil, &deviceFieldError{err}
	}
	intervalValue, ok := authorization["interval"]
	if !ok {
		intervalValue = 5
	}
	interval, err := devicePositiveSeconds(intervalValue, "interval")
	if err != nil {
		return nil, err
	}
	expiresValue, ok := authorization["expires_in"]
	if !ok {
		return nil, &deviceFieldError{errors.New("'expires_in'")}
	}
	expiresIn, err := devicePositiveSeconds(expiresValue, "expires_in")
	if err != nil {
		return nil, err
	}
	timeoutValue, ok := cfg["timeout"]
	if !ok {
		timeoutValue = 300
	}
	window, err := devicePositiveSeconds(timeoutValue, "timeout")
	if err != nil {
		return nil, err
	}
	deadline := deviceNow().Add(deviceSeconds(math.Min(expiresIn, window)))
	fmt.Fprintf(oauthStderr, "\n  MCP OAuth: open %s on any device.\n  Code: %s\n  Waiting for approval...\n\n", verification, authorization["user_code"])
	tokenData := form{{"client_id", p.clientInfo.ClientID}, {"device_code", authorization["device_code"].(string)},
		{"grant_type", deviceCodeGrant}, {"resource", resource}}
	tokenHeaders := http.Header{}
	if err := p.prepareTokenAuth(&tokenData, tokenHeaders); err != nil {
		return nil, err
	}
	for {
		if deadline.Sub(deviceNow()) <= deviceSeconds(interval) {
			return nil, &deviceRuntimeError{"Device authorization expired before approval; run login again"}
		}
		if err := deviceSleep(ctx, deviceSeconds(interval)); err != nil {
			return nil, err
		}
		tokenReq, _, err := newFormRequest(ctx, p.oauthMetadata.TokenEndpoint, tokenData, tokenHeaders)
		if err != nil {
			return nil, &deviceNetworkError{err}
		}
		p.prepareTokenRequest(tokenReq)
		pollCtx, cancel := context.WithTimeout(ctx, deadline.Sub(deviceNow()))
		resp, err := deviceDo(pollCtx, client, tokenReq)
		cancel()
		if err != nil {
			var ne net.Error
			if ctx.Err() == nil && (errors.Is(err, context.DeadlineExceeded) || (errors.As(err, &ne) && ne.Timeout())) {
				interval *= 2
				continue
			}
			return nil, err
		}
		if resp.StatusCode >= 200 && resp.StatusCode < 300 {
			payload, err := devicePayload(resp, "Device token")
			if err != nil {
				return nil, err
			}
			tokens, err := validateOAuthToken(payload)
			if err != nil {
				return nil, err
			}
			if tokens.AccessToken == "" {
				return nil, &deviceRuntimeError{"Device token response has no access token"}
			}
			if tokens.Scope == nil && p.clientMetadata.Scope != nil {
				tokens.Scope = strPtr(*p.clientMetadata.Scope)
			}
			return tokens, nil
		}
		var errorCode any
		if doc, derr := decodeJSONDocument(resp.Body); derr == nil {
			if m, ok := documentMap(doc); ok {
				errorCode = m["error"]
			}
		}
		switch errorCode {
		case "authorization_pending":
			continue
		case "slow_down":
			interval += 5
			continue
		}
		safe := fmt.Sprintf("HTTP %d", resp.StatusCode)
		if errorCode == "access_denied" || errorCode == "expired_token" {
			safe = errorCode.(string)
		}
		return nil, &deviceRuntimeError{"Device authorization failed: " + safe}
	}
}

// Runs an RFC 8628 device login and commits client, metadata and tokens together, restoring the old state on a write failure
func LoginDevice(ctx context.Context, name, serverURL string, oauthConfig map[string]any, transport HTTPOptions) error {
	cfg, storage := prepareOAuthConfig(ctx, name, serverURL, oauthConfig)
	port, ok := cfg["redirect_port"]
	if !ok {
		port = 8420
	}
	cfg["_resolved_port"] = port
	md, err := buildClientMetadata(cfg)
	if err != nil {
		return err
	}
	p, err := newOAuthProvider(oauthProviderOptions{ServerURL: serverURL, ClientMetadata: md, Storage: storage,
		TokenUserAgent: tokenRequestUserAgent(cfg), OAuthFlow: "device", HTTP: transport})
	if err != nil {
		return err
	}
	client := &http.Client{Transport: p.client.Transport, CheckRedirect: p.client.CheckRedirect, Timeout: 10 * time.Second}
	tokens, err := deviceGrant(ctx, client, p, cfg)
	if err != nil {
		var netErr *deviceNetworkError
		switch {
		case isDeviceFieldError(err):
			return errors.New("Device OAuth response has invalid fields")
		case errors.As(err, &netErr):
			return errors.New("Device OAuth network request failed")
		}
		return err
	}
	previous := storage.Snapshot()
	commit := func() error {
		if err := storage.SetClientInfo(p.clientInfo); err != nil {
			return err
		}
		if err := storage.SaveOAuthMetadata(p.oauthMetadata); err != nil {
			return err
		}
		return deviceSetTokens(storage, tokens)
	}
	if err := commit(); err != nil {
		if rerr := storage.Restore(previous, false); rerr != nil {
			return errors.Join(err, rerr)
		}
		return err
	}
	getManager().evict(ctx, name, "")
	return nil
}

// Discovery, registration and authorization of one device login
func deviceGrant(ctx context.Context, client *http.Client, p *OAuthProvider, cfg map[string]any) (*OAuthToken, error) {
	if err := deviceDiscover(ctx, client, p); err != nil {
		return nil, err
	}
	if err := deviceRegister(ctx, client, p, cfg); err != nil {
		return nil, err
	}
	return deviceAuthorize(ctx, client, p, cfg)
}
