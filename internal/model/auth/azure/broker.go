package azure

import (
	"context"
	"errors"
	"log/slog"
	"strconv"
	"strings"
	"sync"
	"time"
	"unsafe"

	"github.com/google/uuid"

	"github.com/nickheyer/icebath/internal/dl"
	"github.com/nickheyer/icebath/internal/toolchain"
)

const (
	brokerPrefix          = "InteractiveBrowserBrokerCredential unavailable. "
	brokerPlatformOnly    = brokerPrefix + "Brokered authentication is only supported on Windows and WSL platforms."
	brokerCancelGrace     = 5 * time.Second
	brokerInsufficient    = 10
	brokerFatalLevel      = 6
	brokerDefaultTenant   = "organizations"
	brokerStatusUndefined = "Unexpected"
)

var brokerStatusNames = []string{"Unexpected", "Reserved", "InteractionRequired", "NoNetwork", "NetworkTemporarilyUnavailable",
	"ServerTemporarilyUnavailable", "ApiContractViolation", "UserCanceled", "ApplicationCanceled", "IncorrectConfiguration",
	"InsufficientBuffer", "AuthorityUntrusted", "UserSwitch", "AccountUnusable", "UserDataRemovalRequired", "KeyNotFound",
	"AccountNotFound", "TransientError", "AccountSwitch", "RequiredBrokerMissing", "DeviceNotRegistered", "FallbackToNativeMsal"}

type brokerError struct {
	status  int32
	code    int64
	tag     int32
	context string
}

func (e *brokerError) Error() string {
	name := brokerStatusUndefined
	if e.status >= 0 && int(e.status) < len(brokerStatusNames) {
		name = brokerStatusNames[e.status]
	}
	return e.context + ". Status: " + name + ", Error code: " + strconv.FormatInt(e.code, 10) + ", Tag: " + strconv.FormatInt(int64(e.tag), 10)
}

type brokerRuntime struct {
	startup, createAuthParameters, releaseAuthParameters, setRequestedScopes, setAdditionalParameter uintptr
	signInSilentlyAsync, readAccountByIDAsync, acquireTokenSilentlyAsync                             uintptr
	releaseAsyncHandle, cancelAsyncOperation, registerLogCallback                                    uintptr
	releaseAuthResult, getAccount, getAccessToken, getExpiresOn, getError                            uintptr
	releaseAccount, getAccountID, releaseReadAccountResult, getReadAccount, getReadAccountError      uintptr
	releaseError, getStatus, getErrorCode, getTag, getContext                                        uintptr
}

func (r *brokerRuntime) symbols() map[string]*uintptr {
	return map[string]*uintptr{
		"MSALRUNTIME_Startup": &r.startup, "MSALRUNTIME_CreateAuthParameters": &r.createAuthParameters,
		"MSALRUNTIME_ReleaseAuthParameters": &r.releaseAuthParameters, "MSALRUNTIME_SetRequestedScopes": &r.setRequestedScopes,
		"MSALRUNTIME_SetAdditionalParameter": &r.setAdditionalParameter, "MSALRUNTIME_SignInSilentlyAsync": &r.signInSilentlyAsync,
		"MSALRUNTIME_ReadAccountByIdAsync": &r.readAccountByIDAsync, "MSALRUNTIME_AcquireTokenSilentlyAsync": &r.acquireTokenSilentlyAsync,
		"MSALRUNTIME_ReleaseAsyncHandle": &r.releaseAsyncHandle, "MSALRUNTIME_CancelAsyncOperation": &r.cancelAsyncOperation,
		"MSALRUNTIME_RegisterLogCallback": &r.registerLogCallback, "MSALRUNTIME_ReleaseAuthResult": &r.releaseAuthResult,
		"MSALRUNTIME_GetAccount": &r.getAccount, "MSALRUNTIME_GetAccessToken": &r.getAccessToken, "MSALRUNTIME_GetExpiresOn": &r.getExpiresOn,
		"MSALRUNTIME_GetError": &r.getError, "MSALRUNTIME_ReleaseAccount": &r.releaseAccount, "MSALRUNTIME_GetAccountId": &r.getAccountID,
		"MSALRUNTIME_ReleaseReadAccountResult": &r.releaseReadAccountResult, "MSALRUNTIME_GetReadAccount": &r.getReadAccount,
		"MSALRUNTIME_GetReadAccountError": &r.getReadAccountError, "MSALRUNTIME_ReleaseError": &r.releaseError,
		"MSALRUNTIME_GetStatus": &r.getStatus, "MSALRUNTIME_GetErrorCode": &r.getErrorCode, "MSALRUNTIME_GetTag": &r.getTag,
		"MSALRUNTIME_GetContext": &r.getContext,
	}
}

type brokerProcess struct {
	mu       sync.Mutex
	runtimes map[string]*brokerRuntime
	complete uintptr
	logged   uintptr

	waitMu  sync.Mutex
	waiting map[uintptr]chan uintptr
	next    uintptr
}

var brokerHost = &brokerProcess{runtimes: map[string]*brokerRuntime{}, waiting: map[uintptr]chan uintptr{}}

func (p *brokerProcess) deliver(result, data uintptr) uintptr {
	p.waitMu.Lock()
	ch, ok := p.waiting[data]
	delete(p.waiting, data)
	p.waitMu.Unlock()
	if ok {
		ch <- result
	}
	return 0
}

func (p *brokerProcess) register() (uintptr, chan uintptr) {
	p.waitMu.Lock()
	defer p.waitMu.Unlock()
	p.next++
	ch := make(chan uintptr, 1)
	p.waiting[p.next] = ch
	return p.next, ch
}

func (p *brokerProcess) forget(id uintptr) {
	p.waitMu.Lock()
	delete(p.waiting, id)
	p.waitMu.Unlock()
}

func brokerLog(message, level, _ uintptr) uintptr {
	text := osText(message)
	if int32(level) == brokerFatalLevel {
		slog.Error("account broker: " + text)
	} else {
		slog.Debug("account broker: " + text)
	}
	return 0
}

func (p *brokerProcess) load(path string) (*brokerRuntime, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if r, ok := p.runtimes[path]; ok {
		return r, nil
	}
	lib, err := dl.Open(path, dl.Local)
	if err != nil {
		return nil, err
	}
	r := &brokerRuntime{}
	for name, slot := range r.symbols() {
		addr, err := lib.Symbol(name)
		if err != nil {
			return nil, err
		}
		*slot = addr
	}
	if p.complete == 0 {
		if p.complete, err = dl.Callback(p.deliver); err != nil {
			return nil, err
		}
		if p.logged, err = dl.Callback(brokerLog); err != nil {
			return nil, err
		}
	}
	if handle, _ := dl.Call(r.startup); handle != 0 {
		return nil, r.takeError(handle)
	}
	var logHandle uintptr
	if handle, _ := dl.Call(r.registerLogCallback, p.logged, 0, uintptr(unsafe.Pointer(&logHandle))); handle != 0 {
		r.release(handle)
	}
	p.runtimes[path] = r
	return r, nil
}

func (r *brokerRuntime) release(handle uintptr) {
	if handle != 0 {
		dl.Call(r.releaseError, handle)
	}
}

func (r *brokerRuntime) statusOf(handle uintptr) int32 {
	var status int32
	if failed, _ := dl.Call(r.getStatus, handle, uintptr(unsafe.Pointer(&status))); failed != 0 {
		r.release(failed)
		return 0
	}
	return status
}

func (r *brokerRuntime) readString(getter, handle uintptr) (string, bool) {
	var size int32
	if failed, _ := dl.Call(getter, handle, 0, uintptr(unsafe.Pointer(&size))); failed != 0 {
		insufficient := r.statusOf(failed) == brokerInsufficient
		r.release(failed)
		if !insufficient {
			return "", false
		}
	}
	if size <= 0 {
		return "", true
	}
	buf := newOSBuffer(size)
	if failed, _ := dl.Call(getter, handle, uintptr(buf.pointer()), uintptr(unsafe.Pointer(&size))); failed != 0 {
		r.release(failed)
		return "", false
	}
	return buf.text(), true
}

func (r *brokerRuntime) takeError(handle uintptr) *brokerError {
	e := &brokerError{status: r.statusOf(handle)}
	if failed, _ := dl.Call(r.getErrorCode, handle, uintptr(unsafe.Pointer(&e.code))); failed != 0 {
		r.release(failed)
	}
	if failed, _ := dl.Call(r.getTag, handle, uintptr(unsafe.Pointer(&e.tag))); failed != 0 {
		r.release(failed)
	}
	e.context, _ = r.readString(r.getContext, handle)
	r.release(handle)
	return e
}

func (r *brokerRuntime) check(handle uintptr) error {
	if handle == 0 {
		return nil
	}
	return r.takeError(handle)
}

func (r *brokerRuntime) discardAuthResult(result uintptr) {
	handle, _ := dl.Call(r.releaseAuthResult, result)
	r.release(handle)
}

func (r *brokerRuntime) discardReadResult(result uintptr) {
	handle, _ := dl.Call(r.releaseReadAccountResult, result)
	r.release(handle)
}

func (r *brokerRuntime) await(ctx context.Context, ch chan uintptr, async uintptr, discard func(result uintptr)) (uintptr, error) {
	defer func() {
		if async != 0 {
			handle, _ := dl.Call(r.releaseAsyncHandle, async)
			r.release(handle)
		}
	}()
	select {
	case result := <-ch:
		return result, nil
	case <-ctx.Done():
	}
	if async != 0 {
		handle, _ := dl.Call(r.cancelAsyncOperation, async)
		r.release(handle)
	}
	timer := time.NewTimer(brokerCancelGrace)
	defer timer.Stop()
	select {
	case result := <-ch:
		return result, ctx.Err()
	case <-timer.C:
		go func() { discard(<-ch) }()
		return 0, ctx.Err()
	}
}

func (r *brokerRuntime) parameters(clientID, authority, scope string, trusted bool) (uintptr, error) {
	var params uintptr
	client, issuer := encodeOS(clientID), encodeOS(authority)
	handle, _ := dl.Call(r.createAuthParameters, uintptr(client.pointer()), uintptr(issuer.pointer()), uintptr(unsafe.Pointer(&params)))
	if err := r.check(handle); err != nil {
		return 0, err
	}
	scopes := encodeOS(scope)
	handle, _ = dl.Call(r.setRequestedScopes, params, uintptr(scopes.pointer()))
	if err := r.check(handle); err != nil {
		r.freeParameters(params)
		return 0, err
	}
	if trusted {
		key, value := encodeOS("validateAuthority"), encodeOS("no")
		handle, _ = dl.Call(r.setAdditionalParameter, params, uintptr(key.pointer()), uintptr(value.pointer()))
		if err := r.check(handle); err != nil {
			r.freeParameters(params)
			return 0, err
		}
	}
	return params, nil
}

func (r *brokerRuntime) freeParameters(params uintptr) {
	handle, _ := dl.Call(r.releaseAuthParameters, params)
	r.release(handle)
}

func (r *brokerRuntime) authResult(result uintptr) (accessToken, string, error) {
	defer r.discardAuthResult(result)
	var failure uintptr
	if err := r.check(firstResult(dl.Call(r.getError, result, uintptr(unsafe.Pointer(&failure))))); err != nil {
		return accessToken{}, "", err
	}
	if failure != 0 {
		return accessToken{}, "", r.takeError(failure)
	}
	value, ok := r.readString(r.getAccessToken, result)
	if !ok || value == "" {
		return accessToken{}, "", errors.New("the account broker returned no access token")
	}
	var expires int64
	if err := r.check(firstResult(dl.Call(r.getExpiresOn, result, uintptr(unsafe.Pointer(&expires))))); err != nil {
		return accessToken{}, "", err
	}
	accountID := ""
	var account uintptr
	if handle, _ := dl.Call(r.getAccount, result, uintptr(unsafe.Pointer(&account))); handle != 0 {
		r.release(handle)
	} else if account != 0 {
		accountID, _ = r.readString(r.getAccountID, account)
		handle, _ := dl.Call(r.releaseAccount, account)
		r.release(handle)
	}
	return accessToken{value: value, expiresOn: expires}, accountID, nil
}

func firstResult(r1, _ uintptr) uintptr { return r1 }

func (r *brokerRuntime) signInSilently(ctx context.Context, clientID, authority, scope string, trusted bool) (accessToken, string, error) {
	params, err := r.parameters(clientID, authority, scope, trusted)
	if err != nil {
		return accessToken{}, "", err
	}
	defer r.freeParameters(params)
	id, ch := brokerHost.register()
	correlation := encodeOS(uuid.NewString())
	var async uintptr
	handle, _ := dl.Call(r.signInSilentlyAsync, params, uintptr(correlation.pointer()), brokerHost.complete, id, uintptr(unsafe.Pointer(&async)))
	if err := r.check(handle); err != nil {
		brokerHost.forget(id)
		return accessToken{}, "", err
	}
	result, err := r.await(ctx, ch, async, r.discardAuthResult)
	if result == 0 {
		return accessToken{}, "", err
	}
	token, accountID, resultErr := r.authResult(result)
	if err != nil {
		return accessToken{}, "", err
	}
	return token, accountID, resultErr
}

func (r *brokerRuntime) readAccount(ctx context.Context, accountID string) (uintptr, error) {
	id, ch := brokerHost.register()
	account, correlation := encodeOS(accountID), encodeOS(uuid.NewString())
	var async uintptr
	handle, _ := dl.Call(r.readAccountByIDAsync, uintptr(account.pointer()), uintptr(correlation.pointer()), brokerHost.complete, id, uintptr(unsafe.Pointer(&async)))
	if err := r.check(handle); err != nil {
		brokerHost.forget(id)
		return 0, err
	}
	result, err := r.await(ctx, ch, async, r.discardReadResult)
	if result == 0 {
		return 0, err
	}
	defer r.discardReadResult(result)
	if err != nil {
		return 0, err
	}
	var failure uintptr
	if err := r.check(firstResult(dl.Call(r.getReadAccountError, result, uintptr(unsafe.Pointer(&failure))))); err != nil {
		return 0, err
	}
	if failure != 0 {
		return 0, r.takeError(failure)
	}
	var found uintptr
	if err := r.check(firstResult(dl.Call(r.getReadAccount, result, uintptr(unsafe.Pointer(&found))))); err != nil {
		return 0, err
	}
	if found == 0 {
		return 0, errors.New("the account broker no longer holds the signed-in account")
	}
	return found, nil
}

func (r *brokerRuntime) acquireSilently(ctx context.Context, clientID, authority, scope, accountID string, trusted bool) (accessToken, string, error) {
	account, err := r.readAccount(ctx, accountID)
	if err != nil {
		return accessToken{}, "", err
	}
	defer func() {
		handle, _ := dl.Call(r.releaseAccount, account)
		r.release(handle)
	}()
	params, err := r.parameters(clientID, authority, scope, trusted)
	if err != nil {
		return accessToken{}, "", err
	}
	defer r.freeParameters(params)
	id, ch := brokerHost.register()
	correlation := encodeOS(uuid.NewString())
	var async uintptr
	handle, _ := dl.Call(r.acquireTokenSilentlyAsync, params, uintptr(correlation.pointer()), account, brokerHost.complete, id, uintptr(unsafe.Pointer(&async)))
	if err := r.check(handle); err != nil {
		brokerHost.forget(id)
		return accessToken{}, "", err
	}
	result, err := r.await(ctx, ch, async, r.discardAuthResult)
	if result == 0 {
		return accessToken{}, "", err
	}
	token, next, resultErr := r.authResult(result)
	if err != nil {
		return accessToken{}, "", err
	}
	return token, next, resultErr
}

type broker struct {
	identity  *Identity
	label     string
	clientID  string
	authority string
	trusted   bool
	platform  string
	prefix    string

	mu        sync.Mutex
	accountID string
}

func (s *broker) name() string { return s.label }

func trustedAuthority(authority, tenant string) bool {
	trusted := tenant == "adfs"
	for _, suffix := range b2cHostSuffixes {
		trusted = trusted || strings.HasSuffix(strings.ToLower(strings.TrimPrefix(authority, "https://")), suffix)
	}
	return trusted
}

func (i *Identity) brokerSource(tenant, authority string) source {
	if tenant == "" {
		tenant = brokerDefaultTenant
	}
	return &broker{identity: i, label: nameBroker, clientID: developerClientID, authority: authority + "/" + tenant,
		trusted: trustedAuthority(authority, tenant), platform: brokerPlatformOnly, prefix: brokerPrefix}
}

func (s *broker) runtime(ctx context.Context) (*brokerRuntime, error) {
	if !brokerSupported(s.identity.host) {
		return nil, unavailable(s.label, s.platform)
	}
	if s.identity.toolchain == nil {
		return nil, unavailable(s.label, s.prefix+"The account broker library store is not configured.")
	}
	install, err := s.identity.toolchain.Ensure(ctx, toolchain.MsalRuntime, toolchain.EnsureOptions{})
	if err != nil {
		return nil, &UnavailableError{Credential: s.label, Message: s.prefix + "The account broker library could not be installed: " + err.Error(), Err: err}
	}
	loaded, err := brokerHost.load(install.Binary)
	if err != nil {
		return nil, &UnavailableError{Credential: s.label, Message: s.prefix + "The account broker library could not be started: " + err.Error(), Err: err}
	}
	return loaded, nil
}

func (s *broker) token(ctx context.Context, scope string, inChain bool) (accessToken, error) {
	loaded, err := s.runtime(ctx)
	if err != nil {
		return accessToken{}, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.accountID != "" {
		if token, next, err := loaded.acquireSilently(ctx, s.clientID, s.authority, scope, s.accountID, s.trusted); err == nil {
			if next != "" {
				s.accountID = next
			}
			return token, nil
		}
	}
	token, accountID, err := loaded.signInSilently(ctx, s.clientID, s.authority, scope, s.trusted)
	if err != nil {
		var native *brokerError
		if errors.As(err, &native) && (strings.Contains(native.context, "AADSTS50011") || strings.Contains(native.context, "AADSTS7000218")) {
			return accessToken{}, &AuthError{Credential: s.label, Message: "MsalRuntime needs the current app to register these redirect_uri (1) ms-appx-web://Microsoft.AAD.BrokerPlugin/" + s.clientID + " (2) msauth.com.msauth.unsignedapp://auth", Err: err}
		}
		message := err.Error()
		if message == "" {
			message = "Failed to authenticate user"
		}
		if inChain {
			return accessToken{}, &UnavailableError{Credential: s.label, Message: message, Err: err}
		}
		return accessToken{}, &AuthError{Credential: s.label, Message: message, Err: err}
	}
	s.accountID = accountID
	return token, nil
}
