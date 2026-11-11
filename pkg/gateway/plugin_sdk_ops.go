package gateway

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"slices"
	"strings"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/internal/pyint"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/media"
	"github.com/nickheyer/icebath/pkg/plugins"
	"github.com/nickheyer/icebath/pkg/provider"
	"github.com/nickheyer/icebath/pkg/selfupdate/updaterecord"
	"github.com/nickheyer/icebath/pkg/tools"
)

type pluginSDKOp func(ctx context.Context, h string, args []any, kwargs map[string]any) (any, error)

var pluginSDKOps = map[string]pluginSDKOp{
	"provider_media.save_url":     sdkProviderMediaSaveURL,
	"read_credential_pool":        sdkReadCredentialPool,
	"open_credentialed_url":       sdkOpenCredentialedURL,
	"chat_catalog_ids":            sdkChatCatalogIDs,
	"format_secret_source_suffix": sdkFormatSecretSourceSuffix,
	"get_version_info":            sdkGetVersionInfo,
}

// Answers the media, credential, catalog, secret source and version plugin SDK calls of a hosted plugin
func ServePluginSDK(ctx context.Context, h, op string, args []any, kwargs map[string]any) (any, error) {
	run, ok := pluginSDKOps[op]
	if !ok {
		return nil, &plugins.HostBuiltinError{Type: "ValueError", Message: fmt.Sprintf("unknown plugin SDK call %s", pyrepr.Repr(op))}
	}
	return run(ctx, h, args, kwargs)
}

// One parameter of a Python signature, required when it carries no default
type sdkParam struct {
	name     string
	kwOnly   bool
	optional bool
	def      any
}

func sdkTypeError(format string, a ...any) error {
	return &plugins.HostBuiltinError{Type: "TypeError", Message: fmt.Sprintf(format, a...)}
}

// English list of quoted names the way CPython words missing arguments
func sdkNameList(names []string) string {
	quoted := make([]string, len(names))
	for i, n := range names {
		quoted[i] = pyrepr.StrRepr(n)
	}
	switch len(quoted) {
	case 1:
		return quoted[0]
	case 2:
		return quoted[0] + " and " + quoted[1]
	}
	return strings.Join(quoted[:len(quoted)-1], ", ") + ", and " + quoted[len(quoted)-1]
}

// Binds positional and keyword arguments to a signature with the TypeError wording CPython raises
func bindSDKSignature(fn string, params []sdkParam, args []any, kwargs map[string]any) (map[string]any, error) {
	var positional []sdkParam
	required := 0
	for _, p := range params {
		if !p.kwOnly {
			positional = append(positional, p)
			if !p.optional {
				required++
			}
		}
	}
	if len(args) > len(positional) {
		takes := fmt.Sprintf("%d positional argument", len(positional))
		if required != len(positional) {
			takes = fmt.Sprintf("from %d to %d positional argument", required, len(positional))
		}
		if len(positional) != 1 {
			takes += "s"
		}
		verb := "were"
		if len(args) == 1 {
			verb = "was"
		}
		return nil, sdkTypeError("%s() takes %s but %d %s given", fn, takes, len(args), verb)
	}
	bound := map[string]any{}
	for i, v := range args {
		bound[positional[i].name] = v
	}
	keys := make([]string, 0, len(kwargs))
	for k := range kwargs {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	for _, k := range keys {
		if !slices.ContainsFunc(params, func(p sdkParam) bool { return p.name == k }) {
			return nil, sdkTypeError("%s() got an unexpected keyword argument %s", fn, pyrepr.StrRepr(k))
		}
		if _, dup := bound[k]; dup {
			return nil, sdkTypeError("%s() got multiple values for argument %s", fn, pyrepr.StrRepr(k))
		}
		bound[k] = kwargs[k]
	}
	var missingPositional, missingKeyword []string
	for _, p := range params {
		if _, ok := bound[p.name]; ok {
			continue
		}
		switch {
		case p.optional:
			bound[p.name] = p.def
		case p.kwOnly:
			missingKeyword = append(missingKeyword, p.name)
		default:
			missingPositional = append(missingPositional, p.name)
		}
	}
	for _, m := range []struct {
		names []string
		kind  string
	}{{missingPositional, "positional"}, {missingKeyword, "keyword-only"}} {
		if len(m.names) > 0 {
			plural := ""
			if len(m.names) > 1 {
				plural = "s"
			}
			return nil, sdkTypeError("%s() missing %d required %s argument%s: %s", fn, len(m.names), m.kind, plural, sdkNameList(m.names))
		}
	}
	return bound, nil
}

func sdkString(fn, name string, v any) (string, error) {
	s, ok := v.(string)
	if !ok {
		return "", sdkTypeError("%s() argument %s must be str, not %s", fn, pyrepr.StrRepr(name), pyrepr.TypeName(v))
	}
	return s, nil
}

func sdkFloat(fn, name string, v any) (float64, error) {
	switch v.(type) {
	case int, int64, float64, bool:
		return pyint.Float(v)
	}
	return 0, sdkTypeError("%s() argument %s must be a number, not %s", fn, pyrepr.StrRepr(name), pyrepr.TypeName(v))
}

func sdkInt(fn, name string, v any) (int64, error) {
	switch n := v.(type) {
	case int:
		return int64(n), nil
	case int64:
		return n, nil
	case bool:
		if n {
			return 1, nil
		}
		return 0, nil
	}
	return 0, sdkTypeError("%s() argument %s must be int, not %s", fn, pyrepr.StrRepr(name), pyrepr.TypeName(v))
}

func sdkStringMap(fn, name string, v any) (map[string]string, error) {
	raw, ok := v.(map[string]any)
	if !ok {
		return nil, sdkTypeError("%s() argument %s must be dict, not %s", fn, pyrepr.StrRepr(name), pyrepr.TypeName(v))
	}
	out := make(map[string]string, len(raw))
	for k, item := range raw {
		s, err := sdkString(fn, name, item)
		if err != nil {
			return nil, err
		}
		out[k] = s
	}
	return out, nil
}

// Tool environment of one home with its config, the way the host builds it for plugin facing calls
func sdkToolsEnv(ctx context.Context, h string) *tools.Env {
	cfg, _ := config.LoadReadonly(ctx, h, nil)
	cwd, _ := os.Getwd()
	return &tools.Env{Home: h, Config: cfg, Cwd: cwd, Log: slog.Default()}
}

// Stream downloads a provider URL into the media cache of the home under the SSRF guard, the cached path
func sdkProviderMediaSaveURL(ctx context.Context, h string, args []any, kwargs map[string]any) (any, error) {
	const fn = "save_url"
	a, err := bindSDKSignature(fn, []sdkParam{{name: "kind"}, {name: "url"}, {name: "prefix", kwOnly: true}, {name: "timeout", kwOnly: true},
		{name: "max_bytes", kwOnly: true}, {name: "chunk_size", kwOnly: true}, {name: "content_types", kwOnly: true},
		{name: "url_extensions", kwOnly: true}, {name: "default_extension", kwOnly: true}, {name: "label", kwOnly: true},
		{name: "empty_error", kwOnly: true}, {name: "headers", kwOnly: true, optional: true},
		{name: "require_known_content_type", kwOnly: true, optional: true, def: false},
		{name: "trusted_origin", kwOnly: true, optional: true, def: false}}, args, kwargs)
	if err != nil {
		return nil, err
	}
	text := map[string]string{}
	for _, name := range []string{"kind", "url", "prefix", "default_extension", "label", "empty_error"} {
		if text[name], err = sdkString(fn, name, a[name]); err != nil {
			return nil, err
		}
	}
	timeout, err := sdkFloat(fn, "timeout", a["timeout"])
	if err != nil {
		return nil, err
	}
	maxBytes, err := sdkInt(fn, "max_bytes", a["max_bytes"])
	if err != nil {
		return nil, err
	}
	chunkSize, err := sdkInt(fn, "chunk_size", a["chunk_size"])
	if err != nil {
		return nil, err
	}
	contentTypes, err := sdkStringMap(fn, "content_types", a["content_types"])
	if err != nil {
		return nil, err
	}
	rawExtensions, ok := a["url_extensions"].([]any)
	if !ok {
		return nil, sdkTypeError("%s() argument 'url_extensions' must be a sequence, not %s", fn, pyrepr.TypeName(a["url_extensions"]))
	}
	extensions := make([]string, len(rawExtensions))
	for i, item := range rawExtensions {
		if extensions[i], err = sdkString(fn, "url_extensions", item); err != nil {
			return nil, err
		}
	}
	var headers map[string]string
	if config.PyTruthy(a["headers"]) {
		if headers, err = sdkStringMap(fn, "headers", a["headers"]); err != nil {
			return nil, err
		}
	}
	return media.SaveURL(ctx, sdkToolsEnv(ctx, h), text["kind"], text["url"], media.SaveURLOptions{
		Prefix: text["prefix"], Timeout: secondsDuration(timeout), MaxBytes: maxBytes, ChunkSize: int(chunkSize),
		ContentTypes: contentTypes, URLExtensions: extensions, DefaultExtension: text["default_extension"],
		Label: text["label"], EmptyError: text["empty_error"], Headers: headers,
		RequireKnownContentType: config.PyTruthy(a["require_known_content_type"]), TrustedOrigin: config.PyTruthy(a["trusted_origin"])})
}

func sdkPoolRows(rows []map[string]any) []any {
	out := make([]any, len(rows))
	for i, row := range rows {
		out[i] = row
	}
	return out
}

// Persisted credential pool of the home, one provider slice when a provider id is given
func sdkReadCredentialPool(_ context.Context, h string, args []any, kwargs map[string]any) (any, error) {
	a, err := bindSDKSignature("read_credential_pool", []sdkParam{{name: "provider_id", optional: true}}, args, kwargs)
	if err != nil {
		return nil, err
	}
	store := provider.NewStore(h)
	if a["provider_id"] == nil {
		pools, err := store.ReadCredentialPools()
		if err != nil {
			return nil, err
		}
		merged := make(map[string]any, len(pools))
		for k, rows := range pools {
			merged[k] = sdkPoolRows(rows)
		}
		return merged, nil
	}
	id, err := sdkString("read_credential_pool", "provider_id", a["provider_id"])
	if err != nil {
		return nil, err
	}
	rows, err := store.ReadCredentialPool(id)
	if err != nil {
		return nil, err
	}
	return sdkPoolRows(rows), nil
}

// urllib.error.HTTPError of a response outside the 2xx range
type sdkHTTPError struct {
	Code int
	Msg  string
}

func (e *sdkHTTPError) Error() string      { return fmt.Sprintf("HTTP Error %d: %s", e.Code, e.Msg) }
func (e *sdkHTTPError) PyTypeName() string { return "HTTPError" }

// Sends one credentialed request under the redirect and TLS policy, the status, final URL, headers and body
func sdkOpenCredentialedURL(ctx context.Context, _ string, args []any, kwargs map[string]any) (any, error) {
	const fn = "open_credentialed_url"
	a, err := bindSDKSignature(fn, []sdkParam{{name: "url"}, {name: "method", kwOnly: true, optional: true, def: "GET"},
		{name: "headers", kwOnly: true, optional: true}, {name: "data", kwOnly: true, optional: true},
		{name: "timeout", kwOnly: true, optional: true}}, args, kwargs)
	if err != nil {
		return nil, err
	}
	rawURL, err := sdkString(fn, "url", a["url"])
	if err != nil {
		return nil, err
	}
	method, err := sdkString(fn, "method", a["method"])
	if err != nil {
		return nil, err
	}
	var body io.Reader
	if a["data"] != nil {
		data, ok := a["data"].([]byte)
		if !ok {
			return nil, sdkTypeError("%s() argument 'data' must be bytes, not %s", fn, pyrepr.TypeName(a["data"]))
		}
		body = bytes.NewReader(data)
	}
	req, err := http.NewRequestWithContext(ctx, method, rawURL, body)
	if err != nil {
		return nil, &plugins.HostBuiltinError{Type: "ValueError", Message: err.Error()}
	}
	if a["headers"] != nil {
		pairs, ok := a["headers"].([]any)
		if !ok {
			return nil, sdkTypeError("%s() argument 'headers' must be a list of pairs, not %s", fn, pyrepr.TypeName(a["headers"]))
		}
		for _, raw := range pairs {
			pair, ok := raw.([]any)
			if !ok || len(pair) != 2 {
				return nil, sdkTypeError("%s() argument 'headers' must be a list of pairs, not %s", fn, pyrepr.TypeName(raw))
			}
			req.Header.Set(pyrepr.Str(pair[0]), pyrepr.Str(pair[1]))
		}
	}
	timeout := 0.0
	if a["timeout"] != nil {
		if timeout, err = sdkFloat(fn, "timeout", a["timeout"]); err != nil {
			return nil, err
		}
	}
	resp, err := provider.OpenCredentialedURL(req, secondsDuration(timeout), nil, nil)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		reason := http.StatusText(resp.StatusCode)
		if _, text, ok := strings.Cut(resp.Status, " "); ok {
			reason = text
		}
		return nil, &sdkHTTPError{Code: resp.StatusCode, Msg: reason}
	}
	names := make([]string, 0, len(resp.Header))
	for name := range resp.Header {
		names = append(names, name)
	}
	slices.Sort(names)
	headers := []any{}
	for _, name := range names {
		for _, value := range resp.Header[name] {
			headers = append(headers, []any{name, value})
		}
	}
	finalURL := rawURL
	if resp.Request != nil && resp.Request.URL != nil {
		finalURL = resp.Request.URL.String()
	}
	return map[string]any{"status": resp.StatusCode, "url": finalURL, "headers": headers, "body": raw}, nil
}

// Model ids of a models payload minus the generation rows
func sdkChatCatalogIDs(_ context.Context, _ string, args []any, kwargs map[string]any) (any, error) {
	a, err := bindSDKSignature("chat_catalog_ids", []sdkParam{{name: "items"}}, args, kwargs)
	if err != nil {
		return nil, err
	}
	out := []any{}
	for _, id := range provider.ChatCatalogIDs(a["items"]) {
		out = append(out, id)
	}
	return out, nil
}

// Suffix naming the external secret source a key of the home came from, empty for .env and shell keys
func sdkFormatSecretSourceSuffix(_ context.Context, h string, args []any, kwargs map[string]any) (any, error) {
	const fn = "format_secret_source_suffix"
	a, err := bindSDKSignature(fn, []sdkParam{{name: "env_var"}}, args, kwargs)
	if err != nil {
		return nil, err
	}
	name, err := sdkString(fn, "env_var", a["env_var"])
	if err != nil {
		return nil, err
	}
	return provider.FormatSecretSourceSuffix(h, name), nil
}

func sdkOptional[T any](p *T) any {
	if p == nil {
		return nil
	}
	return *p
}

// Version record of this process under the field names of the Python dataclass
func sdkGetVersionInfo(_ context.Context, _ string, args []any, kwargs map[string]any) (any, error) {
	if _, err := bindSDKSignature("get_version_info", nil, args, kwargs); err != nil {
		return nil, err
	}
	info, err := updaterecord.GetVersionInfo()
	if err != nil {
		return nil, err
	}
	return map[string]any{"base_version": info.BaseVersion, "derived_version": info.DerivedVersion,
		"distance": sdkOptional(info.Distance), "commit": sdkOptional(info.Commit), "branch": sdkOptional(info.Branch),
		"source": info.Source, "dirty": info.Dirty, "commit_date": sdkOptional(info.CommitDate),
		"distribution": sdkOptional(info.Distribution)}, nil
}
