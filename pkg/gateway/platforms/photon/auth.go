package photon

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"io/fs"
	"math"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/google/uuid"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/gateway"
	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/provider"
	anyutil "github.com/nickheyer/icebath/pkg/utils/any"
	errorutil "github.com/nickheyer/icebath/pkg/utils/error"
	httputil "github.com/nickheyer/icebath/pkg/utils/http"
	jsonutil "github.com/nickheyer/icebath/pkg/utils/json"
	textutil "github.com/nickheyer/icebath/pkg/utils/text"
)

// Device flow token the dashboard API rejects
type PhotonDashboardAuthError struct {
	Msg   string
	Cause error
}

func (e *PhotonDashboardAuthError) Error() string     { return e.Msg }
func (e *PhotonDashboardAuthError) ErrorKind() string { return "PhotonDashboardAuthError" }
func (e *PhotonDashboardAuthError) Unwrap() error     { return e.Cause }

// Photon endpoints, the published CLI device client and the RFC 8628 polling defaults
const (
	DefaultClientID      = "photon-cli"
	DefaultScope         = "openid profile email"
	DefaultDashboardHost = "https://app.photon.codes"
	DefaultSpectrumHost  = "https://spectrum.photon.codes"
	DefaultProjectName   = "Icebath Agent"
	DefaultPollInterval  = 5
	DefaultPollTimeout   = 1800
	DefaultLocation      = "United States"
	httpTimeout          = 30 * time.Second
	statusTimeout        = 5 * time.Second
)

// E.164 phone number, a plus sign then seven to fifteen digits of which the first is not zero
var E164RE = regexp.MustCompile(`^\+[1-9]\p{Nd}{6,14}$`)

// Python exception class name an error is reported under
type errorValue struct {
	class string
	msg   string
	cause error
}

func (e *errorValue) Error() string     { return e.msg }
func (e *errorValue) ErrorKind() string { return e.class }
func (e *errorValue) Unwrap() error     { return e.cause }

// httpx.RequestError family raised when a request never produced a response
type requestError struct {
	class string
	msg   string
	cause error
}

func (e *requestError) Error() string     { return e.msg }
func (e *requestError) ErrorKind() string { return e.class }
func (e *requestError) Unwrap() error     { return e.cause }

func runtimeError(format string, args ...any) error {
	return &errorValue{class: "RuntimeError", msg: fmt.Sprintf(format, args...)}
}

// True for a cancelled context, the KeyboardInterrupt no except Exception clause swallows
func isBaseException(err error) bool {
	return errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded)
}

// True when err is an httpx.HTTPError, RuntimeError or ValueError, the classes the opt in probes catch
func isProbeFailure(err error) bool {
	switch e := err.(type) {
	case *PhotonDashboardAuthError, *requestError:
		return true
	case *errorValue:
		return slices.Contains([]string{"RuntimeError", "ValueError", "JSONDecodeError", "UnicodeDecodeError", "HTTPStatusError"}, e.class)
	}
	return false
}

// Error for a decoded JSON value of another kind than want, under the AttributeError class the probes let through
func kindError(want string, v any) error {
	return &errorValue{class: "AttributeError", msg: fmt.Sprintf("expected %s, got %s", want, anyutil.Kind(v))}
}

// Value of a key of a decoded JSON object, def when the key is absent, a kind error for any other value
func getDefault(v any, key string, def any) (any, error) {
	o, ok := v.(*jsonutil.Object)
	if !ok || o == nil {
		return nil, kindError("an object", v)
	}
	if val, found := o.Get(key); found {
		return val, nil
	}
	return def, nil
}

func lookup(v any, key string) (any, error) { return getDefault(v, key, nil) }

// Subscript of a decoded JSON value by a string key with the KeyError or TypeError Python raises
func subscript(v any, key string) (any, error) {
	switch t := v.(type) {
	case *jsonutil.Object:
		if val, found := t.Get(key); found {
			return val, nil
		}
		return nil, &errorValue{class: "KeyError", msg: strconv.Quote(key)}
	case []any:
		return nil, &errorValue{class: "TypeError", msg: "list indices must be integers or slices, not str"}
	case string:
		return nil, &errorValue{class: "TypeError", msg: "string indices must be integers, not 'str'"}
	}
	return nil, &errorValue{class: "TypeError", msg: fmt.Sprintf("'%s' object is not subscriptable", anyutil.Kind(v))}
}

// Item assignment by a string key with the TypeError a non dict raises
func setItem(v any, key string, val any) error {
	switch t := v.(type) {
	case *jsonutil.Object:
		t.Set(key, val)
		return nil
	case []any:
		return &errorValue{class: "TypeError", msg: "list indices must be integers or slices, not str"}
	}
	return &errorValue{class: "TypeError", msg: fmt.Sprintf("'%s' object does not support item assignment", anyutil.Kind(v))}
}

// Python in operator for a string needle over a decoded JSON container
func contains(container any, needle string) (bool, error) {
	switch t := container.(type) {
	case *jsonutil.Object:
		_, found := t.Get(needle)
		return found, nil
	case []any:
		return slices.ContainsFunc(t, func(v any) bool { return anyutil.Equal(v, needle) }), nil
	case string:
		return strings.Contains(t, needle), nil
	}
	return false, &errorValue{class: "TypeError", msg: fmt.Sprintf("argument of type '%s' is not iterable", anyutil.Kind(container))}
}

// Decoded JSON value as a string, a kind error for any other value
func stringValue(v any) (string, error) {
	s, ok := v.(string)
	if !ok {
		return "", kindError("a string", v)
	}
	return s, nil
}

// First n code points of s like a Python slice
func cutRunes(s string, n int) string {
	if utf8.RuneCountInString(s) <= n {
		return s
	}
	return string([]rune(s)[:n])
}

func strOrEmpty(v any) string {
	if anyutil.IsTrue(v) {
		return anyutil.String(v)
	}
	return ""
}

// Secret read through the scope aware gateway helper so a secondary profile never borrows the default env
var getScopedSecret = gateway.GetScopedSecret

// The active profile's auth.json shared with the rest of icebath
func authJSONPath(ctx context.Context) string { return home.AuthPath(home.Dir(ctx)) }

// Decoded auth.json object, an empty object when missing, unreadable or empty, an error naming the file for bytes that are not UTF-8 or a value that is not an object
func loadAuth(ctx context.Context) (*jsonutil.Object, error) {
	path := authJSONPath(ctx)
	if _, err := os.Stat(path); err != nil {
		if errors.Is(err, fs.ErrNotExist) || errors.Is(err, syscall.ENOTDIR) || errors.Is(err, syscall.ELOOP) {
			return jsonutil.NewObject(), nil
		}
		return nil, err
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		pkgLog().Warn(fmt.Sprintf("photon: could not read %s: %s", path, err.Error()))
		return jsonutil.NewObject(), nil
	}
	raw = textutil.TrimBOM(raw)
	if err := textutil.ValidateUTF8(raw); err != nil {
		return nil, &errorValue{class: "UnicodeDecodeError", msg: path + ": " + err.Error(), cause: err}
	}
	data, err := jsonutil.Decode(raw)
	if err != nil {
		pkgLog().Warn(fmt.Sprintf("photon: could not read %s: %s", path, err))
		return jsonutil.NewObject(), nil
	}
	if !anyutil.IsTrue(data) {
		return jsonutil.NewObject(), nil
	}
	o, ok := data.(*jsonutil.Object)
	if !ok {
		return nil, &errorValue{class: "AttributeError", msg: fmt.Sprintf("%s: expected a JSON object, got %s", path, anyutil.Kind(data))}
	}
	return o, nil
}

// Writes auth.json through a 0600 O_EXCL temp file, fsync and rename so the token is never world readable
func saveAuth(ctx context.Context, data any) error {
	path := authJSONPath(ctx)
	if err := os.MkdirAll(filepath.Dir(path), 0o777); err != nil {
		return err
	}
	text, err := jsonutil.Encode(data, jsonutil.Indent(2), jsonutil.SortKeys(), jsonutil.ASCII())
	if err != nil {
		return err
	}
	tmp := fmt.Sprintf("%s.tmp.%d.%s", path, os.Getpid(), strings.ReplaceAll(uuid.NewString(), "-", ""))
	f, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	fail := func(err error) error {
		_ = f.Close()
		_ = os.Remove(tmp)
		return err
	}
	if _, err := f.WriteString(text); err != nil {
		return fail(err)
	}
	if err := f.Sync(); err != nil {
		return fail(err)
	}
	if err := f.Close(); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	return nil
}

// First entry of credential_pool key when it is a non empty list, else nil
func poolFirst(auth any, key string) (any, error) {
	cp, err := getDefault(auth, "credential_pool", jsonutil.NewObject())
	if err != nil {
		return nil, err
	}
	pool, err := lookup(cp, key)
	if err != nil {
		return nil, err
	}
	if list, ok := pool.([]any); ok && len(list) > 0 {
		return list[0], nil
	}
	return nil, nil
}

// Replaces credential_pool key with the one record under the cross process auth store lock
func storePoolRecord(ctx context.Context, key string, record *jsonutil.Object) error {
	unlock, err := provider.NewStore(home.Dir(ctx)).Lock(provider.AuthLockTimeout)
	if err != nil {
		return err
	}
	defer unlock()
	auth, err := loadAuth(ctx)
	if err != nil {
		return err
	}
	cp, found := auth.Get("credential_pool")
	if !found {
		cp = jsonutil.NewObject()
		auth.Set("credential_pool", cp)
	}
	if err := setItem(cp, key, []any{record}); err != nil {
		return err
	}
	return saveAuth(ctx, auth)
}

// Device flow bearer token stored by the login, empty when none
func LoadPhotonToken(ctx context.Context) (string, error) {
	auth, err := loadAuth(ctx)
	if err != nil {
		return "", err
	}
	entry, err := poolFirst(auth, "photon")
	if err != nil {
		return "", err
	}
	entry = anyutil.Or(entry, jsonutil.NewObject())
	providers, err := getDefault(auth, "providers", jsonutil.NewObject())
	if err != nil {
		return "", err
	}
	legacy, err := getDefault(providers, "photon", jsonutil.NewObject())
	if err != nil {
		return "", err
	}
	token, err := lookup(entry, "access_token")
	if err == nil && !anyutil.IsTrue(token) {
		token, err = lookup(entry, "token")
	}
	if err == nil && !anyutil.IsTrue(token) {
		token, err = lookup(legacy, "access_token")
	}
	if err != nil {
		return "", err
	}
	return strOrEmpty(token), nil
}

// Persists a dashboard bearer token under credential_pool photon
func StorePhotonToken(ctx context.Context, token string) error {
	return storePoolRecord(ctx, "photon", jsonutil.ObjectOf("access_token", token, "issued_at", time.Now().Unix()))
}

// Removes every stored Photon dashboard token before re-authentication
func ClearPhotonToken(ctx context.Context) error {
	auth, err := loadAuth(ctx)
	if err != nil {
		return err
	}
	pool, err := getDefault(auth, "credential_pool", jsonutil.NewObject())
	if err != nil {
		return err
	}
	photon, err := getDefault(pool, "photon", []any{})
	if err != nil {
		return err
	}
	if list, ok := photon.([]any); ok && len(list) > 0 {
		if err := setItem(pool, "photon", []any{}); err != nil {
			return err
		}
		if err := saveAuth(ctx, auth); err != nil {
			return err
		}
	}
	providers, err := getDefault(auth, "providers", jsonutil.NewObject())
	if err != nil {
		return err
	}
	present, err := contains(providers, "photon")
	if err != nil || !present {
		return err
	}
	if err := setItem(providers, "photon", jsonutil.NewObject()); err != nil {
		return err
	}
	return saveAuth(ctx, auth)
}

// False for a token the dashboard definitively rejects, transient failures counting as valid
func CheckPhotonTokenValid(ctx context.Context, token string) (bool, error) {
	if token == "" {
		return false, nil
	}
	_, err := ValidatePhotonToken(ctx, token)
	if err == nil {
		return true, nil
	}
	if _, rejected := err.(*PhotonDashboardAuthError); rejected {
		return false, nil
	}
	if isBaseException(err) {
		return false, err
	}
	return true, nil
}

// Raw spectrum project id and secret, the scoped env first and auth.json for offline status
func loadProjectCredentialsRaw(ctx context.Context) (any, any, error) {
	envID, err := getScopedSecret(ctx, "PHOTON_PROJECT_ID", "", false)
	if err != nil {
		return nil, nil, err
	}
	envSec, err := getScopedSecret(ctx, "PHOTON_PROJECT_SECRET", "", false)
	if err != nil {
		return nil, nil, err
	}
	if envID != "" && envSec != "" {
		return envID, envSec, nil
	}
	auth, err := loadAuth(ctx)
	if err != nil {
		return nil, nil, err
	}
	entry, err := poolFirst(auth, "photon_project")
	if err != nil || entry == nil {
		return envID, envSec, err
	}
	var id, sec any = envID, envSec
	if envID == "" {
		if id, err = lookup(entry, "spectrum_project_id"); err != nil {
			return nil, nil, err
		}
		if !anyutil.IsTrue(id) {
			if id, err = lookup(entry, "project_id"); err != nil {
				return nil, nil, err
			}
		}
	}
	if envSec == "" {
		if sec, err = lookup(entry, "project_secret"); err != nil {
			return nil, nil, err
		}
	}
	return id, sec, nil
}

// Runtime SDK credentials (spectrum project id, project secret), empty strings where unset
func LoadProjectCredentials(ctx context.Context) (string, string, error) {
	id, sec, err := loadProjectCredentialsRaw(ctx)
	return strOrEmpty(id), strOrEmpty(sec), err
}

// Raw management project id preferring the spectrum id over the diverged legacy dashboard id
func loadDashboardProjectIDRaw(ctx context.Context) (any, error) {
	envID, err := getScopedSecret(ctx, "PHOTON_DASHBOARD_PROJECT_ID", "", false)
	if err != nil || envID != "" {
		return envID, err
	}
	auth, err := loadAuth(ctx)
	if err != nil {
		return nil, err
	}
	entry, err := poolFirst(auth, "photon_project")
	if err != nil {
		return nil, err
	}
	entry = anyutil.Or(entry, jsonutil.NewObject())
	var last any
	for _, key := range []string{"spectrum_project_id", "dashboard_project_id", "project_id"} {
		if last, err = lookup(entry, key); err != nil || anyutil.IsTrue(last) {
			return last, err
		}
	}
	return last, nil
}

// Project id for management API calls, empty when none is stored
func LoadDashboardProjectID(ctx context.Context) (string, error) {
	id, err := loadDashboardProjectIDRaw(ctx)
	return strOrEmpty(id), err
}

// Persists project credentials to the .env for the runtime and to auth.json for status
func StoreProjectCredentials(ctx context.Context, spectrumProjectID, projectSecret, dashboardProjectID, name string) error {
	record := jsonutil.ObjectOf("spectrum_project_id", spectrumProjectID, "project_secret", projectSecret, "issued_at", time.Now().Unix())
	if dashboardProjectID != "" {
		record.Set("dashboard_project_id", dashboardProjectID)
	}
	if name != "" {
		record.Set("name", name)
	}
	if err := storePoolRecord(ctx, "photon_project", record); err != nil {
		return err
	}
	persistRuntimeEnv(ctx, spectrumProjectID, projectSecret)
	return nil
}

// Persists the non secret user numbers for offline status, nothing when both numbers are empty
func StoreUserNumbers(ctx context.Context, phoneNumber, assignedPhoneNumber, userID string, dashboardProjectID any) error {
	if phoneNumber == "" && assignedPhoneNumber == "" {
		return nil
	}
	record := jsonutil.ObjectOf("issued_at", time.Now().Unix())
	for _, kv := range [][2]any{{"phone_number", phoneNumber}, {"assigned_phone_number", assignedPhoneNumber}, {"user_id", userID}, {"dashboard_project_id", dashboardProjectID}} {
		if anyutil.IsTrue(kv[1]) {
			record.Set(kv[0].(string), kv[1])
		}
	}
	return storePoolRecord(ctx, "photon_user", record)
}

// Writes the SDK credentials to the profile .env, a failed write logged and dropped
var persistRuntimeEnv = func(ctx context.Context, spectrumProjectID, projectSecret string) {
	h := home.Dir(ctx)
	if err := config.SaveEnvValueContext(ctx, h, "PHOTON_PROJECT_ID", spectrumProjectID); err != nil {
		pkgLog().Warn(fmt.Sprintf("photon: could not write project creds to .env: %s", err))
		return
	}
	if err := config.SaveEnvValueContext(ctx, h, "PHOTON_PROJECT_SECRET", projectSecret); err != nil {
		pkgLog().Warn(fmt.Sprintf("photon: could not write project creds to .env: %s", err))
	}
}

func dashboardHost() string {
	return strings.TrimRight(anyutil.Or(os.Getenv("PHOTON_DASHBOARD_HOST"), DefaultDashboardHost).(string), "/")
}

func spectrumHost() string {
	return strings.TrimRight(anyutil.Or(os.Getenv("PHOTON_SPECTRUM_HOST"), DefaultSpectrumHost).(string), "/")
}

func bearer(token string) map[string]string {
	return map[string]string{"Authorization": "Bearer " + token}
}

func basic(projectID, projectSecret string) map[string]string {
	return map[string]string{"Authorization": "Basic " + base64.StdEncoding.EncodeToString([]byte(projectID+":"+projectSecret))}
}

// Response surface the module reads, status, JSON body, text, headers and raise_for_status
type photonResponse interface {
	StatusCode() int
	JSON() (any, error)
	Text() string
	Header() http.Header
	RaiseForStatus() error
}

type liveResponse struct{ r *httputil.Response }

func (l liveResponse) StatusCode() int     { return l.r.StatusCode }
func (l liveResponse) Text() string        { return l.r.Text() }
func (l liveResponse) Header() http.Header { return l.r.Header }

// Decoded JSON body, a JSONDecodeError kind failure when it does not parse; invalid UTF-8 inside strings decodes as U+FFFD rather than failing
func (l liveResponse) JSON() (any, error) {
	v, err := l.r.JSON()
	if err == nil {
		return v, nil
	}
	return nil, &errorValue{class: "JSONDecodeError", msg: err.Error(), cause: err}
}

// HTTPStatusError for any non 2xx status like httpx raise_for_status
func (l liveResponse) RaiseForStatus() error {
	if l.r.StatusCode >= 200 && l.r.StatusCode < 300 {
		return nil
	}
	status := &httputil.StatusError{StatusCode: l.r.StatusCode, URL: l.r.URL, Location: l.r.Header.Get("Location")}
	return &errorValue{class: "HTTPStatusError", msg: status.Error(), cause: status}
}

// Module level httpx get and post, swappable so tests answer without a network
var (
	httpGet = func(ctx context.Context, rawURL string, headers map[string]string, timeout time.Duration) (photonResponse, error) {
		return liveRequest(ctx, http.MethodGet, rawURL, nil, headers, timeout)
	}
	httpPost = func(ctx context.Context, rawURL string, body any, headers map[string]string, timeout time.Duration) (photonResponse, error) {
		return liveRequest(ctx, http.MethodPost, rawURL, body, headers, timeout)
	}
)

// One httpx module call without redirects, transport failures raised as the httpx.RequestError class
func liveRequest(ctx context.Context, method, rawURL string, body any, headers map[string]string, timeout time.Duration) (photonResponse, error) {
	u, err := url.Parse(rawURL)
	if err != nil {
		return nil, &errorValue{class: "InvalidURL", msg: err.Error(), cause: err}
	}
	switch u.Scheme {
	case "http", "https":
	case "":
		return nil, &requestError{class: "UnsupportedProtocol", msg: "Request URL is missing an 'http://' or 'https://' protocol."}
	default:
		return nil, &requestError{class: "UnsupportedProtocol", msg: fmt.Sprintf("Request URL has an unsupported protocol '%s://'.", u.Scheme)}
	}
	var payload *strings.Reader
	if body != nil {
		text, err := jsonutil.Encode(body)
		if err != nil {
			return nil, &errorValue{class: "ValueError", msg: err.Error(), cause: err}
		}
		payload = strings.NewReader(text)
	}
	var req *http.Request
	if payload != nil {
		req, err = http.NewRequestWithContext(ctx, method, rawURL, payload)
	} else {
		req, err = http.NewRequestWithContext(ctx, method, rawURL, nil)
	}
	if err != nil {
		return nil, &errorValue{class: "InvalidURL", msg: err.Error(), cause: err}
	}
	if payload != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := httputil.Do(httputil.Client(false), req, httputil.EachPhase(timeout))
	if err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, &requestError{class: requestErrorClass(err), msg: err.Error(), cause: err}
	}
	return liveResponse{resp}, nil
}

// httpx exception class of a failed request, the timeout phase naming its Timeout subclass
func requestErrorClass(err error) string {
	var te *httputil.TimeoutError
	if errors.As(err, &te) {
		if class, ok := map[string]string{"connect": "ConnectTimeout", "read": "ReadTimeout", "write": "WriteTimeout", "pool": "PoolTimeout"}[te.Phase]; ok {
			return class
		}
		return "ReadTimeout"
	}
	if errors.Is(err, httputil.ErrTooManyRedirects) {
		return "TooManyRedirects"
	}
	return "ConnectError"
}

func dashboardGet(ctx context.Context, path, token string) (photonResponse, error) {
	return httpGet(ctx, dashboardHost()+path, bearer(token), httpTimeout)
}

// Posts to the dashboard, raising for HTTP errors, and returns the decoded body or an empty dict
func dashboardPost(ctx context.Context, path string, body any, token string) (any, error) {
	resp, err := httpPost(ctx, dashboardHost()+path, body, bearer(token), httpTimeout)
	if err != nil {
		return nil, err
	}
	if err := resp.RaiseForStatus(); err != nil {
		return nil, err
	}
	data, err := resp.JSON()
	if err != nil {
		return nil, err
	}
	return anyutil.Or(data, jsonutil.NewObject()), nil
}

// Error detail of a failed response, the error message or detail key, the sorted JSON or the text
func responseErrorDetail(resp photonResponse) string {
	data, err := resp.JSON()
	if o, ok := data.(*jsonutil.Object); err == nil && ok && o != nil {
		for _, key := range []string{"error", "message", "detail"} {
			if v, _ := o.Get(key); anyutil.IsTrue(v) {
				return anyutil.String(v)
			}
		}
		text, _ := jsonutil.Encode(o, jsonutil.SortKeys(), jsonutil.Spaced(), jsonutil.ASCII())
		return cutRunes(text, 500)
	}
	if text := resp.Text(); text != "" {
		return cutRunes(text, 500)
	}
	return "no response body"
}

func raiseForStatus(resp photonResponse, action string) error {
	if status := resp.StatusCode(); status >= 400 {
		return runtimeError("Photon %s failed: HTTP %d: %s", action, status, responseErrorDetail(resp))
	}
	return nil
}

// Runs a callback with every panic it raises suppressed
func safe(fn func()) {
	defer func() { _ = recover() }()
	fn()
}

// Device and user codes of an RFC 8628 device authorization
type DeviceCode struct {
	DeviceCode              any
	UserCode                any
	VerificationURI         any
	VerificationURIComplete any
	ExpiresIn               int
	Interval                int
}

// Token like value extracted from the device token response with the place it came from
type deviceTokenCandidate struct {
	Source string
	Token  string
}

// int of a device response field with its default, the OverflowError for a value past int64
func deviceInt(data *jsonutil.Object, key string, def int) (int, error) {
	v, _ := data.Get(key)
	n, err := anyutil.ToInt(anyutil.Or(v, def))
	if err != nil {
		return 0, err
	}
	return int(n), nil
}

// Posts the device code request and returns the device and user codes
func RequestDeviceCode(ctx context.Context, clientID, scope string) (DeviceCode, error) {
	body := jsonutil.ObjectOf("client_id", clientID)
	if scope != "" {
		body.Set("scope", scope)
	}
	resp, err := httpPost(ctx, dashboardHost()+"/api/auth/device/code", body, nil, httpTimeout)
	if err != nil {
		return DeviceCode{}, err
	}
	if err := resp.RaiseForStatus(); err != nil {
		return DeviceCode{}, err
	}
	data, err := resp.JSON()
	if err != nil {
		return DeviceCode{}, err
	}
	var code DeviceCode
	for _, f := range []struct {
		key string
		dst *any
	}{{"device_code", &code.DeviceCode}, {"user_code", &code.UserCode}, {"verification_uri", &code.VerificationURI}} {
		if *f.dst, err = subscript(data, f.key); err != nil {
			return DeviceCode{}, err
		}
	}
	o := data.(*jsonutil.Object)
	code.VerificationURIComplete, _ = o.Get("verification_uri_complete")
	if code.ExpiresIn, err = deviceInt(o, "expires_in", DefaultPollTimeout); err != nil {
		return DeviceCode{}, err
	}
	if code.Interval, err = deviceInt(o, "interval", DefaultPollInterval); err != nil {
		return DeviceCode{}, err
	}
	return code, nil
}

// Sleep of the device token poll, swappable so tests run without waiting
var sleepFn = func(ctx context.Context, seconds int) error {
	if seconds < 0 {
		return &errorValue{class: "ValueError", msg: "sleep length must be non-negative"}
	}
	t := time.NewTimer(time.Duration(seconds) * time.Second)
	defer t.Stop()
	select {
	case <-t.C:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// Polls the device token endpoint until approval, sleeping first and backing off on slow_down and 429
func PollForToken(ctx context.Context, code DeviceCode, clientID string, timeout int, interval *int, onPending func()) (string, error) {
	tokenURL := dashboardHost() + "/api/auth/device/token"
	budget := anyutil.Or(timeout, code.ExpiresIn, DefaultPollTimeout).(int)
	deadline := float64(time.Now().UnixNano())/1e9 + float64(budget)
	sleep := anyutil.Or(code.Interval, DefaultPollInterval).(int)
	if interval != nil {
		sleep = *interval
	}
	grant := jsonutil.ObjectOf("grant_type", "urn:ietf:params:oauth:grant-type:device_code", "device_code", code.DeviceCode, "client_id", clientID)
	pending := func() {
		if onPending != nil {
			safe(onPending)
		}
	}
	for float64(time.Now().UnixNano())/1e9 < deadline {
		if err := sleepFn(ctx, sleep); err != nil {
			return "", err
		}
		resp, err := httpPost(ctx, tokenURL, grant, nil, httpTimeout)
		if err != nil {
			if _, transport := err.(*requestError); transport {
				pkgLog().Warn(fmt.Sprintf("photon: device-token poll failed: %s", err))
				continue
			}
			return "", err
		}
		switch resp.StatusCode() {
		case 200:
			body, err := resp.JSON()
			if err != nil {
				body = nil
			}
			o, ok := anyutil.Or(body, jsonutil.NewObject()).(*jsonutil.Object)
			if !ok {
				o = jsonutil.NewObject()
			}
			candidates := deviceResponseTokenCandidates(o, resp.Header())
			if len(candidates) == 0 {
				return "", runtimeError("Photon returned 200 but no token candidate in the device-token response " +
					"(expected access_token, data.access_token, accessToken, or set-auth-token).")
			}
			return candidates[0].Token, nil
		case 429:
			sleep += 10
			pending()
			continue
		case 400:
			var body any = jsonutil.NewObject()
			if v, err := resp.JSON(); err == nil {
				body = anyutil.Or(v, jsonutil.NewObject())
			} else if errorutil.Kind(err) != "JSONDecodeError" {
				return "", err
			}
			e, err := lookup(body, "error")
			if err == nil && !anyutil.IsTrue(e) {
				e, err = lookup(body, "message")
			}
			if err != nil {
				return "", err
			}
			e = anyutil.Or(e, "")
			switch s, _ := e.(string); s {
			case "authorization_pending", "slow_down":
				if s == "slow_down" {
					sleep += 5
				}
				pending()
				continue
			case "expired_token", "access_denied":
				return "", runtimeError("Photon login failed: %s", s)
			}
			detail := resp.Text()
			if anyutil.IsTrue(e) {
				detail = anyutil.String(e)
			}
			return "", runtimeError("Photon device token error: %s", detail)
		}
		pkgLog().Warn(fmt.Sprintf("photon: device-token unexpected status %d: %s", resp.StatusCode(), cutRunes(resp.Text(), 200)))
	}
	return "", &errorValue{class: "TimeoutError", msg: "Photon device login timed out"}
}

// De-duplicated token candidates from every body key and header shape Photon has used
func deviceResponseTokenCandidates(body *jsonutil.Object, headers http.Header) []deviceTokenCandidate {
	nested := func(key string) *jsonutil.Object {
		if v, _ := body.Get(key); v != nil {
			if o, ok := v.(*jsonutil.Object); ok {
				return o
			}
		}
		return jsonutil.NewObject()
	}
	get := func(o *jsonutil.Object, key string) any {
		v, _ := o.Get(key)
		return v
	}
	session, data := nested("session"), nested("data")
	raw := []struct {
		source string
		value  any
	}{
		{"access_token", get(body, "access_token")}, {"accessToken", get(body, "accessToken")},
		{"session.access_token", get(session, "access_token")},
		{"data.access_token", get(data, "access_token")}, {"data.accessToken", get(data, "accessToken")},
	}
	if hv := headerValue(headers, "set-auth-token"); hv != "" {
		raw = append(raw, struct {
			source string
			value  any
		}{"set-auth-token", hv})
	}
	var candidates []deviceTokenCandidate
	seen := map[string]bool{}
	for _, r := range raw {
		if token := cleanBearerToken(r.value); token != "" && !seen[token] {
			seen[token] = true
			candidates = append(candidates, deviceTokenCandidate{Source: r.source, Token: token})
		}
	}
	return candidates
}

// Stripped token text without a leading Bearer scheme, empty for a non string
func cleanBearerToken(value any) string {
	s, ok := value.(string)
	if !ok {
		return ""
	}
	token := strings.TrimSpace(s)
	if strings.HasPrefix(strings.ToLower(token), "bearer ") {
		runes := []rune(token)
		if len(runes) < 7 {
			return ""
		}
		return strings.TrimSpace(string(runes[7:]))
	}
	return token
}

// Header value by name, case insensitive like httpx Headers and again over the raw keys
func headerValue(headers http.Header, name string) string {
	if len(headers) == 0 {
		return ""
	}
	if v := headers.Get(name); v != "" {
		return v
	}
	keys := make([]string, 0, len(headers))
	for k := range headers {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	for _, k := range keys {
		if v := strings.Join(headers[k], ", "); strings.ToLower(k) == strings.ToLower(name) && v != "" {
			return v
		}
	}
	return ""
}

// Verifies a device token against the session lookup and the project API, returning the session user
func ValidatePhotonToken(ctx context.Context, token string) (*jsonutil.Object, error) {
	get := func(path, rejected string) (photonResponse, error) {
		resp, err := dashboardGet(ctx, path, token)
		if err != nil {
			return nil, err
		}
		if s := resp.StatusCode(); s == 401 || s == 403 {
			return nil, &PhotonDashboardAuthError{Msg: rejected}
		}
		if err := resp.RaiseForStatus(); err != nil {
			return nil, err
		}
		return resp, nil
	}
	resp, err := get("/api/auth/get-session", "Photon issued a device token, but the dashboard session lookup rejected it.")
	if err != nil {
		return nil, err
	}
	data, err := resp.JSON()
	if err != nil {
		return nil, err
	}
	var user any
	if o, ok := data.(*jsonutil.Object); ok && o != nil {
		user, _ = o.Get("user")
	}
	u, ok := user.(*jsonutil.Object)
	if !ok || u == nil || u.Len() == 0 {
		return nil, &PhotonDashboardAuthError{Msg: "Photon issued a device token, but the dashboard session lookup did not recognize it."}
	}
	if _, err := get("/api/projects/", "Photon device token was accepted for the session lookup but rejected by the project API."); err != nil {
		return nil, err
	}
	return u, nil
}

// First candidate token that passes dashboard validation, naming every tried source on rejection
func validatedDashboardToken(ctx context.Context, candidates []deviceTokenCandidate) (string, error) {
	if len(candidates) == 0 {
		return "", runtimeError("Photon returned 200 but no token candidate in the device-token response.")
	}
	var dashboardErr *PhotonDashboardAuthError
	var lastErr error
	for _, c := range candidates {
		_, err := ValidatePhotonToken(ctx, c.Token)
		if err == nil {
			return c.Token, nil
		}
		if isBaseException(err) {
			return "", err
		}
		lastErr = err
		if de, ok := err.(*PhotonDashboardAuthError); ok {
			dashboardErr = de
		}
	}
	if dashboardErr != nil {
		sources := make([]string, len(candidates))
		for i, c := range candidates {
			sources[i] = c.Source
		}
		return "", &PhotonDashboardAuthError{
			Msg:   fmt.Sprintf("%s Device login returned no project-valid dashboard token (tried: %s).", dashboardErr.Msg, strings.Join(sources, ", ")),
			Cause: dashboardErr,
		}
	}
	return "", lastErr
}

// Browser opener of the device flow, swappable so tests never launch one
var webbrowserOpen = provider.OpenBrowserTab

// Runs the whole device login, validating the token against the dashboard before persisting it
func LoginDeviceFlow(ctx context.Context, clientID string, openBrowser bool, onUserCode func(DeviceCode)) (string, error) {
	code, err := RequestDeviceCode(ctx, clientID, DefaultScope)
	if err != nil {
		return "", err
	}
	if onUserCode != nil {
		safe(func() { onUserCode(code) })
	}
	if openBrowser {
		if target, ok := anyutil.Or(code.VerificationURIComplete, code.VerificationURI).(string); ok {
			safe(func() { _ = webbrowserOpen(target) })
		}
	}
	first, err := PollForToken(ctx, code, clientID, 0, nil, nil)
	if err != nil {
		return "", err
	}
	token, err := validatedDashboardToken(ctx, []deviceTokenCandidate{{Source: "poll", Token: first}})
	if err != nil {
		return "", err
	}
	if err := StorePhotonToken(ctx, token); err != nil {
		return "", err
	}
	return token, nil
}

// The list inside a dashboard or spectrum envelope, one level of nesting deep
func unwrapList(data any) []any {
	if list, ok := data.([]any); ok {
		return list
	}
	o, ok := data.(*jsonutil.Object)
	if !ok || o == nil {
		return []any{}
	}
	for _, key := range []string{"data", "projects", "users", "lines", "items"} {
		inner, _ := o.Get(key)
		if list, ok := inner.([]any); ok {
			return list
		}
		if io, ok := inner.(*jsonutil.Object); ok && io != nil {
			for _, k := range []string{"projects", "users", "lines", "items"} {
				if v, _ := io.Get(k); v != nil {
					if list, ok := v.([]any); ok {
						return list
					}
				}
			}
		}
	}
	return []any{}
}

func dashboardList(ctx context.Context, path, token string) ([]any, error) {
	resp, err := dashboardGet(ctx, path, token)
	if err != nil {
		return nil, err
	}
	if err := resp.RaiseForStatus(); err != nil {
		return nil, err
	}
	data, err := resp.JSON()
	if err != nil {
		return nil, err
	}
	return unwrapList(data), nil
}

func raiseOnErrorKey(data any, action string) error {
	v, err := lookup(data, "error")
	if err != nil {
		return err
	}
	if anyutil.IsTrue(v) {
		return runtimeError("Photon %s failed: %s", action, anyutil.String(v))
	}
	return nil
}

// The caller's dashboard projects
func ListProjects(ctx context.Context, token string) ([]any, error) {
	return dashboardList(ctx, "/api/projects", token)
}

// First project whose name matches case insensitively, nil when none does
func FindProjectByName(ctx context.Context, token, name string) (*jsonutil.Object, error) {
	target := strings.ToLower(strings.TrimSpace(name))
	projects, err := ListProjects(ctx, token)
	if err != nil {
		return nil, err
	}
	for _, proj := range projects {
		n, err := lookup(proj, "name")
		if err != nil {
			return nil, err
		}
		s, err := stringValue(anyutil.Or(n, ""))
		if err != nil {
			return nil, err
		}
		if strings.ToLower(strings.TrimSpace(s)) == target {
			return proj.(*jsonutil.Object), nil
		}
	}
	return nil, nil
}

// Creates a dashboard project, Spectrum always provisioned so no spectrum flag is sent
func CreateProject(ctx context.Context, token, name, location string) (*jsonutil.Object, error) {
	body := jsonutil.ObjectOf("name", name, "location", location, "template", false, "observability", false)
	data, err := dashboardPost(ctx, "/api/projects", body, token)
	if err != nil {
		return nil, err
	}
	o, ok := data.(*jsonutil.Object)
	if !ok {
		return nil, runtimeError("Photon create-project returned an unexpected response")
	}
	if err := raiseOnErrorKey(o, "create-project"); err != nil {
		return nil, err
	}
	if succeed, _ := o.Get("succeed"); succeed == false {
		msg, _ := o.Get("message")
		return nil, runtimeError("Photon create-project failed: %s", anyutil.String(anyutil.Or(msg, o)))
	}
	project := o
	if inner, _ := o.Get("data"); inner != nil {
		if io, ok := inner.(*jsonutil.Object); ok {
			project = io
		}
	}
	if id, _ := project.Get("id"); !anyutil.IsTrue(id) {
		return nil, runtimeError("Photon create-project did not return a project id")
	}
	return project, nil
}

// Regenerates the project secret and returns it, the only way to read one
func RegenerateProjectSecret(ctx context.Context, token, projectID string) (string, error) {
	data, err := dashboardPost(ctx, "/api/projects/"+projectID+"/regenerate-secret", jsonutil.NewObject(), token)
	if err != nil {
		return "", err
	}
	if err := raiseOnErrorKey(data, "regenerate-secret"); err != nil {
		return "", err
	}
	secret, err := lookup(data, "projectSecret")
	if err != nil {
		return "", err
	}
	if !anyutil.IsTrue(secret) {
		return "", runtimeError("Photon regenerate-secret returned no projectSecret")
	}
	return anyutil.String(secret), nil
}

// Phone text reduced to plus signs and decimal digits for dedup comparison
func normalizePhone(phone string) string {
	return strings.Map(func(r rune) rune {
		if r == '+' || unicode.Is(unicode.Nd, r) {
			return r
		}
		return -1
	}, phone)
}

// Spectrum users of a project
func ListUsers(ctx context.Context, projectID, projectSecret string, timeout time.Duration) ([]any, error) {
	resp, err := httpGet(ctx, spectrumHost()+"/projects/"+projectID+"/users/", basic(projectID, projectSecret), timeout)
	if err != nil {
		return nil, err
	}
	if err := raiseForStatus(resp, "list-users"); err != nil {
		return nil, err
	}
	data, err := resp.JSON()
	if err != nil {
		return nil, err
	}
	return unwrapList(data), nil
}

// The routable Spectrum user with the phone number, nil when none exists
func FindUserByPhone(ctx context.Context, projectID, projectSecret, phoneNumber string, timeout time.Duration) (*jsonutil.Object, error) {
	users, err := ListUsers(ctx, projectID, projectSecret, timeout)
	if err != nil {
		return nil, err
	}
	return FindRoutableUser(users, phoneNumber)
}

// Optional profile fields of a new Spectrum user, empty ones left out of the request
type UserFields struct {
	FirstName string
	LastName  string
	Email     string
}

// Creates a shared Spectrum user for an E.164 phone number and returns it
func CreateUser(ctx context.Context, projectID, projectSecret, phoneNumber string, fields UserFields, sendInvite bool) (*jsonutil.Object, error) {
	if !E164RE.MatchString(phoneNumber) {
		return nil, &errorValue{class: "ValueError", msg: "phone_number must be E.164 (e.g. +15551234567); got " + strconv.Quote(phoneNumber)}
	}
	body := jsonutil.ObjectOf("type", "shared", "phoneNumber", phoneNumber)
	if sendInvite {
		pkgLog().Debug("photon: send_invite is ignored by Spectrum shared-user creation")
	}
	for _, kv := range [][2]string{{"firstName", fields.FirstName}, {"lastName", fields.LastName}, {"email", fields.Email}} {
		if kv[1] != "" {
			body.Set(kv[0], kv[1])
		}
	}
	resp, err := httpPost(ctx, spectrumHost()+"/projects/"+projectID+"/users/", body, basic(projectID, projectSecret), httpTimeout)
	if err != nil {
		return nil, err
	}
	if err := raiseForStatus(resp, "create-user"); err != nil {
		return nil, err
	}
	data, err := resp.JSON()
	if err != nil {
		return nil, err
	}
	data = anyutil.Or(data, jsonutil.NewObject())
	if err := raiseOnErrorKey(data, "create-user"); err != nil {
		return nil, err
	}
	o := data.(*jsonutil.Object)
	u, _ := o.Get("user")
	d, _ := o.Get("data")
	user, ok := anyutil.Or(u, d, o).(*jsonutil.Object)
	if !ok || user == nil {
		return nil, runtimeError("Photon create-user returned an unexpected response")
	}
	return user, nil
}

// Registers a Spectrum user unless one with the number exists, reporting whether it created one
func RegisterUserIfAbsent(ctx context.Context, projectID, projectSecret, phoneNumber string, fields UserFields) (*jsonutil.Object, bool, error) {
	existing, err := FindUserByPhone(ctx, projectID, projectSecret, phoneNumber, httpTimeout)
	if err != nil {
		return nil, false, err
	}
	if existing != nil {
		return existing, false, nil
	}
	user, err := CreateUser(ctx, projectID, projectSecret, phoneNumber, fields, false)
	return user, err == nil, err
}

// The iMessage number a user texts to reach the agent, empty when unassigned
func UserAssignedLine(user *jsonutil.Object) string {
	if user == nil || user.Len() == 0 {
		return ""
	}
	v, _ := user.Get("assignedPhoneNumber")
	return strOrEmpty(v)
}

// True when the row carries meta opt_in, the flag the delivery plane routes on
func UserOptedIn(user *jsonutil.Object) bool {
	if user == nil || user.Len() == 0 {
		return false
	}
	meta, _ := user.Get("meta")
	m, ok := meta.(*jsonutil.Object)
	if !ok || m == nil {
		return false
	}
	optIn, _ := m.Get("opt_in")
	return anyutil.IsTrue(optIn)
}

// The row deciding routing for the number, an opted in duplicate winning over list order
func FindRoutableUser(users []any, phoneNumber string) (*jsonutil.Object, error) {
	target := normalizePhone(phoneNumber)
	var matches []*jsonutil.Object
	for _, u := range users {
		pn, err := lookup(u, "phoneNumber")
		if err != nil {
			return nil, err
		}
		s, ok := anyutil.Or(pn, "").(string)
		if !ok {
			return nil, &errorValue{class: "TypeError", msg: fmt.Sprintf("expected string or bytes-like object, got '%s'", anyutil.Kind(pn))}
		}
		if normalizePhone(s) == target {
			matches = append(matches, u.(*jsonutil.Object))
		}
	}
	for _, m := range matches {
		if UserOptedIn(m) {
			return m, nil
		}
	}
	if len(matches) > 0 {
		return matches[0], nil
	}
	return nil, nil
}

// Polls until the number's row is opted in under an attempt cap and a wall clock deadline, nil at the bound
func WaitForOptedInUser(ctx context.Context, projectID, projectSecret, phoneNumber string, timeoutS, intervalS float64,
	onWaiting func(int), sleep func(float64), clock func() float64) (*jsonutil.Object, error) {
	if sleep == nil {
		sleep = func(s float64) {
			t := time.NewTimer(time.Duration(s * float64(time.Second)))
			defer t.Stop()
			select {
			case <-t.C:
			case <-ctx.Done():
			}
		}
	}
	if clock == nil {
		start := time.Now()
		clock = func() float64 { return time.Since(start).Seconds() }
	}
	deadline := clock() + timeoutS
	if intervalS == 0 {
		return nil, &errorValue{class: "ZeroDivisionError", msg: "float division by zero"}
	}
	ratio := timeoutS / intervalS
	switch {
	case math.IsNaN(ratio):
		return nil, &errorValue{class: "ValueError", msg: "cannot convert float NaN to integer"}
	case math.IsInf(ratio, 0):
		return nil, &errorValue{class: "OverflowError", msg: "cannot convert float infinity to integer"}
	}
	maxAttempts := int64(1)
	if ratio >= math.MaxInt64 {
		maxAttempts = math.MaxInt64
	} else if n := int64(ratio); n > 1 {
		maxAttempts = n
	}
	for attempt := int64(1); attempt <= maxAttempts; attempt++ {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		user, err := FindUserByPhone(ctx, projectID, projectSecret, phoneNumber, httpTimeout)
		switch {
		case err == nil && UserOptedIn(user):
			return user, nil
		case err != nil && !isProbeFailure(err):
			return nil, err
		case err != nil:
			pkgLog().Debug(fmt.Sprintf("photon: opt-in poll failed (attempt %d): %s", attempt, err))
		}
		if onWaiting != nil {
			onWaiting(int(attempt))
		}
		if attempt == maxAttempts || clock()+intervalS > deadline {
			break
		}
		sleep(intervalS)
	}
	return nil, nil
}

// Operator and assigned phone numbers for status, the operator inferred from the env when unstored
func LoadUserNumbers(ctx context.Context) (string, string, error) {
	auth, err := loadAuth(ctx)
	if err != nil {
		return "", "", err
	}
	entry, err := poolFirst(auth, "photon_user")
	if err != nil {
		return "", "", err
	}
	o, ok := entry.(*jsonutil.Object)
	if !ok || o == nil {
		o = jsonutil.NewObject()
	}
	pick := func(a, b string) any {
		va, _ := o.Get(a)
		vb, _ := o.Get(b)
		return anyutil.Or(va, vb)
	}
	phone, assigned := pick("phone_number", "phoneNumber"), pick("assigned_phone_number", "assignedPhoneNumber")
	p := strOrEmpty(phone)
	if p == "" {
		if p, err = configuredOperatorPhone(ctx); err != nil {
			return "", "", err
		}
	}
	return p, strOrEmpty(assigned), nil
}

// Refreshes the cached user numbers from Photon without provisioning anything
func RefreshUserNumbers(ctx context.Context, projectID, projectSecret string) (string, string, error) {
	phone, assigned, err := LoadUserNumbers(ctx)
	if err != nil {
		return "", "", err
	}
	var user any
	if phone != "" {
		u, err := FindUserByPhone(ctx, projectID, projectSecret, phone, httpTimeout)
		if err != nil {
			return "", "", err
		}
		if u != nil {
			user = u
		}
	} else {
		users, err := ListUsers(ctx, projectID, projectSecret, httpTimeout)
		if err != nil {
			return "", "", err
		}
		if len(users) == 1 {
			user = users[0]
		}
	}
	var userID any
	if anyutil.IsTrue(user) {
		o, ok := user.(*jsonutil.Object)
		if !ok {
			return "", "", kindError("an object", user)
		}
		userID, _ = o.Get("id")
		pn, _ := o.Get("phoneNumber")
		if dashboardPhone := normalizePhone(anyutil.String(anyutil.Or(pn, ""))); E164RE.MatchString(dashboardPhone) {
			phone = dashboardPhone
		}
		assigned = UserAssignedLine(o)
	}
	dashboardID, err := loadDashboardProjectIDRaw(ctx)
	if err != nil {
		return "", "", err
	}
	token := ""
	if assigned == "" {
		if token, err = LoadPhotonToken(ctx); err != nil {
			return "", "", err
		}
	}
	if token != "" && anyutil.IsTrue(dashboardID) {
		if line, err := lineNumber(ctx, token, anyutil.String(dashboardID)); err != nil {
			if isBaseException(err) {
				return "", "", err
			}
			pkgLog().Debug(fmt.Sprintf("photon: could not refresh iMessage line for status: %s", err))
		} else if line != "" {
			assigned = line
		}
	}
	if err := StoreUserNumbers(ctx, phone, assigned, strOrEmpty(userID), dashboardID); err != nil {
		return "", "", err
	}
	return phone, assigned, nil
}

// Phone number of the existing iMessage line, empty when there is none
func lineNumber(ctx context.Context, token, projectID string) (string, error) {
	line, err := GetIMessageLine(ctx, token, projectID, false)
	if err != nil || !anyutil.IsTrue(line) {
		return "", err
	}
	pn, err := lookup(line, "phoneNumber")
	return strOrEmpty(pn), err
}

// Operator E.164 number from the home channel or a sole allowed user in the Photon env settings
func configuredOperatorPhone(ctx context.Context) (string, error) {
	homeChannel, err := getConfigEnvValue(ctx, "PHOTON_HOME_CHANNEL")
	if err != nil {
		return "", err
	}
	if h := normalizePhone(homeChannel); h != "" && E164RE.MatchString(h) {
		return h, nil
	}
	allowed, err := getConfigEnvValue(ctx, "PHOTON_ALLOWED_USERS")
	if err != nil {
		return "", err
	}
	var candidates []string
	for _, part := range strings.FieldsFunc(allowed, func(r rune) bool { return r == ',' || unicode.IsSpace(r) }) {
		if n := normalizePhone(part); E164RE.MatchString(n) {
			candidates = append(candidates, n)
		}
	}
	if len(candidates) == 1 {
		return candidates[0], nil
	}
	return "", nil
}

func getConfigEnvValue(ctx context.Context, key string) (string, error) {
	v, _, err := config.GetEnvValue(ctx, home.Dir(ctx), key)
	return v, err
}

// The project's provisioned lines
func ListLines(ctx context.Context, token, projectID string) ([]any, error) {
	return dashboardList(ctx, "/api/projects/"+projectID+"/lines", token)
}

// Provisions a new line on the project and returns it
func AddLine(ctx context.Context, token, projectID, platform string) (any, error) {
	data, err := dashboardPost(ctx, "/api/projects/"+projectID+"/lines", jsonutil.ObjectOf("platform", platform), token)
	if err != nil {
		return nil, err
	}
	if err := raiseOnErrorKey(data, "add-line"); err != nil {
		return nil, err
	}
	line, _ := lookup(data, "line")
	return anyutil.Or(line, data), nil
}

// The project's iMessage line, provisioned when absent and asked for, nil when provisioning failed
func GetIMessageLine(ctx context.Context, token, projectID string, createIfMissing bool) (any, error) {
	lines, err := ListLines(ctx, token, projectID)
	if err != nil {
		return nil, err
	}
	var line any
	for _, ln := range lines {
		p, err := lookup(ln, "platform")
		if err != nil {
			return nil, err
		}
		s, err := stringValue(anyutil.Or(p, ""))
		if err != nil {
			return nil, err
		}
		if strings.ToLower(s) == "imessage" {
			line = ln
			break
		}
	}
	if line != nil || !createIfMissing {
		return line, nil
	}
	added, err := AddLine(ctx, token, projectID, "imessage")
	if err != nil {
		if isBaseException(err) {
			return nil, err
		}
		pkgLog().Warn(fmt.Sprintf("photon: could not auto-provision iMessage line: %s", err))
		return nil, nil
	}
	return added, nil
}

// Display text of a credential value, the TypeError Python raises concatenating a non string
func displayValue(v any, missing string) (string, error) {
	if !anyutil.IsTrue(v) {
		return missing, nil
	}
	s, ok := v.(string)
	if !ok {
		return "", &errorValue{class: "TypeError", msg: fmt.Sprintf("can only concatenate str (not \"%s\") to str", anyutil.Kind(v))}
	}
	return s, nil
}

// Emits the credential status table with every secret reduced to a stored or missing marker
func PrintCredentialSummary(ctx context.Context, emit func(string)) error {
	sid, sec, err := loadProjectCredentialsRaw(ctx)
	if err != nil {
		return err
	}
	phone, assigned, err := LoadUserNumbers(ctx)
	if err != nil {
		return err
	}
	token, err := LoadPhotonToken(ctx)
	if err != nil {
		return err
	}
	tokenRow := "✗ missing (run `icebath photon setup`)"
	if token != "" {
		tokenRow = "✓ stored"
	}
	sidRow, err := displayValue(sid, "✗ missing")
	if err != nil {
		return err
	}
	secRow := "✗ missing"
	if anyutil.IsTrue(sec) {
		secRow = "✓ stored"
	}
	phoneRow := anyutil.Or(phone, "✗ missing (run `icebath photon setup --phone ...`)").(string)
	assignedRow := anyutil.Or(assigned, "✗ missing (run `icebath photon setup`)").(string)
	emit(strings.Join([]string{
		"Photon iMessage status",
		"──────────────────────",
		"  device token        : " + tokenRow,
		"  project id          : " + sidRow,
		"  project secret      : " + secRow,
		"  my number           : " + phoneRow,
		"  assigned number     : " + assignedRow,
	}, "\n"))
	return nil
}

// Emits the live opt in row, verified against the API with a short timeout
func PrintRoutingStatus(ctx context.Context, emit func(string)) error {
	phone, assigned, err := LoadUserNumbers(ctx)
	if err != nil {
		return err
	}
	sid, sec, err := loadProjectCredentialsRaw(ctx)
	if err != nil {
		return err
	}
	if phone == "" || !anyutil.IsTrue(sid) || !anyutil.IsTrue(sec) {
		return nil
	}
	user, err := FindUserByPhone(ctx, anyutil.String(sid), anyutil.String(sec), phone, statusTimeout)
	if err != nil {
		if !isProbeFailure(err) {
			return err
		}
		emit("  routing             : ? could not verify (" + anyutil.Or(err.Error(), errorutil.Kind(err)).(string) + ")")
		return nil
	}
	if UserOptedIn(user) {
		emit("  routing             : ✓ opted in (verified live)")
		return nil
	}
	line := anyutil.Or(UserAssignedLine(user), assigned, "your assigned line").(string)
	emit(fmt.Sprintf("  routing             : ✗ not opted in — text %s once from %s, then re-check", line, phone))
	return nil
}
