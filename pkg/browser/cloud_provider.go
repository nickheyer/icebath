package browser

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"sync"

	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pyrequests"
	"github.com/nickheyer/icebath/pkg/provider"
	"github.com/nickheyer/icebath/pkg/tools"
)

// Release answers counted as a closed session
var cloudCloseOK = map[int]bool{200: true, 201: true, 204: true}

// Python exception raised by a cloud provider, Class its class name and Msg its str
type cloudError struct {
	Class, Msg string
}

func (e *cloudError) Error() string      { return e.Msg }
func (e *cloudError) PyTypeName() string { return e.Class }

func valueError(msg string) error   { return &cloudError{Class: "ValueError", Msg: msg} }
func runtimeError(msg string) error { return &cloudError{Class: "RuntimeError", Msg: msg} }

func isValueError(err error) bool {
	var ce *cloudError
	return errors.As(err, &ce) && ce.Class == "ValueError"
}

// Credentials and endpoint one call of a cloud provider uses
type cloudConfig struct {
	APIKey, ProjectID, BaseURL string
	ManagedMode                bool
}

// Vendor hooks the shared lifecycle calls, the subclass overrides of CloudBrowserProvider
type cloudHooks interface {
	configOrNone(env *tools.Env) (*cloudConfig, error)
	config(env *tools.Env) (*cloudConfig, error)
	headers(env *tools.Env, c *cloudConfig) (map[string]string, error)
	releaseHeaders(env *tools.Env, c *cloudConfig) (map[string]string, error)
	releaseBody(c *cloudConfig) any
}

// Shared REST lifecycle of the bundled cloud browser vendors, one create request and one release request
type CloudBrowserProvider struct {
	providerID, label, module  string
	releaseMethod, releasePath string
	missingCredentialsError    string
	setupTag                   string
	setupEnvVars               [][3]string
	createLabelSuffix          string
	closeFailFmt               string
	hooks                      cloudHooks
}

func (p *CloudBrowserProvider) Name() string        { return p.providerID }
func (p *CloudBrowserProvider) DisplayName() string { return p.label }

func (p *CloudBrowserProvider) log(env *tools.Env) *slog.Logger {
	return cfgOf(env).logger().With("logger", p.module)
}

// Available when the vendor config resolves, a raised resolution error propagating as a panic
func (p *CloudBrowserProvider) IsAvailable(env *tools.Env) bool {
	c, err := p.hooks.configOrNone(env)
	if err != nil {
		panic(err)
	}
	return c != nil
}

// Vendor config or the missing credentials ValueError
func (p *CloudBrowserProvider) baseConfig(env *tools.Env) (*cloudConfig, error) {
	c, err := p.hooks.configOrNone(env)
	if err != nil {
		return nil, err
	}
	if c == nil {
		return nil, valueError(p.missingCredentialsError)
	}
	return c, nil
}

func (p *CloudBrowserProvider) releaseHeaders(env *tools.Env, c *cloudConfig) (map[string]string, error) {
	return p.hooks.headers(env, c)
}

func (p *CloudBrowserProvider) releaseBody(*cloudConfig) any { return nil }

// Transport every cloud call shares, the platform trust store and the environment proxy as requests_verify gives it
var cloudTransport = sync.OnceValue(func() *http.Transport {
	return &http.Transport{Proxy: http.ProxyFromEnvironment, TLSClientConfig: provider.PlatformTLSConfig(), ForceAttemptHTTP2: true}
})

// Sends one requests call with an optional json body serialised as requests does
func cloudRequest(method, url string, headers map[string]string, body any, timeout int) (*pyrequests.Response, error) {
	var payload []byte
	if body != nil {
		text, err := pyjson.Dumps(body, pyjson.AllowNaN(false))
		if err != nil {
			return nil, err
		}
		payload = []byte(text)
	}
	req, err := http.NewRequest(method, url, bytes.NewReader(payload))
	if err != nil {
		return nil, &pyrequests.Error{Class: "InvalidURL", Text: err.Error(), Err: err}
	}
	if body == nil {
		req.Body, req.ContentLength = http.NoBody, 0
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	if body != nil && req.Header.Get("Content-Type") == "" {
		req.Header.Set("Content-Type", "application/json")
	}
	return pyrequests.Send(context.Background(), cloudTransport(), pyrequests.Seconds(timeout), req)
}

// Release request of a session by the vendor's method and path
func (p *CloudBrowserProvider) release(env *tools.Env, c *cloudConfig, sessionID string, timeout int) (*pyrequests.Response, error) {
	headers, err := p.hooks.releaseHeaders(env, c)
	if err != nil {
		return nil, err
	}
	url := c.BaseURL + strings.ReplaceAll(p.releasePath, "{session_id}", sessionID)
	return cloudRequest(strings.ToUpper(p.releaseMethod), url, headers, p.hooks.releaseBody(c), timeout)
}

// Unique session label for logs
func cloudSessionName(taskID string) string {
	buf := make([]byte, 4)
	rand.Read(buf)
	return "icebath_" + taskID + "_" + hex.EncodeToString(buf)
}

// Create request, a network failure a RuntimeError unless the caller retries on the raw exception
func (p *CloudBrowserProvider) postCreate(url string, headers map[string]string, payload any, wrapErrors bool) (*pyrequests.Response, error) {
	resp, err := cloudRequest(http.MethodPost, url, headers, payload, 30)
	var reqErr *pyrequests.Error
	if err != nil && errors.As(err, &reqErr) && wrapErrors {
		return nil, runtimeError(fmt.Sprintf("%s API connection failed: %s", p.label, err.Error()))
	}
	return resp, err
}

func responseOK(r *pyrequests.Response) bool { return r.Status < 400 }

func (p *CloudBrowserProvider) checkCreated(r *pyrequests.Response) error {
	if !responseOK(r) {
		return runtimeError(fmt.Sprintf("Failed to create %s%s session: %d %s", p.label, p.createLabelSuffix, r.Status, string(r.Body)))
	}
	return nil
}

// Releases a session, false on missing credentials or a failed release, logging each outcome
func (p *CloudBrowserProvider) CloseSession(env *tools.Env, sessionID string) bool {
	log := p.log(env)
	c, err := p.hooks.config(env)
	if err != nil {
		if !isValueError(err) {
			panic(err)
		}
		log.Warn(fmt.Sprintf("Cannot close %s session %s — missing credentials", p.label, sessionID))
		return false
	}
	resp, err := p.release(env, c, sessionID, 10)
	if err != nil {
		log.Error(fmt.Sprintf("Exception closing %s session %s: %s", p.label, sessionID, err.Error()))
		return false
	}
	if cloudCloseOK[resp.Status] {
		log.Debug(fmt.Sprintf("Successfully closed %s session %s", p.label, sessionID))
		return true
	}
	format := p.closeFailFmt
	if format == "" {
		format = "Failed to close " + p.label + " session %s: HTTP %s - %s"
	}
	log.Warn(fmt.Sprintf(format, sessionID, strconv.Itoa(resp.Status), firstRunes(string(resp.Body), 200)))
	return false
}

// Teardown from exit paths that tolerates missing credentials and network errors
func (p *CloudBrowserProvider) EmergencyCleanup(env *tools.Env, sessionID string) {
	log := p.log(env)
	c, err := p.hooks.configOrNone(env)
	if err != nil {
		panic(err)
	}
	if c == nil {
		log.Warn(fmt.Sprintf("Cannot emergency-cleanup %s session %s — missing credentials", p.label, sessionID))
		return
	}
	if _, err := p.release(env, c, sessionID, 5); err != nil {
		log.Debug(fmt.Sprintf("Emergency cleanup failed for %s session %s: %s", p.label, sessionID, err.Error()))
	}
}

// Setup picker row, nil hiding the provider from the picker
func (p *CloudBrowserProvider) GetSetupSchema() map[string]any {
	if p.setupTag == "" {
		return nil
	}
	vars := make([]any, 0, len(p.setupEnvVars))
	for _, v := range p.setupEnvVars {
		row := map[string]any{"key": v[0], "prompt": v[1]}
		if v[2] != "" {
			row["url"] = v[2]
		}
		vars = append(vars, row)
	}
	return map[string]any{"name": p.label, "badge": "paid", "tag": p.setupTag, "env_vars": vars}
}

func firstRunes(s string, n int) string {
	r := []rune(s)
	if len(r) > n {
		r = r[:n]
	}
	return string(r)
}

// Response body parsed as requests' json() parses it
func responseJSON(r *pyrequests.Response) (any, error) {
	v, err := pyjson.Loads(string(r.Body))
	if err != nil {
		return nil, &pyrequests.Error{Class: "JSONDecodeError", Text: err.Error(), Err: err}
	}
	return v, nil
}

// dict.get of a decoded json value, ok false when the value is not an object
func jsonGet(v any, key string) (any, bool, bool) {
	switch o := v.(type) {
	case map[string]any:
		got, ok := o[key]
		return got, ok, true
	case *pyjson.Object:
		got, ok := o.Get(key)
		return got, ok, true
	}
	return nil, false, false
}

// Subscript of a decoded json value with the KeyError or TypeError Python raises
func jsonItem(v any, key string) (any, error) {
	got, ok, isObject := jsonGet(v, key)
	if !isObject {
		return nil, &cloudError{Class: "TypeError", Msg: subscriptError(v)}
	}
	if !ok {
		return nil, &cloudError{Class: "KeyError", Msg: pyrepr.Repr(key)}
	}
	return got, nil
}

// TypeError text of a str subscript on a non object json value
func subscriptError(v any) string {
	switch v.(type) {
	case []any:
		return "list indices must be integers or slices, not str"
	case string:
		return "string indices must be integers, not 'str'"
	case bool:
		return "'bool' object is not subscriptable"
	case float64:
		return "'float' object is not subscriptable"
	case nil:
		return "'NoneType' object is not subscriptable"
	}
	return "'int' object is not subscriptable"
}
