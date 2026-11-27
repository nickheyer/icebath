package msalruntime

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"runtime"
	"strings"
	"sync"
	"unsafe"
)

// Version of the msalruntime release whose native library this binding drives
const Version = "0.20.7"

// MSALRUNTIME_RESPONSE_STATUS of an error handle
type Status int32

const (
	StatusUnexpected Status = iota
	StatusReserved
	StatusInteractionRequired
	StatusNoNetwork
	StatusNetworkTemporarilyUnavailable
	StatusServerTemporarilyUnavailable
	StatusApiContractViolation
	StatusUserCanceled
	StatusApplicationCanceled
	StatusIncorrectConfiguration
	StatusInsufficientBuffer
	StatusAuthorityUntrusted
	StatusUserSwitch
	StatusAccountUnusable
	StatusUserDataRemovalRequired
	StatusKeyNotFound
	StatusAccountNotFound
	StatusTransientError
	StatusAccountSwitch
	StatusRequiredBrokerMissing
	StatusDeviceNotRegistered
	StatusFallbackToNativeMsal
)

var statusNames = [...]string{"Status_Unexpected", "Status_Reserved", "Status_InteractionRequired", "Status_NoNetwork",
	"Status_NetworkTemporarilyUnavailable", "Status_ServerTemporarilyUnavailable", "Status_ApiContractViolation",
	"Status_UserCanceled", "Status_ApplicationCanceled", "Status_IncorrectConfiguration", "Status_InsufficientBuffer",
	"Status_AuthorityUntrusted", "Status_UserSwitch", "Status_AccountUnusable", "Status_UserDataRemovalRequired",
	"Status_KeyNotFound", "Status_AccountNotFound", "Status_TransientError", "Status_AccountSwitch",
	"Status_RequiredBrokerMissing", "Status_DeviceNotRegistered", "Status_FallbackToNativeMsal"}

// Python enum text pymsalruntime formats, Response_Status.Status_Name or the bare number outside the enum
func (s Status) String() string {
	if s >= 0 && int(s) < len(statusNames) {
		return "Response_Status." + statusNames[s]
	}
	return fmt.Sprint(int32(s))
}

// MSALRUNTIME_LOG_LEVEL of a log callback entry
type LogLevel int32

const (
	LogTrace LogLevel = iota + 1
	LogDebug
	LogInfo
	LogWarning
	LogError
	LogFatal
)

// Window handle the interactive calls parent their UI on, HWND on Windows and int64 elsewhere
type WindowHandle uintptr

// Contents of an MSALRUNTIME_ERROR_HANDLE read before the handle is released
type Error struct {
	Status  Status
	Code    int64
	Tag     int32
	Context string
}

func (e *Error) Error() string {
	return fmt.Sprintf("%s. The sign-in broker gave the state %s, error code %d and tag %d.", strings.TrimSuffix(e.Context, "."), e.Status, e.Code, e.Tag)
}

// Library cannot be loaded on this machine, naming the file and the reason
type LoadError struct {
	Path   string
	Detail string
}

func (e *LoadError) Error() string {
	return fmt.Sprintf("The msalruntime library %s could not be loaded: %s", e.Path, e.Detail)
}

// MSALRUNTIME_Startup failed, the condition msal answers by turning the broker off
type StartupError struct{ Err *Error }

func (e *StartupError) Error() string {
	return "The msalruntime library could not start: " + e.Err.Error()
}

// Native library file Microsoft publishes for this OS and architecture, false where none is published
func libraryName() (string, bool) {
	switch runtime.GOOS + "/" + runtime.GOARCH {
	case "windows/amd64":
		return "msalruntime.dll", true
	case "windows/386":
		return "msalruntime_x86.dll", true
	case "windows/arm64":
		return "msalruntime_arm64.dll", true
	case "darwin/arm64":
		return "libmsalruntime_arm64.dylib", true
	case "darwin/amd64":
		return "libmsalruntime_x86_64.dylib", true
	case "linux/amd64":
		return "libmsalruntime.so", true
	}
	return "", false
}

// Every C function the binding calls, resolved from the library by name
type functions struct {
	startup, shutdown                                                          uintptr
	readAccountByIDAsync, signInSilentlyAsync, signInInteractivelyAsync        uintptr
	acquireTokenSilentlyAsync, releaseAsyncHandle, cancelAsyncOperation        uintptr
	registerLogCallback, createAuthParameters, releaseAuthParameters           uintptr
	setRequestedScopes, setRedirectURI, setAdditionalParameter                 uintptr
	releaseError, getStatus, getErrorCode, getTag, getContext                  uintptr
	releaseAuthResult, getAccount, getIDToken, getRawIDToken, getAccessToken   uintptr
	getGrantedScopes, getAuthorizationHeader, isPopAuthorization, getExpiresOn uintptr
	getError, getTelemetryData, releaseAccount, getAccountID, getClientInfo    uintptr
	releaseReadAccountResult, getReadAccount, getReadAccountError              uintptr
	runloopRun, runloopStop                                                    uintptr
}

func (f *functions) table() map[string]*uintptr {
	return map[string]*uintptr{
		"MSALRUNTIME_Startup": &f.startup, "MSALRUNTIME_Shutdown": &f.shutdown,
		"MSALRUNTIME_ReadAccountByIdAsync": &f.readAccountByIDAsync, "MSALRUNTIME_SignInSilentlyAsync": &f.signInSilentlyAsync,
		"MSALRUNTIME_SignInInteractivelyAsync": &f.signInInteractivelyAsync, "MSALRUNTIME_AcquireTokenSilentlyAsync": &f.acquireTokenSilentlyAsync,
		"MSALRUNTIME_ReleaseAsyncHandle": &f.releaseAsyncHandle, "MSALRUNTIME_CancelAsyncOperation": &f.cancelAsyncOperation,
		"MSALRUNTIME_RegisterLogCallback": &f.registerLogCallback, "MSALRUNTIME_CreateAuthParameters": &f.createAuthParameters,
		"MSALRUNTIME_ReleaseAuthParameters": &f.releaseAuthParameters, "MSALRUNTIME_SetRequestedScopes": &f.setRequestedScopes,
		"MSALRUNTIME_SetRedirectUri": &f.setRedirectURI, "MSALRUNTIME_SetAdditionalParameter": &f.setAdditionalParameter,
		"MSALRUNTIME_ReleaseError": &f.releaseError, "MSALRUNTIME_GetStatus": &f.getStatus, "MSALRUNTIME_GetErrorCode": &f.getErrorCode,
		"MSALRUNTIME_GetTag": &f.getTag, "MSALRUNTIME_GetContext": &f.getContext, "MSALRUNTIME_ReleaseAuthResult": &f.releaseAuthResult,
		"MSALRUNTIME_GetAccount": &f.getAccount, "MSALRUNTIME_GetIdToken": &f.getIDToken, "MSALRUNTIME_GetRawIdToken": &f.getRawIDToken,
		"MSALRUNTIME_GetAccessToken": &f.getAccessToken, "MSALRUNTIME_GetGrantedScopes": &f.getGrantedScopes,
		"MSALRUNTIME_GetAuthorizationHeader": &f.getAuthorizationHeader, "MSALRUNTIME_IsPopAuthorization": &f.isPopAuthorization,
		"MSALRUNTIME_GetExpiresOn": &f.getExpiresOn, "MSALRUNTIME_GetError": &f.getError, "MSALRUNTIME_GetTelemetryData": &f.getTelemetryData,
		"MSALRUNTIME_ReleaseAccount": &f.releaseAccount, "MSALRUNTIME_GetAccountId": &f.getAccountID, "MSALRUNTIME_GetClientInfo": &f.getClientInfo,
		"MSALRUNTIME_ReleaseReadAccountResult": &f.releaseReadAccountResult, "MSALRUNTIME_GetReadAccount": &f.getReadAccount,
		"MSALRUNTIME_GetReadAccountError": &f.getReadAccountError,
	}
}

// A started MSALRuntime with its log callback registered, shared by the whole process
type Runtime struct {
	Path string
	fn   functions
}

var (
	loadMu sync.Mutex
	loaded = map[string]*Runtime{}
	cbOnce sync.Once
	cbAuth uintptr
	cbRead uintptr
	cbLog  uintptr
)

// Loads the library at path, starts the runtime and registers the log callback, retried until a start succeeds
func Load(path string) (*Runtime, error) {
	loadMu.Lock()
	defer loadMu.Unlock()
	if r, ok := loaded[path]; ok {
		return r, nil
	}
	r, err := load(path)
	if err != nil {
		return nil, err
	}
	loaded[path] = r
	return r, nil
}

func load(path string) (*Runtime, error) {
	if !loadSupported {
		return nil, &LoadError{Path: path, Detail: "shared library loading is unavailable on this platform"}
	}
	lib, err := openLibrary(path)
	if err != nil {
		return nil, &LoadError{Path: path, Detail: err.Error()}
	}
	r := &Runtime{Path: path}
	for name, slot := range r.fn.table() {
		addr, err := lookupSymbol(lib, name)
		if err != nil {
			return nil, &LoadError{Path: path, Detail: err.Error()}
		}
		*slot = addr
	}
	if runtime.GOOS == "darwin" {
		for name, slot := range map[string]*uintptr{"MSALRUNTIME_Runloop_run": &r.fn.runloopRun, "MSALRUNTIME_Runloop_stop": &r.fn.runloopStop} {
			addr, err := lookupSymbol(lib, name)
			if err != nil {
				return nil, &LoadError{Path: path, Detail: err.Error()}
			}
			*slot = addr
		}
	}
	cbOnce.Do(func() {
		cbAuth = newCompletionCallback(deliver)
		cbRead = newCompletionCallback(deliver)
		cbLog = newLogCallback(logEntry)
	})
	if eh := call(r.fn.startup); eh != 0 {
		return nil, &StartupError{Err: r.takeError(eh)}
	}
	var handle uintptr
	if eh := call(r.fn.registerLogCallback, cbLog, 0, uintptr(unsafe.Pointer(&handle))); eh != 0 {
		err := r.takeError(eh)
		call(r.fn.shutdown)
		return nil, err
	}
	return r, nil
}

// Log entries routed into slog the way msal routes them, fatal as critical and everything else as debug
func logEntry(message uintptr, level int32, _ uintptr) {
	text := decodeOSString(message)
	logger := pkgLog()
	if LogLevel(level) == LogFatal {
		logger.Log(context.Background(), slog.LevelError+4, text)
		return
	}
	logger.Debug(text)
}

// Operations waiting for their completion callback, keyed by the callbackData each one passed
var (
	pendingMu sync.Mutex
	pending   = map[uintptr]chan uintptr{}
	nextID    uintptr
)

// Completion callback body, handing the result handle to the waiting call named by callbackData
func deliver(result, data uintptr) {
	pendingMu.Lock()
	ch, ok := pending[data]
	delete(pending, data)
	pendingMu.Unlock()
	if ok {
		ch <- result
	}
}

func register() (uintptr, chan uintptr) {
	pendingMu.Lock()
	defer pendingMu.Unlock()
	nextID++
	ch := make(chan uintptr, 1)
	pending[nextID] = ch
	return nextID, ch
}

func unregister(id uintptr) {
	pendingMu.Lock()
	delete(pending, id)
	pendingMu.Unlock()
}

// Reads status, code, tag and context of an error handle, then releases it
func (r *Runtime) takeError(eh uintptr) *Error {
	e := &Error{}
	var status int32
	if x := call(r.fn.getStatus, eh, uintptr(unsafe.Pointer(&status))); x == 0 {
		e.Status = Status(status)
	} else {
		r.releaseErrorHandle(x)
	}
	if x := call(r.fn.getErrorCode, eh, uintptr(unsafe.Pointer(&e.Code))); x != 0 {
		r.releaseErrorHandle(x)
	}
	if x := call(r.fn.getTag, eh, uintptr(unsafe.Pointer(&e.Tag))); x != 0 {
		r.releaseErrorHandle(x)
	}
	if ctx, err := r.readString(r.fn.getContext, eh, false); err == nil {
		e.Context = ctx
	}
	r.releaseErrorHandle(eh)
	return e
}

func (r *Runtime) releaseErrorHandle(eh uintptr) {
	if eh != 0 {
		call(r.fn.releaseError, eh)
	}
}

// Status of an error handle a getter returned, released at once
func (r *Runtime) statusOf(eh uintptr) Status {
	var status int32
	if x := call(r.fn.getStatus, eh, uintptr(unsafe.Pointer(&status))); x != 0 {
		r.releaseErrorHandle(x)
		return StatusUnexpected
	}
	return Status(status)
}

func (r *Runtime) getString(fn, handle uintptr) (string, error) {
	return r.readString(fn, handle, true)
}

// String getter with the two call length protocol, a getter failure read whole only outside error context reads
func (r *Runtime) readString(fn, handle uintptr, full bool) (string, error) {
	fail := func(eh uintptr) error {
		if full {
			return r.takeError(eh)
		}
		st := r.statusOf(eh)
		r.releaseErrorHandle(eh)
		return &Error{Status: st}
	}
	var size int32
	if eh := call(fn, handle, 0, uintptr(unsafe.Pointer(&size))); eh != 0 {
		if r.statusOf(eh) != StatusInsufficientBuffer {
			return "", fail(eh)
		}
		r.releaseErrorHandle(eh)
	}
	if size <= 0 {
		return "", nil
	}
	buf := newOSBuffer(size)
	if eh := call(fn, handle, buf.ptr(), uintptr(unsafe.Pointer(&size))); eh != 0 {
		return "", fail(eh)
	}
	return buf.text(), nil
}

// Error-returning call whose error handle becomes a Go error
func (r *Runtime) check(eh uintptr) error {
	if eh == 0 {
		return nil
	}
	return r.takeError(eh)
}

// Waits for the completion callback of a started operation, cancelling it when ctx ends first
func (r *Runtime) await(ctx context.Context, interactive bool, ch chan uintptr, async uintptr) uintptr {
	defer func() {
		if async != 0 {
			r.releaseErrorHandle(call(r.fn.releaseAsyncHandle, async))
		}
	}()
	if interactive && r.fn.runloopRun != 0 {
		return r.awaitRunloop(ctx, ch, async)
	}
	select {
	case res := <-ch:
		return res
	case <-ctx.Done():
		r.releaseErrorHandle(call(r.fn.cancelAsyncOperation, async))
		return <-ch
	}
}

// Interactive wait on macOS, running the runtime's run loop on this thread until the callback stops it
func (r *Runtime) awaitRunloop(ctx context.Context, ch chan uintptr, async uintptr) uintptr {
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	stop := context.AfterFunc(ctx, func() { r.releaseErrorHandle(call(r.fn.cancelAsyncOperation, async)) })
	defer stop()
	for {
		select {
		case res := <-ch:
			return res
		default:
		}
		call(r.fn.runloopRun)
	}
}

// Starts one asynchronous operation and returns the result handle its callback delivers
func (r *Runtime) run(ctx context.Context, interactive bool, cb uintptr, start func(cb, data, asyncOut uintptr) uintptr) (uintptr, error) {
	id, ch := register()
	var async uintptr
	if eh := start(cb, id, uintptr(unsafe.Pointer(&async))); eh != 0 {
		unregister(id)
		return 0, r.takeError(eh)
	}
	if interactive && r.fn.runloopStop != 0 {
		inner := ch
		ch = make(chan uintptr, 1)
		go func() {
			res := <-inner
			ch <- res
			call(r.fn.runloopStop)
		}()
	}
	return r.await(ctx, interactive, ch, async), nil
}

// MSALRUNTIME_AUTH_PARAMETERS_HANDLE with its setters
type AuthParameters struct {
	r *Runtime
	h uintptr
}

// Creates auth parameters for a client id and an authority URL
func (r *Runtime) NewAuthParameters(clientID, authority string) (*AuthParameters, error) {
	var h uintptr
	c, a := encodeOSString(clientID), encodeOSString(authority)
	if err := r.check(call(r.fn.createAuthParameters, c.ptr(), a.ptr(), uintptr(unsafe.Pointer(&h)))); err != nil {
		return nil, err
	}
	runtime.KeepAlive(c)
	runtime.KeepAlive(a)
	return &AuthParameters{r: r, h: h}, nil
}

// Releases the parameters handle
func (p *AuthParameters) Release() {
	if p.h != 0 {
		p.r.releaseErrorHandle(call(p.r.fn.releaseAuthParameters, p.h))
		p.h = 0
	}
}

func (p *AuthParameters) set(fn uintptr, values ...string) error {
	args := []uintptr{p.h}
	bufs := make([]osString, len(values))
	for i, v := range values {
		bufs[i] = encodeOSString(v)
		args = append(args, bufs[i].ptr())
	}
	err := p.r.check(call(fn, args...))
	runtime.KeepAlive(bufs)
	return err
}

// Requested scopes, joined with spaces into the single string the C API takes
func (p *AuthParameters) SetRequestedScopes(scopes []string) error {
	return p.set(p.r.fn.setRequestedScopes, strings.Join(scopes, " "))
}

func (p *AuthParameters) SetRedirectURI(uri string) error { return p.set(p.r.fn.setRedirectURI, uri) }

func (p *AuthParameters) SetAdditionalParameter(key, value string) error {
	return p.set(p.r.fn.setAdditionalParameter, key, value)
}

// MSALRUNTIME_ACCOUNT_HANDLE with its getters
type Account struct {
	r *Runtime
	h uintptr
}

// Releases the account handle
func (a *Account) Release() {
	if a != nil && a.h != 0 {
		a.r.releaseErrorHandle(call(a.r.fn.releaseAccount, a.h))
		a.h = 0
	}
}

func (a *Account) AccountID() (string, error)  { return a.r.getString(a.r.fn.getAccountID, a.h) }
func (a *Account) ClientInfo() (string, error) { return a.r.getString(a.r.fn.getClientInfo, a.h) }

// Account handle written to an out parameter, nil for a null handle
func (r *Runtime) takeAccount(fn, handle uintptr) (*Account, error) {
	var h uintptr
	if err := r.check(call(fn, handle, uintptr(unsafe.Pointer(&h)))); err != nil {
		return nil, err
	}
	if h == 0 {
		return nil, nil
	}
	return &Account{r: r, h: h}, nil
}

// Error handle written to an out parameter, nil when the operation reported none
func (r *Runtime) takeFailure(fn, handle uintptr) (*Error, error) {
	var eh uintptr
	if err := r.check(call(fn, handle, uintptr(unsafe.Pointer(&eh)))); err != nil {
		return nil, err
	}
	if eh == 0 {
		return nil, nil
	}
	return r.takeError(eh), nil
}

// Telemetry JSON object of a result as the string map pymsalruntime returns
func (r *Runtime) telemetry(fn, handle uintptr) (map[string]string, error) {
	raw, err := r.getString(fn, handle)
	if err != nil {
		return nil, err
	}
	out := map[string]string{}
	if strings.TrimSpace(raw) == "" {
		return out, nil
	}
	var parsed map[string]any
	if err := json.Unmarshal([]byte(raw), &parsed); err != nil {
		return nil, err
	}
	for k, v := range parsed {
		if s, ok := v.(string); ok {
			out[k] = s
		} else {
			b, _ := json.Marshal(v)
			out[k] = string(b)
		}
	}
	return out, nil
}

// MSALRUNTIME_AUTH_RESULT_HANDLE with its getters
type AuthResult struct {
	r *Runtime
	h uintptr
}

func (a *AuthResult) Release() {
	if a.h != 0 {
		a.r.releaseErrorHandle(call(a.r.fn.releaseAuthResult, a.h))
		a.h = 0
	}
}

func (a *AuthResult) Account() (*Account, error)   { return a.r.takeAccount(a.r.fn.getAccount, a.h) }
func (a *AuthResult) Failure() (*Error, error)     { return a.r.takeFailure(a.r.fn.getError, a.h) }
func (a *AuthResult) IDToken() (string, error)     { return a.r.getString(a.r.fn.getIDToken, a.h) }
func (a *AuthResult) RawIDToken() (string, error)  { return a.r.getString(a.r.fn.getRawIDToken, a.h) }
func (a *AuthResult) AccessToken() (string, error) { return a.r.getString(a.r.fn.getAccessToken, a.h) }
func (a *AuthResult) AuthorizationHeader() (string, error) {
	return a.r.getString(a.r.fn.getAuthorizationHeader, a.h)
}
func (a *AuthResult) Telemetry() (map[string]string, error) {
	return a.r.telemetry(a.r.fn.getTelemetryData, a.h)
}

// Granted scopes split on spaces, empty when the runtime reports none
func (a *AuthResult) GrantedScopes() ([]string, error) {
	s, err := a.r.getString(a.r.fn.getGrantedScopes, a.h)
	if err != nil {
		return nil, err
	}
	return strings.Fields(s), nil
}

func (a *AuthResult) IsPopAuthorization() (bool, error) {
	var v int32
	if err := a.r.check(call(a.r.fn.isPopAuthorization, a.h, uintptr(unsafe.Pointer(&v)))); err != nil {
		return false, err
	}
	return v != 0, nil
}

// Access token expiry as epoch seconds
func (a *AuthResult) ExpiresOn() (int64, error) {
	var v int64
	if err := a.r.check(call(a.r.fn.getExpiresOn, a.h, uintptr(unsafe.Pointer(&v)))); err != nil {
		return 0, err
	}
	return v, nil
}

// MSALRUNTIME_READ_ACCOUNT_RESULT_HANDLE with its getters
type ReadAccountResult struct {
	r *Runtime
	h uintptr
}

func (a *ReadAccountResult) Release() {
	if a.h != 0 {
		a.r.releaseErrorHandle(call(a.r.fn.releaseReadAccountResult, a.h))
		a.h = 0
	}
}

func (a *ReadAccountResult) Account() (*Account, error) {
	return a.r.takeAccount(a.r.fn.getReadAccount, a.h)
}

func (a *ReadAccountResult) Failure() (*Error, error) {
	return a.r.takeFailure(a.r.fn.getReadAccountError, a.h)
}

// Reads the broker account with this id
func (r *Runtime) ReadAccountByID(ctx context.Context, accountID, correlationID string) (*ReadAccountResult, error) {
	id, corr := encodeOSString(accountID), encodeOSString(correlationID)
	h, err := r.run(ctx, false, cbRead, func(cb, data, out uintptr) uintptr {
		return call(r.fn.readAccountByIDAsync, id.ptr(), corr.ptr(), cb, data, out)
	})
	runtime.KeepAlive(id)
	runtime.KeepAlive(corr)
	if err != nil {
		return nil, err
	}
	return &ReadAccountResult{r: r, h: h}, nil
}

func (r *Runtime) authOperation(ctx context.Context, interactive bool, start func(cb, data, out uintptr) uintptr) (*AuthResult, error) {
	h, err := r.run(ctx, interactive, cbAuth, start)
	if err != nil {
		return nil, err
	}
	return &AuthResult{r: r, h: h}, nil
}

// Signs in the default broker account without UI
func (r *Runtime) SignInSilently(ctx context.Context, params *AuthParameters, correlationID string) (*AuthResult, error) {
	corr := encodeOSString(correlationID)
	defer runtime.KeepAlive(corr)
	return r.authOperation(ctx, false, func(cb, data, out uintptr) uintptr {
		return call(r.fn.signInSilentlyAsync, params.h, corr.ptr(), cb, data, out)
	})
}

// Signs in through the broker UI parented on the window handle
func (r *Runtime) SignInInteractively(ctx context.Context, parent WindowHandle, params *AuthParameters, correlationID, accountHint string) (*AuthResult, error) {
	corr, hint := encodeOSString(correlationID), encodeOSString(accountHint)
	defer runtime.KeepAlive(hint)
	defer runtime.KeepAlive(corr)
	return r.authOperation(ctx, true, func(cb, data, out uintptr) uintptr {
		return call(r.fn.signInInteractivelyAsync, uintptr(parent), params.h, corr.ptr(), hint.ptr(), cb, data, out)
	})
}

// Acquires a token for a broker account without UI
func (r *Runtime) AcquireTokenSilently(ctx context.Context, params *AuthParameters, correlationID string, account *Account) (*AuthResult, error) {
	corr := encodeOSString(correlationID)
	defer runtime.KeepAlive(corr)
	return r.authOperation(ctx, false, func(cb, data, out uintptr) uintptr {
		return call(r.fn.acquireTokenSilentlyAsync, params.h, corr.ptr(), account.h, cb, data, out)
	})
}
