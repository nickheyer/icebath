package tuiserver

import (
	"context"
	"encoding/json"
	"errors"
	"image"
	_ "image/gif"
	_ "image/jpeg"
	_ "image/png"
	"os"
	"path/filepath"
	"sync"

	_ "golang.org/x/image/webp"

	"github.com/nickheyer/icebath/pkg/provider"
	"github.com/nickheyer/icebath/pkg/tuihost"
	jsonutil "github.com/nickheyer/icebath/pkg/utils/json"
)

// JSON RPC handler of one registered method
type Handler func(ctx context.Context, rid any, params map[string]any) map[string]any

// Registered JSON RPC methods by name
var (
	methodsMu sync.RWMutex
	methods   = map[string]Handler{}
)

// Successful JSON RPC response
func ok(rid any, result any) map[string]any {
	return map[string]any{"jsonrpc": "2.0", "id": rid, "result": result}
}

// JSON RPC error response, data only when given
func rpcErr(rid any, code int, msg string, data any) map[string]any {
	errObj := map[string]any{"code": code, "message": msg}
	if data != nil {
		errObj["data"] = data
	}
	return map[string]any{"jsonrpc": "2.0", "id": rid, "error": errObj}
}

// Registers a JSON RPC method, the one registration seam every handler module goes through
func registerMethod(name string, fn Handler) {
	methodsMu.Lock()
	defer methodsMu.Unlock()
	methods[name] = fn
}

// Registered handler of a method name, nil when none
func lookupMethod(name string) Handler {
	methodsMu.RLock()
	defer methodsMu.RUnlock()
	return methods[name]
}

// Request id, method and params of a JSON RPC request valid enough for local dispatch, else its error response
func normalizeRequest(req any) (rid any, method string, params map[string]any, errResp map[string]any) {
	obj, isDict := req.(map[string]any)
	if !isDict {
		return nil, "", nil, rpcErr(nil, -32600, "invalid request: expected an object", nil)
	}
	rid = obj["id"]
	method, _ = obj["method"].(string)
	if method == "" {
		return rid, "", nil, rpcErr(rid, -32600, "invalid request: method must be a non-empty string", nil)
	}
	raw, present := obj["params"]
	if !present || raw == nil {
		return rid, method, map[string]any{}, nil
	}
	params, isDict = raw.(map[string]any)
	if !isDict {
		return rid, "", nil, rpcErr(rid, -32602, "invalid params: expected an object", nil)
	}
	return rid, method, params, nil
}

// Attachment name with its pixel size and a rough tile based token estimate when the image decodes
func imageMeta(path string) map[string]any {
	meta := map[string]any{"name": filepath.Base(path)}
	f, err := os.Open(path)
	if err != nil {
		return meta
	}
	defer f.Close()
	cfg, _, err := image.DecodeConfig(f)
	if err != nil {
		return meta
	}
	width, height := cfg.Width, cfg.Height
	tiles := 0
	if width > 0 && height > 0 {
		tiles = max(1, (width+511)/512) * max(1, (height+511)/512)
	}
	meta["width"], meta["height"], meta["token_estimate"] = width, height, tiles*85
	return meta
}

// Blocking server to client string request every bridge goes through
var askRequest = ask

// Server to client request answered by one string, empty when skipped, timed out or cancelled
func ask(ctx context.Context, method, sid string, params map[string]any, timeout *float64) string {
	result := sendResult(ctx, tuihost.Send)(method, sid, objectOf(params), timeout, nil)
	if result == nil {
		return ""
	}
	value, present := result.Get("value")
	if !present {
		return ""
	}
	if text, isText := value.(string); isText {
		return text
	}
	encoded, err := jsonutil.Encode(value, jsonutil.Spaced())
	if err != nil {
		panic(err)
	}
	return encoded
}

// Ordered JSON object of a payload map
func objectOf(payload map[string]any) *jsonutil.Object {
	raw, err := json.Marshal(payload)
	if err != nil {
		panic(err)
	}
	obj, err := jsonutil.DecodeObject(string(raw))
	if err != nil {
		panic(err)
	}
	return obj
}

// is_truthy_value of utils, strings truthy only when they spell an affirmative word
func isTruthyValue(v any) bool { return provider.IsTruthyValue(v) }

// Server request sender of the request helpers, its answer nil when the wait was cancelled or timed out
type requestSender func(ctx context.Context, method, sid string, params *jsonutil.Object, timeout *float64, qids []string) (*jsonutil.Object, error)

// Sender whose cancelled wait reads as no answer and whose registration failure raises
func sendResult(ctx context.Context, send requestSender) func(method, sid string, params *jsonutil.Object, timeout *float64, qids []string) *jsonutil.Object {
	return func(method, sid string, params *jsonutil.Object, timeout *float64, qids []string) *jsonutil.Object {
		result, err := send(ctx, method, sid, params, timeout, qids)
		if err != nil {
			if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
				return nil
			}
			panic(err)
		}
		return result
	}
}
