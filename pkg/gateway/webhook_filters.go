package gateway

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"math/big"
	"os"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/internal/pyint"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pypath"
	"github.com/nickheyer/icebath/pkg/internal/pyre"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
	"github.com/nickheyer/icebath/pkg/internal/pysubprocess"
	"github.com/nickheyer/icebath/pkg/pm"
	"github.com/nickheyer/icebath/pkg/tools"
)

// Seconds a route script may run before the webhook is dropped
const WebhookDefaultScriptTimeoutSeconds = 30

// Marker of a filter field that did not resolve
type webhookMissingValue struct{}

var webhookMissing any = &webhookMissingValue{}

// Logger of the route filter and script module
func webhookFiltersLog() *slog.Logger {
	return slog.Default().With("logger", "gateway.platforms.webhook_filters")
}

// Text a filter operator compares against, JSON for dicts and lists and empty when missing
func webhookStringifyFilterValue(value any) (string, error) {
	if value == webhookMissing {
		return "", nil
	}
	switch value.(type) {
	case *pyjson.Object, map[string]any, []any:
		return pyjson.Dumps(value, pyjson.SortKeys(true))
	}
	return pyrepr.Str(value), nil
}

// Profile home path of a user path with the dot hermes prefix mapped onto the active home
func webhookResolveProfilePath(ctx context.Context, pathValue any) (string, bool, error) {
	text, ok := pathValue.(string)
	if !ok {
		return "", false, nil
	}
	raw := pypath.Expandvars(pystr.Strip(text))
	if raw == "" {
		return "", false, nil
	}
	h := home.Dir(ctx)
	if raw == "~/.icebath" || strings.HasPrefix(raw, "~/.icebath/") {
		rest := ""
		if len(raw) > len("~/.icebath/") {
			rest = raw[len("~/.icebath/"):]
		}
		return pypath.PathJoin(h, rest), true, nil
	}
	expanded, err := pypath.PathExpanduser(raw)
	if err != nil {
		return "", false, err
	}
	if pypath.IsAbs(expanded) {
		return expanded, true, nil
	}
	return pypath.PathJoin(h, expanded), true, nil
}

// Script path under the home scripts directory, or the reason the route script is refused
func webhookResolveScriptPath(ctx context.Context, scriptValue any) (string, string, error) {
	text, ok := scriptValue.(string)
	if !ok || pystr.Strip(text) == "" {
		return "", "script path is empty", nil
	}
	scriptsRoot, err := pypath.Resolve(pypath.PathJoin(home.Dir(ctx), "scripts"))
	if err != nil {
		return "", "", err
	}
	rawText := pypath.Expandvars(pystr.Strip(text))
	candidate := scriptsRoot
	if rawText == "~/.icebath" || strings.HasPrefix(rawText, "~/.icebath/") {
		mapped, found, err := webhookResolveProfilePath(ctx, rawText)
		if err != nil {
			return "", "", err
		}
		if found {
			if candidate, err = pypath.Resolve(mapped); err != nil {
				return "", "", err
			}
		}
	} else {
		raw, err := pypath.PathExpanduser(rawText)
		if err != nil {
			return "", "", err
		}
		if !pypath.IsAbs(raw) {
			raw = pypath.PathJoin(scriptsRoot, raw)
		}
		if candidate, err = pypath.Resolve(raw); err != nil {
			return "", "", err
		}
	}
	if _, inside := pypath.PathRelativeTo(candidate, scriptsRoot); !inside {
		return "", "script path resolves outside " + scriptsRoot, nil
	}
	info, err := os.Stat(candidate)
	if err != nil {
		return "", "script not found: " + candidate, nil
	}
	if !info.Mode().IsRegular() {
		return "", "script path is not a file: " + candidate, nil
	}
	return candidate, "", nil
}

// Values of an in_file list, a JSON list, the keys of a JSON object or each non blank line
func webhookLoadFilterFileValues(ctx context.Context, pathValue any) ([]any, error) {
	path, found, err := webhookResolveProfilePath(ctx, pathValue)
	if err != nil || !found {
		return nil, err
	}
	data, err := os.ReadFile(path)
	if err != nil {
		webhookFiltersLog().Warn(fmt.Sprintf("[webhook] filter in_file read failed for %s: %s", path, pyrepr.ExceptionMessage(err)))
		return nil, nil
	}
	data = []byte(strings.TrimPrefix(string(data), "\ufeff"))
	if !utf8.Valid(data) {
		return nil, &PyError{Kind: "UnicodeDecodeError", Msg: "'utf-8' codec can't decode bytes: invalid continuation byte"}
	}
	raw := string(data)
	decoded, err := pyjson.Loads(raw)
	if err != nil {
		var decodeErr *pyjson.DecodeError
		if !errors.As(err, &decodeErr) {
			return nil, err
		}
		var lines []any
		for _, line := range pystr.Splitlines(raw, false) {
			if stripped := pystr.Strip(line); stripped != "" {
				lines = append(lines, stripped)
			}
		}
		return lines, nil
	}
	switch v := decoded.(type) {
	case []any:
		return v, nil
	case *pyjson.Object:
		keys := make([]any, 0, len(v.Keys))
		for _, k := range v.Keys {
			keys = append(keys, k)
		}
		return keys, nil
	}
	return []any{decoded}, nil
}

// Python membership of item in a list, the keys of a dict or a substring of a str
func webhookPyIn(item, container any) (bool, error) {
	switch c := container.(type) {
	case []any:
		for _, element := range c {
			if pyrepr.Equal(element, item) {
				return true, nil
			}
		}
		return false, nil
	case *pyjson.Object, map[string]any:
		obj, _ := pyjson.AsObject(c)
		switch key := item.(type) {
		case string:
			_, found := obj.Get(key)
			return found, nil
		case *pyjson.Object, map[string]any, []any:
			return false, &PyError{Kind: "TypeError", Msg: fmt.Sprintf("unhashable type: '%s'", pyrepr.TypeName(item))}
		}
		return false, nil
	case string:
		needle, ok := item.(string)
		if !ok {
			return false, &PyError{Kind: "TypeError", Msg: fmt.Sprintf("'in <string>' requires string as left operand, not %s", pyrepr.TypeName(item))}
		}
		return strings.Contains(c, needle), nil
	}
	if item == webhookMissing {
		return false, nil
	}
	return false, &PyError{Kind: "TypeError", Msg: fmt.Sprintf("argument of type '%s' is not iterable", pyrepr.TypeName(container))}
}

// Whether the operand occurs in a container value or its text occurs in a scalar value
func webhookOpContains(value, needle any) (bool, error) {
	if value == webhookMissing {
		return false, nil
	}
	switch value.(type) {
	case []any, *pyjson.Object, map[string]any:
		return webhookPyIn(needle, value)
	}
	text, err := webhookStringifyFilterValue(value)
	if err != nil {
		return false, err
	}
	return strings.Contains(text, pyrepr.Str(needle)), nil
}

// Whether the pattern searches the value text, an invalid pattern logging and matching nothing
func webhookOpRegex(value, pattern any) (bool, error) {
	if value == webhookMissing {
		return false, nil
	}
	re, err := pyre.Compile(pyrepr.Str(pattern))
	if err != nil {
		webhookFiltersLog().Warn("[webhook] Invalid webhook filter regex: " + err.Error())
		return false, nil
	}
	text, err := webhookStringifyFilterValue(value)
	if err != nil {
		return false, err
	}
	match, err := re.FindStringMatch(text)
	if err != nil {
		return false, err
	}
	return match != nil, nil
}

// Field operators in precedence order, the first key present in a spec deciding the match
var webhookFieldOperators = []struct {
	key   string
	match func(ctx context.Context, value, arg any) (bool, error)
}{
	{"exists", func(_ context.Context, value, arg any) (bool, error) {
		return (value != webhookMissing) == pyrepr.Truthy(arg), nil
	}},
	{"equals", func(_ context.Context, value, arg any) (bool, error) {
		return value != webhookMissing && pyrepr.Equal(value, arg), nil
	}},
	{"not_equals", func(_ context.Context, value, arg any) (bool, error) {
		return value == webhookMissing || !pyrepr.Equal(value, arg), nil
	}},
	{"contains", func(_ context.Context, value, arg any) (bool, error) { return webhookOpContains(value, arg) }},
	{"in", func(_ context.Context, value, arg any) (bool, error) {
		list, ok := arg.([]any)
		if !ok {
			return false, nil
		}
		return webhookPyIn(value, list)
	}},
	{"in_file", func(ctx context.Context, value, arg any) (bool, error) {
		values, err := webhookLoadFilterFileValues(ctx, arg)
		if err != nil {
			return false, err
		}
		return webhookPyIn(value, values)
	}},
	{"regex", func(_ context.Context, value, arg any) (bool, error) { return webhookOpRegex(value, arg) }},
}

// WebhookRouteProcessor evaluates declarative route filters and optional script transforms
type WebhookRouteProcessor struct {
	ScriptTimeoutSeconds int
}

// Processor with a script timeout of at least one second
func NewWebhookRouteProcessor(scriptTimeoutSeconds int) *WebhookRouteProcessor {
	return &WebhookRouteProcessor{ScriptTimeoutSeconds: max(1, scriptTimeoutSeconds)}
}

// Mapping value of a payload, the AttributeError a get call on any other value raises
func webhookPayloadGet(payload any, key string, def any) (any, error) {
	obj, ok := pyjson.AsObject(payload)
	if !ok {
		return nil, &PyError{Kind: "AttributeError", Msg: fmt.Sprintf("'%s' object has no attribute 'get'", pyrepr.TypeName(payload))}
	}
	return obj.GetOr(key, def), nil
}

// Dotted field resolved against the payload, event and headers context, missing when absent
func (p *WebhookRouteProcessor) ResolveFilterField(field, payload, eventType any, headers *pyjson.Object) (any, error) {
	text, ok := field.(string)
	if !ok || pystr.Strip(text) == "" {
		return webhookMissing, nil
	}
	var parts []string
	for _, part := range strings.Split(pystr.Strip(text), ".") {
		if part != "" {
			parts = append(parts, part)
		}
	}
	if len(parts) == 0 {
		return webhookMissing, nil
	}
	inner, err := webhookPayloadGet(payload, "payload", payload)
	if err != nil {
		return nil, err
	}
	if headers == nil {
		headers = pyjson.NewObject()
	}
	context := pyjson.ObjectOf("payload", inner, "event", eventType, "event_type", eventType, "headers", headers.Copy())
	value := payload
	if head, found := context.Get(parts[0]); found {
		value, parts = head, parts[1:]
	}
	for _, part := range parts {
		switch v := value.(type) {
		case *pyjson.Object, map[string]any:
			obj, _ := pyjson.AsObject(v)
			value = obj.GetOr(part, webhookMissing)
		case []any:
			if !webhookIsDigits(part) {
				return webhookMissing, nil
			}
			idx, err := pyint.Of(part)
			if err != nil {
				return nil, err
			}
			if idx.Sign() >= 0 && idx.Cmp(big.NewInt(int64(len(v)))) < 0 {
				value = v[idx.Int64()]
			} else {
				value = webhookMissing
			}
		default:
			return webhookMissing, nil
		}
		if value == webhookMissing {
			return webhookMissing, nil
		}
	}
	return value, nil
}

// Whether every rune of a non empty text is a Unicode digit, the str isdigit test
func webhookIsDigits(text string) bool {
	if text == "" {
		return false
	}
	for _, r := range text {
		if !pystr.IsDigit(r) {
			return false
		}
	}
	return true
}

// Evaluates one filter spec, the all any and not combinators first and then the field operators
func (p *WebhookRouteProcessor) FilterMatches(ctx context.Context, spec, payload, eventType any, headers *pyjson.Object) (bool, error) {
	obj, ok := pyjson.AsObject(spec)
	if !ok {
		webhookFiltersLog().Warn("[webhook] Ignoring invalid filter spec: " + pyrepr.Repr(spec))
		return false, nil
	}
	for _, key := range []string{"all", "any"} {
		items, found := obj.Get(key)
		if !found {
			continue
		}
		list, isList := items.([]any)
		if !isList {
			return false, nil
		}
		for _, item := range list {
			matched, err := p.FilterMatches(ctx, item, payload, eventType, headers)
			if err != nil {
				return false, err
			}
			if key == "all" && !matched {
				return false, nil
			}
			if key == "any" && matched {
				return true, nil
			}
		}
		return key == "all", nil
	}
	if inner, found := obj.Get("not"); found {
		matched, err := p.FilterMatches(ctx, inner, payload, eventType, headers)
		return !matched, err
	}
	value, err := p.ResolveFilterField(obj.GetOr("field", nil), payload, eventType, headers)
	if err != nil {
		return false, err
	}
	if _, hasExists := obj.Get("exists"); !hasExists && obj.GetOr("missing", nil) == true {
		return value == webhookMissing, nil
	}
	for _, op := range webhookFieldOperators {
		if arg, found := obj.Get(op.key); found {
			return op.match(ctx, value, arg)
		}
	}
	webhookFiltersLog().Warn("[webhook] Filter spec has no supported operator: " + pyrepr.Repr(spec))
	return false, nil
}

// Whether the route filters admit the event, a list requiring every spec and an object a single spec
func (p *WebhookRouteProcessor) RouteFiltersMatch(ctx context.Context, routeConfig *pyjson.Object, payload, eventType any, headers *pyjson.Object) (bool, error) {
	filters := routeConfig.GetOr("filters", nil)
	if !pyrepr.Truthy(filters) {
		return true, nil
	}
	switch v := filters.(type) {
	case *pyjson.Object, map[string]any:
		return p.FilterMatches(ctx, v, payload, eventType, headers)
	case []any:
		for _, spec := range v {
			matched, err := p.FilterMatches(ctx, spec, payload, eventType, headers)
			if err != nil || !matched {
				return false, err
			}
		}
		return true, nil
	}
	webhookFiltersLog().Warn("[webhook] filters must be a list or object")
	return false, nil
}

// Interpreter the application runs under, the selected dependency environment python
func webhookSysExecutable(ctx context.Context) (string, error) {
	root := pm.RepoRoot()
	venv, err := pm.SelectedVenv(ctx, root)
	if err != nil {
		return "", err
	}
	command, err := pm.VenvCommand(ctx, root, venv, nil)
	if err != nil {
		return "", err
	}
	return command[0], nil
}

// Runs a route script on the payload, false drops the webhook and a JSON object replaces the payload
func (p *WebhookRouteProcessor) RunRouteScript(ctx context.Context, scriptValue any, payload any) (bool, *pyjson.Object, error) {
	log := webhookFiltersLog()
	path, reason, err := webhookResolveScriptPath(ctx, scriptValue)
	if err != nil {
		return false, nil, err
	}
	if reason != "" || path == "" {
		log.Warn("[webhook] script ignored webhook: " + reason)
		return false, nil, nil
	}
	suffix := strings.ToLower(pypath.Suffix(path))
	var interpreter string
	if suffix == ".sh" || suffix == ".bash" {
		bash, err := tools.FindBash()
		if err != nil {
			log.Warn("[webhook] script ignored webhook: " + err.Error())
			return false, nil, nil
		}
		interpreter = bash
	} else if interpreter, err = webhookSysExecutable(ctx); err != nil {
		log.Warn("[webhook] script execution failed: " + pyrepr.ExceptionMessage(err))
		return false, nil, nil
	}
	input, err := pyjson.Dumps(payload)
	if err != nil {
		log.Warn("[webhook] script execution failed: " + pyrepr.ExceptionMessage(err))
		return false, nil, nil
	}
	child, err := tools.BuildSubprocessEnvOrdered(ctx, nil, tools.SubprocessEnvOptions{InheritProfileHome: true, ScrubSecrets: true})
	if err != nil {
		log.Warn("[webhook] script execution failed: " + pyrepr.ExceptionMessage(err))
		return false, nil, nil
	}
	result, err := pysubprocess.RunWith(ctx, []string{interpreter, path}, pysubprocess.Options{Dir: pypath.PathParent(path),
		Env: child.Environ(), Input: []byte(input), Timeout: time.Duration(p.ScriptTimeoutSeconds) * time.Second,
		Attr: webhookScriptProcAttr()})
	var timeout *pysubprocess.TimeoutExpired
	if errors.As(err, &timeout) {
		log.Warn("[webhook] script timed out: " + path)
		return false, nil, nil
	}
	if err != nil {
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			return false, nil, err
		}
		log.Warn("[webhook] script execution failed: " + webhookExecErrorText(err))
		return false, nil, nil
	}
	stdout := pystr.Strip(strings.ToValidUTF8(string(result.Stdout), "�"))
	stderr := pystr.Strip(strings.ToValidUTF8(string(result.Stderr), "�"))
	stdout, stderr = tools.RedactSensitiveText(stdout), tools.RedactSensitiveText(stderr)
	if result.Returncode != 0 {
		level := slog.LevelInfo
		if stdout == "" && stderr == "" {
			level = slog.LevelWarn
		}
		log.Log(ctx, level, fmt.Sprintf("[webhook] script ignored webhook path=%s code=%d stderr=%s",
			pypath.PathName(path), result.Returncode, pySliceRunes(stderr, 200)))
	}
	if result.Returncode != 0 || stdout == "" || stdout == "[SILENT]" {
		return false, nil, nil
	}
	var transformed any
	decoded, err := pyjson.Loads(stdout)
	var decodeErr *pyjson.DecodeError
	switch {
	case err == nil:
		transformed = decoded
	case errors.As(err, &decodeErr):
		merged := pyjson.NewObject()
		if obj, ok := pyjson.AsObject(payload); ok {
			merged = obj.Copy()
		} else {
			return false, nil, &PyError{Kind: "TypeError", Msg: fmt.Sprintf("'%s' object is not a mapping", pyrepr.TypeName(payload))}
		}
		merged.Set("script_output", stdout)
		transformed = merged
	default:
		return false, nil, err
	}
	obj, ok := transformed.(*pyjson.Object)
	if !ok {
		log.Warn("[webhook] script stdout must be a JSON object or text")
		return false, nil, nil
	}
	if obj.GetOr("[SILENT]", nil) == true || obj.GetOr("__icebath_ignore__", nil) == true {
		return false, nil, nil
	}
	return true, obj, nil
}

// Message of a spawn failure the way Python words an OSError from Popen
func webhookExecErrorText(err error) string {
	var pathErr *fs.PathError
	if errors.As(err, &pathErr) {
		return fmt.Sprintf("%s: '%s'", pyrepr.ExceptionMessage(pathErr.Err), pathErr.Path)
	}
	return pyrepr.ExceptionMessage(err)
}
