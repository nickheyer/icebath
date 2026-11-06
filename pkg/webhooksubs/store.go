package webhooksubs

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"unicode/utf8"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
)

const (
	subscriptionsFilename     = "webhook_subscriptions.json"
	subscriptionsFileMode     = 0o600
	subscriptionsLockFilename = ".webhook_subscriptions.lock"
)

// Serializes snapshots and mutations inside this process before the cross process file lock
var subscriptionsThreadLock sync.Mutex

type missingSubscription struct{}

// Sentinel expected value of Replace for a route that must not exist yet
var Missing any = missingSubscription{}

// Route changed after the caller took the snapshot it meant to replace
type SubscriptionMutationConflict struct{ Msg string }

func (e *SubscriptionMutationConflict) Error() string { return e.Msg }

// Python exception class name the error stands for
func (e *SubscriptionMutationConflict) PyTypeName() string { return "SubscriptionMutationConflict" }

// Lock file that could not be opened or locked, the RuntimeError of the file lock
type LockUnavailableError struct {
	Path string
	Err  error
}

func (e *LockUnavailableError) Error() string { return "file lock unavailable: " + e.Path }

func (e *LockUnavailableError) Unwrap() error { return e.Err }

// Subscription store file of the home
func Path(h string) string { return filepath.Join(h, subscriptionsFilename) }

// Lock file beside the subscription store
func LockPath(h string) string { return filepath.Join(h, subscriptionsLockFilename) }

// Takes the thread lock then the store file lock and returns their release
func Lock(h string) (func(), error) {
	subscriptionsThreadLock.Lock()
	p := LockPath(h)
	fail := func(err error) (func(), error) {
		subscriptionsThreadLock.Unlock()
		return nil, &LockUnavailableError{Path: p, Err: err}
	}
	if err := os.MkdirAll(filepath.Dir(p), 0o777); err != nil {
		subscriptionsThreadLock.Unlock()
		return nil, err
	}
	f, err := os.OpenFile(p, os.O_RDWR|os.O_CREATE|os.O_APPEND, 0o666)
	if err != nil {
		subscriptionsThreadLock.Unlock()
		return nil, err
	}
	if err := lockFile(f); err != nil {
		f.Close()
		return fail(err)
	}
	return func() {
		unlockFile(f)
		f.Close()
		subscriptionsThreadLock.Unlock()
	}, nil
}

// One complete store snapshot in file order, unreadable or malformed files reading as empty
func LoadUnlocked(h string) *pyjson.Object {
	raw, err := os.ReadFile(Path(h))
	if err != nil {
		return pyjson.NewObject()
	}
	raw = bytes.TrimPrefix(raw, []byte("\xef\xbb\xbf"))
	if !utf8.Valid(raw) {
		return pyjson.NewObject()
	}
	data, err := pyjson.Loads(string(raw))
	if err != nil {
		return pyjson.NewObject()
	}
	if obj, ok := data.(*pyjson.Object); ok {
		return obj
	}
	return pyjson.NewObject()
}

// Lock free read, writers publish by atomic rename so a reader always sees one whole snapshot
func Load(h string) *pyjson.Object { return LoadUnlocked(h) }

// Publishes the snapshot atomically as two space indented JSON with owner only mode
func SaveUnlocked(h string, subs *pyjson.Object) error {
	text, err := pyjson.Dumps(subs, pyjson.Indent(2), pyjson.EnsureASCII(false))
	if err != nil {
		return err
	}
	if !utf8.ValidString(text) {
		if text, err = pyjson.Dumps(subs, pyjson.Indent(2), pyjson.EnsureASCII(true)); err != nil {
			return err
		}
	}
	p := Path(h)
	if err := config.AtomicWrite(p, []byte(text), subscriptionsFileMode); err != nil {
		return err
	}
	return os.Chmod(p, subscriptionsFileMode)
}

// Locks, rereads, mutates and atomically publishes one snapshot, an error from mutate publishing nothing
func Mutate(h string, mutate func(subs *pyjson.Object) error) error {
	release, err := Lock(h)
	if err != nil {
		return err
	}
	defer release()
	subs := LoadUnlocked(h)
	if err := mutate(subs); err != nil {
		return err
	}
	return SaveUnlocked(h, subs)
}

// Compares one route to a snapshot value with Python equality over decoded JSON
func sameRoute(a, b any) bool {
	return pyrepr.Equal(pyjson.Plain(a), pyjson.Plain(b))
}

// Swaps one route only when it still equals expected, keeping an explicit enabled false, and returns it as published
func Replace(h, name string, route *pyjson.Object, expected any) (*pyjson.Object, error) {
	var published *pyjson.Object
	err := Mutate(h, func(subs *pyjson.Object) error {
		current, present := subs.Get(name)
		expectedMissing := expected == Missing
		if present == expectedMissing || (!expectedMissing && !sameRoute(current, expected)) {
			return &SubscriptionMutationConflict{Msg: fmt.Sprintf("Webhook subscription '%s' changed concurrently; retry the operation.", name)}
		}
		published = route
		if cur, ok := current.(*pyjson.Object); ok {
			if enabled, ok := cur.Get("enabled"); ok && enabled == false {
				published = copyObject(route)
				published.Set("enabled", false)
			}
		}
		subs.Set(name, published)
		return nil
	})
	if err != nil {
		return nil, err
	}
	return published, nil
}

// Shallow copy of an ordered object like {**route}
func copyObject(o *pyjson.Object) *pyjson.Object {
	out := pyjson.NewObject()
	for _, k := range o.Keys {
		out.Set(k, o.Vals[k])
	}
	return out
}

// Writes a plain map mutation back into the ordered snapshot, surviving keys in file order and new keys sorted after
func ApplyPlain(dst *pyjson.Object, plain map[string]any) {
	for _, k := range slices.Clone(dst.Keys) {
		if _, ok := plain[k]; !ok {
			dst.Delete(k)
		}
	}
	var added []string
	for k := range plain {
		if _, ok := dst.Get(k); !ok {
			added = append(added, k)
		}
	}
	slices.Sort(added)
	for _, k := range slices.Clone(dst.Keys) {
		old, _ := dst.Get(k)
		dst.Set(k, orderLike(old, plain[k]))
	}
	for _, k := range added {
		dst.Set(k, orderLike(nil, plain[k]))
	}
}

// Ordered form of a plain value, maps taking the template object's key order where it has one
func orderLike(template, v any) any {
	switch t := v.(type) {
	case map[string]any:
		out := pyjson.NewObject()
		if tmpl, ok := template.(*pyjson.Object); ok {
			for _, k := range tmpl.Keys {
				if val, ok := t[k]; ok {
					out.Set(k, orderLike(tmpl.Vals[k], val))
				}
			}
		}
		keys := make([]string, 0, len(t))
		for k := range t {
			if _, ok := out.Get(k); !ok {
				keys = append(keys, k)
			}
		}
		slices.Sort(keys)
		for _, k := range keys {
			out.Set(k, orderLike(nil, t[k]))
		}
		return out
	case []any:
		tl, _ := template.([]any)
		out := make([]any, len(t))
		for i, item := range t {
			var tmpl any
			if i < len(tl) {
				tmpl = tl[i]
			}
			out[i] = orderLike(tmpl, item)
		}
		return out
	}
	return v
}

// The platforms.webhook config mapping, empty when unconfigured or unreadable
func WebhookConfig(ctx context.Context, h string) map[string]any {
	tree, err := config.Load(h, config.NewSecrets(h).Lookup())
	if err != nil {
		return map[string]any{}
	}
	platforms, _ := map[string]any(tree)["platforms"].(map[string]any)
	if wh, ok := platforms["webhook"].(map[string]any); ok {
		return wh
	}
	return map[string]any{}
}

// Whether the webhook platform is enabled in the home config
func WebhookEnabled(ctx context.Context, h string) bool {
	return pyTruthy(WebhookConfig(ctx, h)["enabled"])
}

// Local base URL of the webhook listener, wildcard hosts shown as localhost and IPv6 bracketed
func BaseURL(ctx context.Context, h string) string {
	extra, _ := WebhookConfig(ctx, h)["extra"].(map[string]any)
	host, _ := extra["host"].(string)
	displayHost := host
	if extra["host"] != nil && host == "" {
		displayHost = pyrepr.Str(extra["host"])
	}
	if !pyTruthy(extra["host"]) || host == "0.0.0.0" || host == "::" {
		displayHost = "localhost"
	}
	if strings.Contains(displayHost, ":") && !strings.HasPrefix(displayHost, "[") {
		displayHost = "[" + displayHost + "]"
	}
	port, ok := extra["port"]
	if !ok {
		port = 8644
	}
	return fmt.Sprintf("http://%s:%s", displayHost, pyrepr.Str(port))
}

// Delivery URL of one route, prefixed with /p/<profile> for a non default profile
func RouteURL(ctx context.Context, h, name string, route *pyjson.Object) string {
	profile := any("default")
	if v, ok := route.Get("profile"); ok {
		profile = v
	}
	prefix := ""
	if p, ok := profile.(string); !ok || p != "default" {
		prefix = "/p/" + pyrepr.Str(profile)
	}
	return BaseURL(ctx, h) + prefix + "/webhooks/" + name
}

// Python truthiness of a decoded config value
func pyTruthy(v any) bool {
	switch t := v.(type) {
	case nil:
		return false
	case bool:
		return t
	case string:
		return t != ""
	case int:
		return t != 0
	case int64:
		return t != 0
	case float64:
		return t != 0
	case map[string]any:
		return len(t) > 0
	case []any:
		return len(t) > 0
	}
	return true
}
