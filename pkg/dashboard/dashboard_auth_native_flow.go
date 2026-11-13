package dashboard

import (
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"sync"
	"time"

	"github.com/nickheyer/icebath/pkg/gateway"
)

const (
	nativePendingTTLSeconds = 600
	nativeCodeTTLSeconds    = 120
	nativeMaxEntries        = 256
	nativeMaxPendingPerIP   = 8
)

// In flight native authorization awaiting the upstream callback
type NativePending struct {
	CodeChallenge string
	RedirectURI   string
	ClientState   string
	ClientIP      string
	ExpiresAt     int64
}

// Minted one time gateway authorization code bound to a session
type nativeIssuedCode struct {
	codeChallenge string
	session       *gateway.DashboardSession
	expiresAt     int64
}

var (
	nativeMu      sync.Mutex
	nativePending = map[string]*NativePending{}
	nativeIssued  = map[string]*nativeIssuedCode{}
)

// Native flow failure, a bad, expired or replayed handle or a PKCE mismatch
type NativeFlowError struct {
	Msg  string
	Kind string
}

func (e *NativeFlowError) Error() string { return e.Msg }
func (e *NativeFlowError) PyTypeName() string {
	if e.Kind != "" {
		return e.Kind
	}
	return "NativeFlowError"
}

// Unknown or expired broker state
func pendingNotFound(msg string) error { return &NativeFlowError{Msg: msg, Kind: "PendingNotFound"} }

// Unknown, expired, redeemed or PKCE mismatched gateway code
func codeInvalid(msg string) error { return &NativeFlowError{Msg: msg, Kind: "CodeInvalid"} }

// RFC 7636 S256 transform of a verifier, unpadded base64url of its SHA-256
func s256(verifier string) string {
	sum := sha256.Sum256([]byte(verifier))
	return base64.RawURLEncoding.EncodeToString(sum[:])
}

// Drops expired pending and issued entries with the lock held
func nativeGCLocked(now int64) {
	for k, v := range nativePending {
		if v.ExpiresAt < now {
			delete(nativePending, k)
		}
	}
	for k, v := range nativeIssued {
		if v.expiresAt < now {
			delete(nativeIssued, k)
		}
	}
}

func nativeCapacityOKLocked() bool { return len(nativePending)+len(nativeIssued) < nativeMaxEntries }

// Explicit clock value or the current Unix second
func nativeNow(now *int64) int64 {
	if now == nil {
		return time.Now().Unix()
	}
	return *now
}

// Pending entry of a broker state, removed when consume is set, PendingNotFound when unknown
func nativePopPendingLocked(brokerState string, consume bool) (*NativePending, error) {
	entry, ok := nativePending[brokerState]
	if !ok {
		return nil, pendingNotFound("unknown or expired native authorization")
	}
	if consume {
		delete(nativePending, brokerState)
	}
	return entry, nil
}

// Stashes a pending native authorization and returns its opaque broker state, refused at capacity or per address cap
func RegisterNativePending(codeChallenge, redirectURI, clientState, clientIP string, now *int64) (string, error) {
	t := nativeNow(now)
	brokerState := tokenURLSafe(32)
	nativeMu.Lock()
	defer nativeMu.Unlock()
	nativeGCLocked(t)
	if !nativeCapacityOKLocked() {
		return "", &NativeFlowError{Msg: "native-flow authorization store at capacity"}
	}
	perIP := 0
	for _, v := range nativePending {
		if v.ClientIP == clientIP {
			perIP++
		}
	}
	if clientIP != "" && perIP >= nativeMaxPendingPerIP {
		return "", &NativeFlowError{Msg: "too many pending native authorizations from this address"}
	}
	nativePending[brokerState] = &NativePending{CodeChallenge: codeChallenge, RedirectURI: redirectURI,
		ClientState: clientState, ClientIP: clientIP, ExpiresAt: t + nativePendingTTLSeconds}
	return brokerState, nil
}

// Pending authorization of a broker state without consuming it
func GetNativePending(brokerState string, now *int64) (*NativePending, error) {
	nativeMu.Lock()
	defer nativeMu.Unlock()
	nativeGCLocked(nativeNow(now))
	return nativePopPendingLocked(brokerState, false)
}

// Consumes a pending authorization and mints a one time code bound to the desktop challenge and session
func CompleteNativePending(brokerState string, session *gateway.DashboardSession, now *int64) (string, error) {
	t := nativeNow(now)
	nativeMu.Lock()
	defer nativeMu.Unlock()
	nativeGCLocked(t)
	pending, err := nativePopPendingLocked(brokerState, true)
	if err != nil {
		return "", err
	}
	if !nativeCapacityOKLocked() {
		return "", &NativeFlowError{Msg: "native-flow code store at capacity"}
	}
	code := tokenURLSafe(32)
	nativeIssued[code] = &nativeIssuedCode{codeChallenge: pending.CodeChallenge, session: session, expiresAt: t + nativeCodeTTLSeconds}
	return code, nil
}

// Verifies PKCE and consumes a gateway code, popped first so a wrong verifier cannot retry
func RedeemNativeCode(code, codeVerifier string, now *int64) (*gateway.DashboardSession, error) {
	t := nativeNow(now)
	nativeMu.Lock()
	nativeGCLocked(t)
	issued, ok := nativeIssued[code]
	delete(nativeIssued, code)
	nativeMu.Unlock()
	if !ok {
		return nil, codeInvalid("unknown, expired, or already-redeemed code")
	}
	if issued.expiresAt < t {
		return nil, codeInvalid("code expired")
	}
	if subtle.ConstantTimeCompare([]byte(issued.codeChallenge), []byte(s256(codeVerifier))) != 1 {
		return nil, codeInvalid("PKCE verification failed")
	}
	return issued.session, nil
}

// Drops every pending and issued entry
func resetNativeFlow() {
	nativeMu.Lock()
	defer nativeMu.Unlock()
	clear(nativePending)
	clear(nativeIssued)
}
