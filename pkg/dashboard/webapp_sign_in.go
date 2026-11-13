package dashboard

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/nickheyer/icebath/pkg/internal/pycodecs"
	"github.com/nickheyer/icebath/pkg/internal/pyfs"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pypath"
)

const webappRefreshTokenLifetime = 30 * 24 * 60 * 60

// Record of native sign in token digests under the gateway home
var webappRecordPath = filepath.Join("webapp", "native-sign-in-tokens.json")

var (
	webappRecordsMu sync.Mutex
	webappRecords   = map[string]*pyjson.Object{}
)

func webappSignInLog() *slog.Logger {
	return slog.Default().With("logger", "hermes_cli.webapp_sign_in")
}

// Sign in cookie value for a browser session token, the hex HMAC of the sign in message
func webappSignInBinding(token string) string {
	mac := hmac.New(sha256.New, []byte(token))
	mac.Write([]byte("webapp-sign-in"))
	return hex.EncodeToString(mac.Sum(nil))
}

// Whether the request reaches the gateway's Webapp behind the sign in gate
func webappSignInApplies(conn *HTTPConnection) bool {
	state := conn.App().State
	return state.String("ui_surface") == "webapp" && state.Bool("auth_required")
}

// Whether the cookie login is bound to the credential in use now, recording its generation on the request state
func webappSignInCheck(conn *HTTPConnection) bool {
	token := BrowserCredentialCurrent()
	presented, _ := readWebappSigninCookie(conn, webappGateway(conn))
	if subtle.ConstantTimeCompare([]byte(presented), []byte(webappSignInBinding(token))) != 1 {
		return false
	}
	conn.State().Set("webapp_sign_in", BrowserCredentialGenerationOf(token))
	return true
}

// Credential generation the request's login was checked against, nil off the Webapp's gate
func webappSignInOf(conn *HTTPConnection) *string {
	if v, ok := conn.State().Get("webapp_sign_in").(string); ok {
		return &v
	}
	return nil
}

func webappSignInSet(resp ASGIResponse, conn *HTTPConnection, token string) {
	setWebappSigninCookie(resp, webappSignInBinding(token), detectHTTPS(conn), PrefixFromRequest(conn), webappGateway(conn))
}

// Binds the login the request just completed to the credential in use now
func webappSignInBind(resp ASGIResponse, conn *HTTPConnection) {
	if webappSignInApplies(conn) {
		webappSignInSet(resp, conn, BrowserCredentialCurrent())
	}
}

// Keeps the binding as long lived as freshly rotated session cookies unless the credential was replaced meanwhile
func webappSignInRenew(resp ASGIResponse, conn *HTTPConnection) {
	if token, ok := BrowserCredentialCurrentIf(webappSignInOf(conn)); ok {
		webappSignInSet(resp, conn, token)
	}
}

// Whether a provider token works alone here, behind the Webapp's gate only when given to a native app
func webappTokenWorksAlone(conn *HTTPConnection, token string) bool {
	if !webappSignInApplies(conn) {
		return true
	}
	webappRecordsMu.Lock()
	defer webappRecordsMu.Unlock()
	expires, ok := webappLoaded(webappHome(conn)).Vals[webappDigest(token)].(int64)
	return ok && float64(expires) > float64(time.Now().UnixNano())/1e9
}

// Records that this listener gave the session's tokens to a native app's sign in
func webappGaveNativeApp(ctx context.Context, conn *HTTPConnection, accessToken, refreshToken string, expiresAt int64) {
	if !webappSignInApplies(conn) {
		return
	}
	now := time.Now().Unix()
	atExpires := expiresAt
	if atExpires == 0 {
		atExpires = now + webappRefreshTokenLifetime
	}
	home := webappHome(conn)
	webappRecordsMu.Lock()
	defer webappRecordsMu.Unlock()
	records := webappLoaded(home)
	for _, given := range []struct {
		token   string
		expires int64
	}{{accessToken, atExpires}, {refreshToken, now + webappRefreshTokenLifetime}} {
		if given.token != "" {
			records.Set(webappDigest(given.token), given.expires)
		}
	}
	for _, key := range append([]string(nil), records.Keys...) {
		if v, _ := records.Vals[key].(int64); v <= now {
			records.Delete(key)
		}
	}
	if home != "" {
		webappWriteRecords(ctx, filepath.Join(home, webappRecordPath), records)
	}
}

func webappDigest(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}

// Gateway home the Webapp's listener serves, empty without one
func webappHome(conn *HTTPConnection) string {
	return conn.App().State.String("webapp_home")
}

// Suffix the gateway's Webapp gives every sign in cookie, empty off the Webapp
func webappCookieGateway(conn *HTTPConnection) string {
	if conn.App().State.String("ui_surface") == "webapp" {
		return webappGateway(conn)
	}
	return ""
}

// Gateway name in its sign in cookies, a digest of its absolute home, empty without a home
func webappGateway(conn *HTTPConnection) string {
	home := webappHome(conn)
	if home == "" {
		return ""
	}
	abs := pypath.PathStr(home)
	if wd, err := os.Getwd(); err == nil && !filepath.IsAbs(abs) {
		abs = pypath.PathStr(wd + string(filepath.Separator) + abs)
	}
	sum := sha256.Sum256([]byte(abs))
	return hex.EncodeToString(sum[:])[:16]
}

// Home's record read from its file on first use, in memory only without a home, called with the lock held
func webappLoaded(home string) *pyjson.Object {
	if records, ok := webappRecords[home]; ok {
		return records
	}
	records := pyjson.NewObject()
	if home != "" {
		raw, err := os.ReadFile(filepath.Join(home, webappRecordPath))
		var text string
		if err == nil {
			text, err = pycodecs.Decode(raw, "utf-8-sig")
		}
		switch {
		case errors.Is(err, fs.ErrNotExist):
		case err != nil:
			webappSignInLog().Warn(fmt.Sprintf("Could not read the Webapp's record of native sign-ins: %s", pypath.OSErrorText(err)))
		default:
			stored, err := pyjson.Loads(text)
			if err != nil {
				webappSignInLog().Warn(fmt.Sprintf("Could not read the Webapp's record of native sign-ins: %s", err))
			} else if obj, ok := stored.(*pyjson.Object); ok {
				for _, k := range obj.Keys {
					if n, ok := obj.Vals[k].(int64); ok {
						records.Set(k, n)
					}
				}
			}
		}
	}
	webappRecords[home] = records
	return records
}

// Saves the native sign in record owner only, a failure leaving it in memory for this process
func webappWriteRecords(ctx context.Context, path string, records *pyjson.Object) {
	err := os.MkdirAll(filepath.Dir(path), 0o700)
	if err == nil {
		var text string
		if text, err = pyjson.Dumps(records, pyjson.Separators(",", ":")); err == nil {
			err = pyfs.AtomicWriteTextMode(ctx, path, text+"\n", "", 0o600)
		}
	}
	if err != nil {
		webappSignInLog().Warn(fmt.Sprintf("Could not save the Webapp's record of native sign-ins: %s", err))
	}
}
