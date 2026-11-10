package gateway

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pypath"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
)

const (
	ControlProtocolVersion      = 1
	controlSocketFilename       = "gateway.sock"
	controlPointerFilename      = "gateway.sock.path"
	maxControlResponseBytes     = 512 * 1024
	DefaultControlClientTimeout = 2 * time.Second
)

var errSurrogateEncode = errors.New("'utf-8' codec can't encode characters: surrogates not allowed")

// Sixteen hex digits of the sha256 of the expanded, resolved and normcased home
func controlHomeHash(home string) (string, error) {
	resolved, err := pypath.Resolve(pypath.Expanduser(home))
	if err != nil {
		return "", err
	}
	norm := pypath.Normcase(resolved)
	if !utf8.ValidString(norm) {
		return "", errSurrogateEncode
	}
	sum := sha256.Sum256([]byte(norm))
	return hex.EncodeToString(sum[:])[:16], nil
}

// Per home named pipe path of the Windows control transport
func WindowsPipeName(home string) (string, error) {
	h, err := controlHomeHash(home)
	if err != nil {
		return "", err
	}
	return `\\.\pipe\icebath-gateway-` + h, nil
}

// Short socket path under the private runtime dir, the temp dir or /tmp, the first candidate when none fits
func fallbackSocketPath(home string) (string, error) {
	h, err := controlHomeHash(home)
	if err != nil {
		return "", err
	}
	name := filepath.Join("icebath-gw-"+h, "control.sock")
	var candidates []string
	if rt := privateRuntimeDir(); rt != "" {
		candidates = append(candidates, filepath.Join(rt, name))
	}
	tmp, err := pypath.Gettempdir()
	if err != nil {
		return "", err
	}
	candidates = append(candidates, filepath.Join(tmp, name))
	if runtime.GOOS != "windows" {
		candidates = append(candidates, filepath.Join("/tmp", name))
	}
	for _, c := range candidates {
		fits, err := fitsSunPath(c)
		if err != nil {
			return "", err
		}
		if fits {
			return c, nil
		}
	}
	return candidates[0], nil
}

// Bind path and pointer file of the server socket, the pointer set only for the short fallback path
func ResolveServerSocketPath(home string) (bindPath, pointerFile string, err error) {
	direct := filepath.Join(home, controlSocketFilename)
	fits, err := fitsSunPath(direct)
	if err != nil {
		return "", "", err
	}
	if fits {
		return direct, "", nil
	}
	fallback, err := fallbackSocketPath(home)
	if err != nil {
		return "", "", err
	}
	return fallback, filepath.Join(home, controlPointerFilename), nil
}

// Socket a client connects to for home, empty when neither the direct socket nor a live pointer target exists
func ResolveClientSocketPath(home string) (string, error) {
	direct := filepath.Join(home, controlSocketFilename)
	if _, err := os.Stat(direct); err == nil {
		return direct, nil
	}
	pointer := filepath.Join(home, controlPointerFilename)
	info, err := os.Stat(pointer)
	if err != nil || !info.Mode().IsRegular() {
		return "", nil
	}
	raw, err := os.ReadFile(pointer)
	if err != nil {
		return "", nil
	}
	if !utf8.Valid(raw) {
		return "", errors.New("'utf-8' codec can't decode the control socket pointer")
	}
	target := pystr.Strip(strings.TrimPrefix(string(raw), "\ufeff"))
	if target == "" {
		return "", nil
	}
	if _, err := os.Stat(target); err == nil {
		return target, nil
	}
	return "", nil
}

// result payload of a control verb answered ok true by the gateway serving home, nil on any failure
func QueryGatewayControl(home, verb string, params *pyjson.Object, timeout time.Duration) *pyjson.Object {
	payload := pyjson.NewObject()
	payload.Set("verb", verb)
	payload.Set("id", int64(1))
	payload.Set("protocol", int64(ControlProtocolVersion))
	if params != nil && params.Len() > 0 {
		payload.Set("params", params)
	}
	text, err := pyjson.Dumps(payload)
	if err != nil {
		return nil
	}
	raw, err := controlQuery(home, append([]byte(text), '\n'), timeout)
	if err != nil || len(raw) == 0 || !utf8.Valid(raw) {
		return nil
	}
	response, err := pyjson.Loads(string(raw))
	if err != nil {
		return nil
	}
	obj, ok := response.(*pyjson.Object)
	if !ok {
		return nil
	}
	if okv, _ := obj.Get("ok"); okv != true {
		return nil
	}
	result, _ := obj.Get("result")
	r, _ := result.(*pyjson.Object)
	return r
}

// Chunks up to a newline, EOF, the deadline or the size cap, nil past the cap or for an empty line
func readResponseLine(read func() ([]byte, error), deadline time.Time) ([]byte, error) {
	var buf []byte
	for time.Now().Before(deadline) {
		chunk, err := read()
		if err != nil {
			return nil, err
		}
		buf = append(buf, chunk...)
		if len(chunk) == 0 || strings.Contains(string(chunk), "\n") {
			break
		}
		if len(buf) > maxControlResponseBytes {
			return nil, nil
		}
	}
	if i := strings.IndexByte(string(buf), '\n'); i >= 0 {
		buf = buf[:i]
	}
	if len(buf) == 0 {
		return nil, nil
	}
	return buf, nil
}

// identify answer of the gateway serving home
func IdentifyGateway(home string, timeout time.Duration) *pyjson.Object {
	return QueryGatewayControl(home, "identify", nil, timeout)
}

// Drain and exit acknowledgement of the gateway serving home for an update
func PauseGatewayForUpdate(home string, timeout time.Duration) *pyjson.Object {
	return QueryGatewayControl(home, "pause-for-update", nil, timeout)
}

// Asks the multiplexer serving home to reconcile profiles now
func RescanGatewayProfiles(home string, timeout time.Duration) *pyjson.Object {
	return QueryGatewayControl(home, "rescan-profiles", nil, timeout)
}

// Context aware form of the rescan request the profile and status callers use
func rescanGatewayProfiles(_ context.Context, root string, timeout time.Duration) (any, error) {
	if r := RescanGatewayProfiles(root, timeout); r != nil {
		return r, nil
	}
	return nil, nil
}

// Asks the multiplexer serving home to stop serving name
func RequestUnserveProfile(home, name string) *pyjson.Object {
	return QueryGatewayControl(home, "unserve-profile", controlParams("name", name), 8*time.Second)
}

// Asks the multiplexer serving home to hot serve name
func RequestServeProfileHot(home, name string) *pyjson.Object {
	return QueryGatewayControl(home, "serve-profile", controlParams("name", name), 8*time.Second)
}

// Asks the multiplexer serving home to rekey a renamed profile routing from old to new
func MigrateGatewayProfileIdentity(home, oldName, newName string, timeout time.Duration) *pyjson.Object {
	return QueryGatewayControl(home, "migrate-profile-identity", controlParams("old", oldName, "new", newName), timeout)
}

// Asks the multiplexer serving home to drop a deleted profile routing identity in memory and on disk
func PurgeGatewayProfileIdentity(home, name string, timeout time.Duration) *pyjson.Object {
	return QueryGatewayControl(home, "purge-profile-identity", controlParams("name", name), timeout)
}

// Asks the gateway serving home to rediscover plugins for profileHome, home when empty
func ReloadGatewayPlugins(home, profileHome string, timeout time.Duration) *pyjson.Object {
	if profileHome == "" {
		profileHome = home
	}
	return QueryGatewayControl(home, "reload-plugins", controlParams("home", profileHome), timeout)
}

// Ordered params object from alternating keys and values
func controlParams(kv ...string) *pyjson.Object {
	o := pyjson.NewObject()
	for i := 0; i+1 < len(kv); i += 2 {
		o.Set(kv[i], kv[i+1])
	}
	return o
}

// Control answer as an interface value, untyped nil when no gateway answered
func controlAnswer(r *pyjson.Object) any {
	if r == nil {
		return nil
	}
	return r
}
