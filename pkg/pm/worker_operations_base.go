package pm

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"github.com/nickheyer/icebath/pkg/errchain"
	anyutil "github.com/nickheyer/icebath/pkg/utils/any"
)

// Prerequisite policies of worker operations, policy, state and never
const (
	bootstrapPolicy = "policy"
	bootstrapState  = "state"
	bootstrapNever  = "never"
)

// Callbacks and cancellation the worker hands an operation, each also a keyword the operation must accept
type workerHooks struct {
	pause            *PauseEvent
	progress         InstallProgress
	downloadProgress ProgressFunc
}

// One routable worker operation, the packages it needs restored, its prerequisite policy and its dispatch
type workerOperation struct {
	packages  []string
	bootstrap string
	run       func(ctx context.Context, args map[string]any, hooks workerHooks) (any, error)
}

// Keyword arguments of one dispatched call with Python's binding errors
type kwargs struct {
	op         string
	args       map[string]any
	positional []string
	keyword    []string
}

// Binding of an operation's keyword arguments, hook keywords included as the worker adds them
func bindKwargs(op string, args map[string]any, hooks workerHooks, positional, keyword []string) *kwargs {
	bound := make(map[string]any, len(args)+3)
	for k, v := range args {
		bound[k] = v
	}
	if hooks.pause != nil {
		bound["pause_event"] = hooks.pause
	}
	if hooks.progress != nil {
		bound["progress"] = hooks.progress
	}
	if hooks.downloadProgress != nil {
		bound["download_progress"] = hooks.downloadProgress
	}
	return &kwargs{op: op, args: bound, positional: positional, keyword: keyword}
}

// Python wording of a missing argument list, quoted and joined with and
func missingList(names []string) string {
	quoted := make([]string, len(names))
	for i, n := range names {
		quoted[i] = "'" + n + "'"
	}
	switch len(quoted) {
	case 1:
		return quoted[0]
	case 2:
		return quoted[0] + " and " + quoted[1]
	}
	return strings.Join(quoted[:len(quoted)-1], ", ") + ", and " + quoted[len(quoted)-1]
}

// Python TypeError for required arguments the call left out, positional ones reported first
func (k *kwargs) checkRequired() error {
	for _, group := range []struct {
		names []string
		kind  string
	}{{k.positional, "positional"}, {k.keyword, "keyword-only"}} {
		var missing []string
		for _, name := range group.names {
			if _, ok := k.args[name]; !ok {
				missing = append(missing, name)
			}
		}
		if len(missing) == 0 {
			continue
		}
		plural := ""
		if len(missing) > 1 {
			plural = "s"
		}
		return &PyError{Class: "TypeError", Message: fmt.Sprintf("%s() missing %d required %s argument%s: %s",
			k.op, len(missing), group.kind, plural, missingList(missing))}
	}
	return nil
}

// Python TypeError for the first keyword the operation does not accept
func (k *kwargs) checkKnown(known map[string]bool) error {
	names := make([]string, 0, len(k.args))
	for name := range k.args {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		if !known[name] {
			return &PyError{Class: "TypeError", Message: fmt.Sprintf("%s does not take the option %s", k.op, name)}
		}
	}
	return nil
}

// Python TypeError for a value of the wrong JSON kind
func kwargTypeError(op, name, want string, value any) error {
	return &PyError{Class: "TypeError", Message: fmt.Sprintf("the %s option %s must be %s, got %s", op, name, want, errchain.Kind(value))}
}

// Value of an optional keyword and whether the call passed it
func (k *kwargs) take(name string) (any, bool) {
	v, ok := k.args[name]
	return v, ok
}

// String keyword, a TypeError for another kind
func (k *kwargs) str(name string) (string, error) {
	v, _ := k.take(name)
	s, ok := v.(string)
	if !ok {
		return "", kwargTypeError(k.op, name, "text", v)
	}
	return s, nil
}

// Boolean keyword with its Python default
func (k *kwargs) flag(name string, fallback bool) bool {
	v, ok := k.take(name)
	if !ok {
		return fallback
	}
	return anyutil.IsTrue(v)
}

// Keyword set of an engine function
func keywordSet(names ...string) map[string]bool {
	out := make(map[string]bool, len(names))
	for _, n := range names {
		out[n] = true
	}
	return out
}

// Worker dispatch of the tool install engine
func runEnsure(ctx context.Context, args map[string]any, hooks workerHooks) (any, error) {
	k := bindKwargs("ensure", args, hooks, []string{"name"}, nil)
	if err := k.checkRequired(); err != nil {
		return nil, err
	}
	if err := k.checkKnown(keywordSet("name", "base_env", "explicit", "verify", "progress", "pause_event", "download_progress")); err != nil {
		return nil, err
	}
	name, err := k.str("name")
	if err != nil {
		return nil, err
	}
	opts := EnsureOptions{Explicit: k.flag("explicit", false), SkipVerify: !k.flag("verify", true),
		Progress: hooks.progress, PauseEvent: hooks.pause, DownloadProgress: hooks.downloadProgress}
	if base, ok := k.take("base_env"); ok && base != nil {
		object, isMap := base.(map[string]any)
		if !isMap {
			return nil, kwargTypeError("ensure", "base_env", "a mapping", base)
		}
		opts.BaseEnv = map[string]string{}
		for key, value := range object {
			opts.BaseEnv[key] = anyutil.String(value)
		}
	}
	if _, err := ensure(ctx, name, opts); err != nil {
		return nil, err
	}
	return nil, nil
}

// Operations the private worker routes by their Python name, the binary store's ensure here and the package manager's added at init
var workerOperations = map[string]*workerOperation{
	"ensure": {bootstrap: bootstrapState, run: runEnsure},
}

// Routed operation of a name, the KeyError Python raises for one the worker does not route
func lookupOperation(name string) (*workerOperation, error) {
	spec, ok := workerOperations[name]
	if !ok {
		return nil, &PyError{Class: "KeyError", Message: "the worker has no operation " + name}
	}
	return spec, nil
}
