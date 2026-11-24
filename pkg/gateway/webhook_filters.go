package gateway

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/nickheyer/icebath/pkg/errchain"
	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/tools"
	anyutil "github.com/nickheyer/icebath/pkg/utils/any"
	executil "github.com/nickheyer/icebath/pkg/utils/exec"
	jsonutil "github.com/nickheyer/icebath/pkg/utils/json"
	pathutil "github.com/nickheyer/icebath/pkg/utils/path"
	regexutil "github.com/nickheyer/icebath/pkg/utils/regex"
	textutil "github.com/nickheyer/icebath/pkg/utils/text"
)

// Seconds a route script may run before the webhook is dropped
const WebhookDefaultScriptTimeoutSeconds = 30

// Marker of a filter field that did not resolve
type webhookMissingValue struct{}

var webhookMissing any = &webhookMissingValue{}

// Text a filter operator compares against, JSON for dicts and lists and empty when missing
func webhookStringifyFilterValue(value any) (string, error) {
	if value == webhookMissing {
		return "", nil
	}
	switch value.(type) {
	case *jsonutil.Object, map[string]any, []any:
		return jsonutil.Encode(value, jsonutil.SortKeys())
	}
	return anyutil.String(value), nil
}

// Profile home path of a user path with the ~/.icebath prefix mapped onto the active home
func webhookResolveProfilePath(ctx context.Context, pathValue any) (string, bool, error) {
	text, ok := pathValue.(string)
	if !ok {
		return "", false, nil
	}
	raw := pathutil.ExpandEnv(strings.TrimSpace(text))
	if raw == "" {
		return "", false, nil
	}
	h := home.Dir(ctx)
	if raw == "~/.icebath" || strings.HasPrefix(raw, "~/.icebath/") {
		rest := ""
		if len(raw) > len("~/.icebath/") {
			rest = raw[len("~/.icebath/"):]
		}
		return pathutil.Join(h, rest), true, nil
	}
	expanded, err := pathutil.ExpandUserPath(raw)
	if err != nil {
		return "", false, err
	}
	if filepath.IsAbs(expanded) {
		return expanded, true, nil
	}
	return pathutil.Join(h, expanded), true, nil
}

// Script path under the home scripts directory, or the reason the route script is refused
func webhookResolveScriptPath(ctx context.Context, scriptValue any) (string, string, error) {
	text, ok := scriptValue.(string)
	if !ok || strings.TrimSpace(text) == "" {
		return "", "script path is empty", nil
	}
	scriptsRoot, err := pathutil.Resolve(pathutil.Join(home.Dir(ctx), "scripts"))
	if err != nil {
		return "", "", err
	}
	rawText := pathutil.ExpandEnv(strings.TrimSpace(text))
	candidate := scriptsRoot
	if rawText == "~/.icebath" || strings.HasPrefix(rawText, "~/.icebath/") {
		mapped, found, err := webhookResolveProfilePath(ctx, rawText)
		if err != nil {
			return "", "", err
		}
		if found {
			if candidate, err = pathutil.Resolve(mapped); err != nil {
				return "", "", err
			}
		}
	} else {
		raw, err := pathutil.ExpandUserPath(rawText)
		if err != nil {
			return "", "", err
		}
		if !filepath.IsAbs(raw) {
			raw = pathutil.Join(scriptsRoot, raw)
		}
		if candidate, err = pathutil.Resolve(raw); err != nil {
			return "", "", err
		}
	}
	if _, inside := pathutil.RelativeTo(candidate, scriptsRoot); !inside {
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
		pkgLog().Warn(fmt.Sprintf("[webhook] filter in_file read failed for %s: %s", path, err.Error()))
		return nil, nil
	}
	data = []byte(strings.TrimPrefix(string(data), "\ufeff"))
	if !utf8.Valid(data) {
		return nil, &PyError{Kind: "UnicodeDecodeError", Msg: "the webhook filter file is not valid UTF-8"}
	}
	raw := string(data)
	decoded, err := jsonutil.Decode(raw)
	if err != nil {
		var decodeErr *jsonutil.SyntaxError
		if !errors.As(err, &decodeErr) {
			return nil, err
		}
		var lines []any
		for _, line := range textutil.Lines(raw) {
			if stripped := strings.TrimSpace(line); stripped != "" {
				lines = append(lines, stripped)
			}
		}
		return lines, nil
	}
	switch v := decoded.(type) {
	case []any:
		return v, nil
	case *jsonutil.Object:
		keys := make([]any, 0, v.Len())
		for _, k := range v.Keys() {
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
			if anyutil.Equal(element, item) {
				return true, nil
			}
		}
		return false, nil
	case *jsonutil.Object, map[string]any:
		obj, _ := jsonutil.AsObject(c)
		switch key := item.(type) {
		case string:
			_, found := obj.Get(key)
			return found, nil
		case *jsonutil.Object, map[string]any, []any:
			return false, &PyError{Kind: "TypeError", Msg: fmt.Sprintf("a webhook filter value is %s, expected text or a number", errchain.Kind(item))}
		}
		return false, nil
	case string:
		needle, ok := item.(string)
		if !ok {
			return false, &PyError{Kind: "TypeError", Msg: fmt.Sprintf("a webhook filter value to find in text is %s, expected text", errchain.Kind(item))}
		}
		return strings.Contains(c, needle), nil
	}
	if item == webhookMissing {
		return false, nil
	}
	return false, &PyError{Kind: "TypeError", Msg: fmt.Sprintf("a webhook filter list is %s, expected a list", errchain.Kind(container))}
}

// Whether the operand occurs in a container value or its text occurs in a scalar value
func webhookOpContains(value, needle any) (bool, error) {
	if value == webhookMissing {
		return false, nil
	}
	switch value.(type) {
	case []any, *jsonutil.Object, map[string]any:
		return webhookPyIn(needle, value)
	}
	text, err := webhookStringifyFilterValue(value)
	if err != nil {
		return false, err
	}
	return strings.Contains(text, anyutil.String(needle)), nil
}

// Whether the pattern searches the value text, an invalid pattern logging and matching nothing
func webhookOpRegex(value, pattern any) (bool, error) {
	if value == webhookMissing {
		return false, nil
	}
	re, err := regexutil.Compile(anyutil.String(pattern))
	if err != nil {
		pkgLog().Warn("[webhook] Invalid webhook filter regex: " + err.Error())
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
		return (value != webhookMissing) == anyutil.IsTrue(arg), nil
	}},
	{"equals", func(_ context.Context, value, arg any) (bool, error) {
		return value != webhookMissing && anyutil.Equal(value, arg), nil
	}},
	{"not_equals", func(_ context.Context, value, arg any) (bool, error) {
		return value == webhookMissing || !anyutil.Equal(value, arg), nil
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
	obj, ok := jsonutil.AsObject(payload)
	if !ok {
		return nil, &PyError{Kind: "AttributeError", Msg: fmt.Sprintf("the webhook payload is %s, expected a mapping", errchain.Kind(payload))}
	}
	return obj.ValueOr(key, def), nil
}

// Dotted field resolved against the payload, event and headers context, missing when absent
func (p *WebhookRouteProcessor) ResolveFilterField(field, payload, eventType any, headers *jsonutil.Object) (any, error) {
	text, ok := field.(string)
	if !ok || strings.TrimSpace(text) == "" {
		return webhookMissing, nil
	}
	var parts []string
	for _, part := range strings.Split(strings.TrimSpace(text), ".") {
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
		headers = jsonutil.NewObject()
	}
	context := jsonutil.ObjectOf("payload", inner, "event", eventType, "event_type", eventType, "headers", headers.Clone())
	value := payload
	if head, found := context.Get(parts[0]); found {
		value, parts = head, parts[1:]
	}
	for _, part := range parts {
		switch v := value.(type) {
		case *jsonutil.Object, map[string]any:
			obj, _ := jsonutil.AsObject(v)
			value = obj.ValueOr(part, webhookMissing)
		case []any:
			if !webhookIsDigits(part) {
				return webhookMissing, nil
			}
			idx, err := strconv.Atoi(part)
			if err != nil || idx >= len(v) {
				return webhookMissing, nil
			}
			value = v[idx]
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
		if !unicode.IsDigit(r) {
			return false
		}
	}
	return true
}

// Evaluates one filter spec, the all any and not combinators first and then the field operators
func (p *WebhookRouteProcessor) FilterMatches(ctx context.Context, spec, payload, eventType any, headers *jsonutil.Object) (bool, error) {
	obj, ok := jsonutil.AsObject(spec)
	if !ok {
		pkgLog().Warn("[webhook] Ignoring invalid filter spec: " + anyutil.Quote(spec))
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
	value, err := p.ResolveFilterField(obj.ValueOr("field", nil), payload, eventType, headers)
	if err != nil {
		return false, err
	}
	if _, hasExists := obj.Get("exists"); !hasExists && obj.ValueOr("missing", nil) == true {
		return value == webhookMissing, nil
	}
	for _, op := range webhookFieldOperators {
		if arg, found := obj.Get(op.key); found {
			return op.match(ctx, value, arg)
		}
	}
	pkgLog().Warn("[webhook] Filter spec has no supported operator: " + anyutil.Quote(spec))
	return false, nil
}

// Whether the route filters admit the event, a list requiring every spec and an object a single spec
func (p *WebhookRouteProcessor) RouteFiltersMatch(ctx context.Context, routeConfig *jsonutil.Object, payload, eventType any, headers *jsonutil.Object) (bool, error) {
	filters := routeConfig.ValueOr("filters", nil)
	if !anyutil.IsTrue(filters) {
		return true, nil
	}
	switch v := filters.(type) {
	case *jsonutil.Object, map[string]any:
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
	pkgLog().Warn("[webhook] filters must be a list or object")
	return false, nil
}

// First python3 or python on PATH, the interpreter Python route scripts run on
func webhookSysExecutable() (string, error) {
	for _, name := range []string{"python3", "python"} {
		path, err := exec.LookPath(name)
		if err != nil || (runtime.GOOS == "windows" && strings.Contains(strings.ToLower(path), `\microsoft\windowsapps\`)) {
			continue
		}
		return path, nil
	}
	return "", errors.New("webhook route scripts in Python need a Python interpreter on PATH (python3 or python), none was found")
}

// Runs a route script on the payload, false drops the webhook and a JSON object replaces the payload
func (p *WebhookRouteProcessor) RunRouteScript(ctx context.Context, scriptValue any, payload any) (bool, *jsonutil.Object, error) {
	log := pkgLog()
	path, reason, err := webhookResolveScriptPath(ctx, scriptValue)
	if err != nil {
		return false, nil, err
	}
	if reason != "" || path == "" {
		log.Warn("[webhook] script ignored webhook: " + reason)
		return false, nil, nil
	}
	suffix := strings.ToLower(pathutil.Suffix(path))
	var interpreter string
	if suffix == ".sh" || suffix == ".bash" {
		bash, err := tools.FindBash(ctx)
		if err != nil {
			log.Warn("[webhook] script ignored webhook: " + err.Error())
			return false, nil, nil
		}
		interpreter = bash
	} else if interpreter, err = webhookSysExecutable(); err != nil {
		log.Warn("[webhook] script execution failed: " + err.Error())
		return false, nil, nil
	}
	input, err := jsonutil.Encode(payload)
	if err != nil {
		log.Warn("[webhook] script execution failed: " + err.Error())
		return false, nil, nil
	}
	child, err := tools.BuildSubprocessEnvOrdered(ctx, tools.EnvForProfileHome(ctx, nil, home.Dir(ctx)), tools.SubprocessEnvOptions{InheritProfileHome: true, ScrubSecrets: true})
	if err != nil {
		log.Warn("[webhook] script execution failed: " + err.Error())
		return false, nil, nil
	}
	result, err := executil.Run(ctx, []string{interpreter, path}, executil.Options{Dir: filepath.Dir(filepath.Clean(path)),
		Env: child.Environ(), Input: []byte(input), Timeout: time.Duration(p.ScriptTimeoutSeconds) * time.Second,
		Attr: webhookScriptProcAttr()})
	var timeout *executil.TimeoutError
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
	stdout := strings.TrimSpace(strings.ToValidUTF8(string(result.Stdout), "�"))
	stderr := strings.TrimSpace(strings.ToValidUTF8(string(result.Stderr), "�"))
	stdout, stderr = tools.RedactSensitiveText(stdout), tools.RedactSensitiveText(stderr)
	if result.ExitCode != 0 {
		level := slog.LevelInfo
		if stdout == "" && stderr == "" {
			level = slog.LevelWarn
		}
		log.Log(ctx, level, fmt.Sprintf("[webhook] script ignored webhook path=%s code=%d stderr=%s",
			pathutil.Name(path), result.ExitCode, pySliceRunes(stderr, 200)))
	}
	if result.ExitCode != 0 || stdout == "" || stdout == "[SILENT]" {
		return false, nil, nil
	}
	var transformed any
	decoded, err := jsonutil.Decode(stdout)
	var decodeErr *jsonutil.SyntaxError
	switch {
	case err == nil:
		transformed = decoded
	case errors.As(err, &decodeErr):
		merged := jsonutil.NewObject()
		if obj, ok := jsonutil.AsObject(payload); ok {
			merged = obj.Clone()
		} else {
			return false, nil, &PyError{Kind: "TypeError", Msg: fmt.Sprintf("the webhook payload is %s, expected a mapping", errchain.Kind(payload))}
		}
		merged.Set("script_output", stdout)
		transformed = merged
	default:
		return false, nil, err
	}
	obj, ok := transformed.(*jsonutil.Object)
	if !ok {
		log.Warn("[webhook] script stdout must be a JSON object or text")
		return false, nil, nil
	}
	if obj.ValueOr("[SILENT]", nil) == true || obj.ValueOr("__icebath_ignore__", nil) == true {
		return false, nil, nil
	}
	return true, obj, nil
}

// Message of a spawn failure the way Python words an OSError from Popen
func webhookExecErrorText(err error) string {
	var pathErr *fs.PathError
	if errors.As(err, &pathErr) {
		return fmt.Sprintf("%s: '%s'", pathErr.Err.Error(), pathErr.Path)
	}
	return err.Error()
}
