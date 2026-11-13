package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/dlclark/regexp2/v2"
	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/internal/pymimetypes"
	"github.com/nickheyer/icebath/pkg/internal/pypath"
	"github.com/nickheyer/icebath/pkg/internal/pyre"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/plugins"
	"github.com/nickheyer/icebath/pkg/provider"
	"github.com/nickheyer/icebath/pkg/state"
	"github.com/nickheyer/icebath/pkg/tools"
)

// One autocomplete row of a context reference provider, display defaulting to the text
type ContextCompletionItem struct {
	Text    string
	Display string
	Meta    string
}

// NewContextCompletionItem applies the dataclass post init, an empty display reads as the text
func NewContextCompletionItem(text, display, meta string) ContextCompletionItem {
	if display == "" {
		display = text
	}
	return ContextCompletionItem{Text: text, Display: display, Meta: meta}
}

// Plugin @prefix provider registered through PluginContext.register_context_reference
type ContextReferenceProvider interface {
	Prefix() string
	Description() string
	Autocomplete(ctx context.Context, query string, limit int) ([]ContextCompletionItem, error)
	Expand(ctx context.Context, target string) (*string, error)
}

// BuiltinPrefixes are the reference kinds plugins can never claim
var BuiltinPrefixes = map[string]bool{"diff": true, "staged": true, "file": true, "folder": true, "git": true, "url": true}

var (
	contextReferenceProviders   = map[string]ContextReferenceProvider{}
	contextReferenceProvidersMu sync.Mutex
)

// Plugin host object subclassing ContextReferenceProvider as a Go provider, prefix and description read once
type hostedContextReferenceProvider struct {
	obj         *plugins.HostedObject
	prefix      string
	description string
}

func (h hostedContextReferenceProvider) Prefix() string      { return h.prefix }
func (h hostedContextReferenceProvider) Description() string { return h.description }

func (h hostedContextReferenceProvider) Autocomplete(ctx context.Context, query string, limit int) ([]ContextCompletionItem, error) {
	raw, err := h.obj.Call(ctx, "autocomplete", []any{query}, map[string]any{"limit": limit})
	if err != nil {
		return nil, err
	}
	items, _ := raw.([]any)
	out := make([]ContextCompletionItem, 0, len(items))
	for _, item := range items {
		m, _ := item.(map[string]any)
		if m == nil {
			if rec, ok := item.(plugins.Record); ok {
				m = map[string]any(rec)
			}
		}
		out = append(out, NewContextCompletionItem(pyrepr.Str(m["text"]), pyrepr.Str(m["display"]), pyrepr.Str(m["meta"])))
	}
	return out, nil
}

func (h hostedContextReferenceProvider) Expand(ctx context.Context, target string) (*string, error) {
	raw, err := h.obj.Call(ctx, "expand", []any{target}, nil)
	if err != nil || raw == nil {
		return nil, err
	}
	s := pyrepr.Str(raw)
	return &s, nil
}

const (
	hostedContextEngineBase    = "agent.context_engine:ContextEngine"
	hostedContextReferenceBase = "agent.context_references:ContextReferenceProvider"
)

// Plugin host object that subclasses ContextReferenceProvider as a Go provider, any other value unchanged
func adaptHostedContextReferenceProvider(p any) any {
	obj, ok := p.(*plugins.HostedObject)
	if !ok || obj == nil || obj.Base != hostedContextReferenceBase {
		return p
	}
	prefix, _ := obj.Get(context.Background(), "prefix")
	description, _ := obj.Get(context.Background(), "description")
	return hostedContextReferenceProvider{obj: obj, prefix: pyrepr.Str(prefix), description: pyrepr.Str(description)}
}

// HostedContextObjectName names a context engine or context reference provider, false when obj is neither
func HostedContextObjectName(kind string, obj any) (string, bool) {
	switch kind {
	case "context_engine":
		if engine, ok := obj.(ContextEngine); ok {
			return engine.Name(), true
		}
		if hosted, ok := obj.(*plugins.HostedObject); ok && hosted != nil && hosted.Base == hostedContextEngineBase {
			name, err := hosted.Get(context.Background(), "name")
			if err != nil {
				return "", false
			}
			return pyrepr.Str(name), true
		}
	case "context_reference":
		if p, ok := adaptHostedContextReferenceProvider(obj).(ContextReferenceProvider); ok {
			return p.Prefix(), true
		}
	}
	return "", false
}

// RegisterContextReference registers a plugin reference provider, a foreign value failing like the Python TypeError
func RegisterContextReference(provider any) error {
	p, ok := adaptHostedContextReferenceProvider(provider).(ContextReferenceProvider)
	if !ok {
		return &plugins.HostBuiltinError{Type: "TypeError", Message: "provider must be a ContextReferenceProvider instance"}
	}
	return RegisterContextReferenceProvider(p)
}

// RegisterContextReferenceProvider claims a lower case prefix, refusing empty, builtin and taken ones
func RegisterContextReferenceProvider(provider ContextReferenceProvider) error {
	prefix := strings.TrimSpace(strings.ToLower(provider.Prefix()))
	if prefix == "" {
		return &plugins.HostBuiltinError{Type: "ValueError", Message: "prefix must be a non-empty string"}
	}
	if BuiltinPrefixes[prefix] {
		return &plugins.HostBuiltinError{Type: "ValueError", Message: fmt.Sprintf("prefix '%s' is reserved for built-in references", prefix)}
	}
	contextReferenceProvidersMu.Lock()
	defer contextReferenceProvidersMu.Unlock()
	if _, taken := contextReferenceProviders[prefix]; taken {
		return &plugins.HostBuiltinError{Type: "ValueError", Message: fmt.Sprintf("prefix '%s' is already registered", prefix)}
	}
	contextReferenceProviders[prefix] = provider
	return nil
}

// GetContextReferenceProviders is a snapshot of every registered plugin provider by prefix
func GetContextReferenceProviders() map[string]ContextReferenceProvider {
	contextReferenceProvidersMu.Lock()
	defer contextReferenceProvidersMu.Unlock()
	out := make(map[string]ContextReferenceProvider, len(contextReferenceProviders))
	for k, v := range contextReferenceProviders {
		out[k] = v
	}
	return out
}

// ClearContextReferenceProvidersForTests empties the plugin provider registry
func ClearContextReferenceProvidersForTests() {
	contextReferenceProvidersMu.Lock()
	defer contextReferenceProvidersMu.Unlock()
	contextReferenceProviders = map[string]ContextReferenceProvider{}
}

const quotedReferenceValue = "(?:`[^`\\n]+`|\"[^\"\\n]+\"|'[^'\\n]+')"

// ReferencePattern matches the built in @ references, the quoted or bare value with an optional line range
var ReferencePattern = mustPyRegex(`(?<![\w/])@(?:(?P<simple>diff|staged)\b|(?P<kind>file|folder|git|url):(?P<value>` + quotedReferenceValue + `(?::\d+(?:-\d+)?)?|\S+))`)

// Plugin fallback, any at word value pair the built in pattern did not claim
var pluginReferencePattern = mustPyRegex(`(?<![\w/])@(?P<kind>[a-zA-Z][a-zA-Z0-9_-]*):(?P<value>` + quotedReferenceValue + `(?::\d+(?:-\d+)?)?|\S+)`)

// A Python regex with its groupindex, named groups read by number the way the translator numbers them
type pyRegex struct {
	re    *regexp2.Regexp
	names map[string]int
}

// Compiles a Python regex at init, a bad pattern stopping the program like a module import error
func mustPyRegex(pattern string) *pyRegex {
	re, err := pyre.Compile(pattern)
	if err != nil {
		panic(err)
	}
	names, err := pyre.GroupIndex(pattern)
	if err != nil {
		panic(err)
	}
	return &pyRegex{re: re, names: names}
}

// Named group of a match, empty when the group did not participate
func (r *pyRegex) group(m *regexp2.Match, name string) string {
	n, ok := r.names[name]
	if !ok {
		return ""
	}
	g := m.GroupByNumber(n)
	if g == nil || g.RuneLength == 0 {
		return ""
	}
	return g.String()
}

// One regex match with its code point span and named groups
type referenceMatch struct {
	Text   string
	Start  int
	End    int
	groups map[string]string
}

func (m referenceMatch) Group(name string) string { return m.groups[name] }

// Every non overlapping match of a Python regex in order, spans in code points like re.finditer
func findAllReferenceMatches(re *pyRegex, s string) []referenceMatch {
	var out []referenceMatch
	m, err := re.re.FindStringMatch(s)
	for err == nil && m != nil {
		groups := map[string]string{}
		for name := range re.names {
			if v := re.group(m, name); v != "" {
				groups[name] = v
			}
		}
		out = append(out, referenceMatch{Text: m.String(), Start: m.RuneIndex, End: m.RuneIndex + m.RuneLength, groups: groups})
		m, err = re.re.FindNextMatch(m)
	}
	return out
}

// An at file value, a quoted or bare path with an optional start and end line range
var fileValuePattern = mustPyRegex("^(?:(?P<quote>`|\"|')(?P<qpath>.+?)(?P=quote)|(?P<path>.+?))(?::(?P<start>\\d+)(?:-(?P<end>\\d+))?)?$")

const (
	trailingReferencePunctuation = ",.;!?"
	ComposerPastesDirname        = "composer-pastes"
	maxExpandedReferences        = 16
	lineCountMaxBytes            = 4 * 1024 * 1024
	maxQuietOutputBytes          = 4 * 1024 * 1024
	outputCapExceededReturnCode  = 137
)

var (
	referenceOpeners          = map[byte]byte{')': '(', ']': '[', '}': '{'}
	sensitiveIcebathDirs      = []string{filepath.Join("skills", ".hub")}
	sensitiveHomeFiles        = []string{".ssh/authorized_keys", ".ssh/id_rsa", ".ssh/id_ed25519", ".ssh/config", ".bashrc", ".zshrc", ".profile", ".bash_profile", ".zprofile", ".netrc", ".pgpass", ".npmrc", ".pypirc"}
	referenceTextExtensions   = []string{".py", ".md", ".txt", ".json", ".yaml", ".yml", ".toml", ".js", ".ts"}
	referenceFenceLanguages   = map[string]string{".py": "python", ".js": "javascript", ".ts": "typescript", ".tsx": "tsx", ".jsx": "jsx", ".json": "json", ".md": "markdown", ".sh": "bash", ".yml": "yaml", ".yaml": "yaml", ".toml": "toml"}
	referenceReadBlockErrorFn = func(ctx context.Context, path string) (string, error) {
		return tools.ReadBlockError(referenceToolsEnv(ctx), path), nil
	}
	referenceReadTextFn   = os.ReadFile
	referenceReadPrefixFn = readFilePrefix
	quietRunnerFn         = runQuiet
	referenceOpenTextFn   = openReferenceText
	referenceGitArgs      = map[string]func(ref ContextReference) []string{
		"diff":   func(ContextReference) []string { return []string{"diff"} },
		"staged": func(ContextReference) []string { return []string{"diff", "--staged"} },
		"git": func(ref ContextReference) []string {
			n := 1
			if v, err := strconv.Atoi(strings.TrimSpace(ref.Target)); err == nil {
				n = v
			} else if strings.TrimSpace(ref.Target) != "" {
				n = pyIntOrPanic(ref.Target)
			}
			return []string{"log", fmt.Sprintf("-%d", max(1, min(n, 10))), "-p"}
		},
	}
)

// int() of a git count the way Python raises a ValueError the expander reports as a warning
func pyIntOrPanic(text string) int {
	panic(&plugins.HostBuiltinError{Type: "ValueError", Message: fmt.Sprintf("invalid literal for int() with base 10: %s", pyrepr.Repr(text))})
}

// One parsed @ reference with its span in the message and the optional line range of a file reference
type ContextReference struct {
	Raw       string
	Kind      string
	Target    string
	Start     int
	End       int
	LineStart *int
	LineEnd   *int
}

// Expansion outcome of a message, the final message with blocks and warnings appended
type ContextReferenceResult struct {
	Message         string
	OriginalMessage string
	References      []ContextReference
	Warnings        []string
	InjectedTokens  int
	Expanded        bool
	Blocked         bool
}

// URLFetcher fetches the extracted text of a url, nil meaning the web_extract tool
type URLFetcher func(ctx context.Context, url string) (string, error)

// Exactly one side set, the warning or the attached block
type referenceExpansion struct {
	warning string
	block   string
}

// ParseContextReferences reads the built in references and, with plugins, the registered plugin prefixes
func ParseContextReferences(message string, usePlugins bool) []ContextReference {
	var refs []ContextReference
	if message == "" {
		return refs
	}
	for _, m := range findAllReferenceMatches(ReferencePattern, message) {
		kind := m.Group("simple")
		if kind == "" {
			kind = m.Group("kind")
		}
		value := stripTrailingReferencePunctuation(m.Group("value"))
		var target string
		var lineStart, lineEnd *int
		switch {
		case m.Group("simple") != "":
			target = ""
		case kind == "file":
			target, lineStart, lineEnd = parseFileReferenceValue(value)
		default:
			target = stripReferenceWrappers(value)
		}
		refs = append(refs, ContextReference{Raw: m.Text, Kind: kind, Target: target, Start: m.Start, End: m.End, LineStart: lineStart, LineEnd: lineEnd})
	}
	providers := GetContextReferenceProviders()
	if !usePlugins || len(providers) == 0 {
		return refs
	}
	for _, m := range findAllReferenceMatches(pluginReferencePattern, message) {
		kind := m.Group("kind")
		if BuiltinPrefixes[kind] {
			continue
		}
		if _, ok := providers[kind]; !ok {
			continue
		}
		duplicate := false
		for _, r := range refs {
			if r.Kind == kind && r.Start == m.Start {
				duplicate = true
			}
		}
		if duplicate {
			continue
		}
		target := stripReferenceWrappers(stripTrailingReferencePunctuation(m.Group("value")))
		refs = append(refs, ContextReference{Raw: m.Text, Kind: kind, Target: target, Start: m.Start, End: m.End})
	}
	return refs
}

// Options of one preprocess run, AllowedRoot nil meaning the cwd and plugins on unless the caller bypasses them
type ContextReferenceOptions struct {
	Cwd           string
	ContextLength int
	URLFetcher    URLFetcher
	AllowedRoot   *string
	NoPlugins     bool
}

// PreprocessContextReferences expands every @ reference of a message within the token budget, concurrently per reference
func PreprocessContextReferences(ctx context.Context, message string, o ContextReferenceOptions) (*ContextReferenceResult, error) {
	refs := ParseContextReferences(message, !o.NoPlugins)
	if len(refs) == 0 {
		return &ContextReferenceResult{Message: message, OriginalMessage: message}, nil
	}
	cwdPath, err := pypath.Resolve(pypath.Expanduser(o.Cwd))
	if err != nil {
		return nil, err
	}
	allowedRoot := cwdPath
	if o.AllowedRoot != nil {
		allowedRoot, err = pypath.Resolve(pypath.Expanduser(*o.AllowedRoot))
		if err != nil {
			return nil, err
		}
	}
	hardLimit := max(1, o.ContextLength/2)
	softLimit := max(1, o.ContextLength/4)
	expanding := refs
	if len(expanding) > maxExpandedReferences {
		expanding = expanding[:maxExpandedReferences]
	}
	expanded := make([]referenceExpansion, len(expanding))
	var wg sync.WaitGroup
	for i, ref := range expanding {
		wg.Add(1)
		go func(i int, ref ContextReference) {
			defer wg.Done()
			expanded[i] = expandReference(ctx, ref, cwdPath, o.URLFetcher, allowedRoot, hardLimit)
		}(i, ref)
	}
	wg.Wait()
	var warnings []string
	for _, e := range expanded {
		if e.warning != "" {
			warnings = append(warnings, e.warning)
		}
	}
	for _, ref := range refs[len(expanding):] {
		warnings = append(warnings, fmt.Sprintf("%s: not expanded (maximum %d @-references per message)", ref.Raw, maxExpandedReferences))
	}
	var blocks []string
	injected := 0
	for _, e := range expanded {
		if e.block != "" {
			blocks = append(blocks, e.block)
			injected += provider.EstimateTokens(e.block)
		}
	}
	result := &ContextReferenceResult{Message: message, OriginalMessage: message, References: refs, Warnings: warnings, InjectedTokens: injected}
	if injected > hardLimit {
		result.Warnings = append(result.Warnings, fmt.Sprintf("@ context injection refused: %d tokens exceeds the 50%% hard limit (%d).", injected, hardLimit))
		result.Blocked = true
		return result, nil
	}
	if injected > softLimit {
		result.Warnings = append(result.Warnings, fmt.Sprintf("@ context injection warning: %d tokens exceeds the 25%% soft limit (%d).", injected, softLimit))
	}
	final := message
	if len(result.Warnings) > 0 {
		lines := make([]string, len(result.Warnings))
		for i, w := range result.Warnings {
			lines[i] = "- " + w
		}
		final = final + "\n\n--- Context Warnings ---\n" + strings.Join(lines, "\n")
	}
	if len(blocks) > 0 {
		final = final + "\n\n--- Attached Context ---\n\n" + strings.Join(blocks, "\n\n")
	}
	result.Message = strings.TrimSpace(final)
	result.Expanded = len(blocks) > 0 || len(result.Warnings) > 0
	return result, nil
}

// Text of a Python exception the expander reports, the message of a host error or the error text
func referenceErrorText(err error) string {
	var builtin *plugins.HostBuiltinError
	if errors.As(err, &builtin) {
		return builtin.Message
	}
	return err.Error()
}

// One reference expanded by kind, every failure folded into the warning side
func expandReference(ctx context.Context, ref ContextReference, cwd string, fetcher URLFetcher, allowedRoot string, maxInlineTokens int) (out referenceExpansion) {
	defer func() {
		if r := recover(); r != nil {
			err, ok := r.(error)
			if !ok {
				err = fmt.Errorf("%v", r)
			}
			out = referenceExpansion{warning: fmt.Sprintf("%s: %s", ref.Raw, referenceErrorText(err))}
		}
	}()
	switch {
	case ref.Kind == "file" || ref.Kind == "folder":
		exp, err := expandPathReference(ctx, ref, cwd, allowedRoot, maxInlineTokens)
		if err != nil {
			return referenceExpansion{warning: fmt.Sprintf("%s: %s", ref.Raw, referenceErrorText(err))}
		}
		return exp
	case referenceGitArgs[ref.Kind] != nil:
		args := referenceGitArgs[ref.Kind](ref)
		return expandGitReference(ref, cwd, args, "git "+strings.Join(args, " "))
	case ref.Kind == "url":
		content, err := fetchURLContent(ctx, ref.Target, fetcher)
		if err != nil {
			return referenceExpansion{warning: fmt.Sprintf("%s: %s", ref.Raw, referenceErrorText(err))}
		}
		if content == "" {
			return referenceExpansion{warning: fmt.Sprintf("%s: no content extracted", ref.Raw)}
		}
		return referenceExpansion{block: fmt.Sprintf("\U0001F310 %s (%d tokens)\n%s", ref.Raw, provider.EstimateTokens(content), content)}
	}
	if p, ok := GetContextReferenceProviders()[ref.Kind]; ok {
		content, err := p.Expand(ctx, ref.Target)
		if err != nil {
			return referenceExpansion{warning: fmt.Sprintf("%s: plugin expansion error: %s", ref.Raw, referenceErrorText(err))}
		}
		if content != nil {
			return referenceExpansion{block: fmt.Sprintf("\U0001F4CC %s (%d tokens)\n%s", ref.Raw, provider.EstimateTokens(*content), *content)}
		}
	}
	return referenceExpansion{warning: fmt.Sprintf("%s: unsupported reference type", ref.Raw)}
}

// @file and @folder, resolve, allow check, then inline text, a binary block or a listing
func expandPathReference(ctx context.Context, ref ContextReference, cwd, allowedRoot string, maxInlineTokens int) (referenceExpansion, error) {
	isFolder := ref.Kind == "folder"
	path, err := resolveReferencePath(ctx, cwd, ref.Target, allowedRoot)
	if err != nil {
		return referenceExpansion{}, err
	}
	if err := ensureReferencePathAllowed(ctx, path); err != nil {
		return referenceExpansion{}, err
	}
	info, statErr := os.Stat(path)
	if statErr != nil {
		return referenceExpansion{warning: fmt.Sprintf("%s: %s not found", ref.Raw, ref.Kind)}, nil
	}
	if isFolder != info.IsDir() || (!isFolder && !info.Mode().IsRegular()) {
		return referenceExpansion{warning: fmt.Sprintf("%s: path is not a %s", ref.Raw, ref.Kind)}, nil
	}
	if isFolder {
		listing := buildFolderListing(path, cwd, 200, allowedRoot)
		return referenceExpansion{block: fmt.Sprintf("\U0001F4C1 %s (%d tokens)\n%s", ref.Raw, provider.EstimateTokens(listing), listing)}, nil
	}
	var early *referenceExpansion
	var text string
	var readErr error
	guard := state.OfflineFileAccess(path, "preview context reference", func() {
		early, text, readErr = readFileReference(ref, path, maxInlineTokens)
	})
	var live *state.LiveConnectionError
	if errors.As(guard, &live) {
		return referenceExpansion{block: onDiskReferenceBlock(ctx, ref, path, "live SQLite database file",
			"not previewed: raw access would cancel SQLite's POSIX locks.", "Do not open this file directly while its database connection is live.")}, nil
	}
	if readErr != nil {
		return referenceExpansion{}, readErr
	}
	if early != nil {
		return *early, nil
	}
	return formatFileReference(ctx, ref, path, text, maxInlineTokens), nil
}

// Raw file read of an @file reference, an early refusal block or the text to inline
func readFileReference(ref ContextReference, path string, maxInlineTokens int) (*referenceExpansion, string, error) {
	binary, err := isBinaryReferenceFile(path)
	if err != nil {
		return nil, "", err
	}
	if binary {
		return &referenceExpansion{block: binaryReferenceBlock(context.Background(), ref, path)}, "", nil
	}
	if ref.LineStart != nil {
		charBudget := maxInlineTokens * provider.CharsPerToken
		text, oversized, err := readLineWindow(path, *ref.LineStart, ref.LineEnd, charBudget)
		if err != nil {
			return nil, "", err
		}
		if oversized >= 0 {
			return &referenceExpansion{block: oversizedTextReferenceBlock(context.Background(), ref, path, oversized/provider.CharsPerToken)}, "", nil
		}
		return nil, text, nil
	}
	info, err := os.Stat(path)
	if err != nil {
		return nil, "", err
	}
	if info.Size() > int64(maxInlineTokens)*int64(provider.CharsPerToken) {
		return &referenceExpansion{block: oversizedTextReferenceBlock(context.Background(), ref, path, int(info.Size())/provider.CharsPerToken)}, "", nil
	}
	raw, err := referenceReadTextFn(path)
	if err != nil {
		return nil, "", err
	}
	return nil, strings.TrimPrefix(string(raw), "\ufeff"), nil
}

// Streams only the requested line window in bounded pieces, the character total when it passed the budget
func readLineWindow(path string, lineStart int, lineEnd *int, charBudget int) (string, int, error) {
	fh, err := referenceOpenTextFn(path)
	if err != nil {
		return "", -1, err
	}
	defer fh.Close()
	lineCap := charBudget + 1
	for i := 0; i < max(lineStart-1, 0); i++ {
		if _, seen, err := nextReferenceLine(fh, lineCap, false, -1); err != nil {
			return "", -1, err
		} else if !seen {
			break
		}
	}
	end := lineStart
	if lineEnd != nil {
		end = *lineEnd
	}
	var parts []string
	total := 0
	for i := 0; i < end-lineStart+1; i++ {
		line, seen, err := nextReferenceLine(fh, lineCap, true, charBudget-total)
		if err != nil {
			return "", -1, err
		}
		if !seen {
			break
		}
		total += len(line)
		if total > charBudget {
			return "", total, nil
		}
		parts = append(parts, line)
	}
	return strings.Join(parts, ""), -1, nil
}

// Open text handle of a reference file whose readline returns at most size characters, empty at EOF
type referenceTextFile interface {
	readline(size int) (string, error)
	Close() error
}

// Text handle over a file whose readline never materializes more than the size asked
type boundedLineReader struct {
	r   io.ReadCloser
	buf []byte
}

// Opens a reference file as a text handle read in bounded readline pieces
func openReferenceText(path string) (referenceTextFile, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	return &boundedLineReader{r: f}, nil
}

func (b *boundedLineReader) Close() error { return b.r.Close() }

// One readline(size) piece, ending at a newline or the size, empty at EOF
func (b *boundedLineReader) readline(size int) (string, error) {
	var out []byte
	one := make([]byte, 1)
	for len(out) < size {
		if len(b.buf) == 0 {
			chunk := make([]byte, 4096)
			n, err := b.r.Read(chunk)
			if n > 0 {
				b.buf = append(b.buf, chunk[:n]...)
			}
			if n == 0 && err != nil {
				if err == io.EOF {
					break
				}
				return "", err
			}
		}
		if len(b.buf) == 0 {
			break
		}
		one[0] = b.buf[0]
		b.buf = b.buf[1:]
		out = append(out, one[0])
		if one[0] == '\n' {
			break
		}
	}
	return string(out), nil
}

// One logical line read in pieces of at most lineCap, collected only when asked and stopped once over remaining
func nextReferenceLine(fh referenceTextFile, lineCap int, collect bool, remaining int) (string, bool, error) {
	var pieces []string
	seen, collected := false, 0
	for {
		piece, err := fh.readline(lineCap)
		if err != nil {
			return "", false, err
		}
		if piece == "" {
			break
		}
		seen = true
		if collect {
			pieces = append(pieces, piece)
			collected += len(piece)
			if remaining >= 0 && collected > remaining {
				break
			}
		}
		if strings.HasSuffix(piece, "\n") {
			break
		}
	}
	if !seen {
		return "", false, nil
	}
	if collect {
		return strings.Join(pieces, ""), true, nil
	}
	return "", true, nil
}

// Fenced inline block of a text file, the oversized block when it passes the inline budget
func formatFileReference(ctx context.Context, ref ContextReference, path, text string, maxInlineTokens int) referenceExpansion {
	lang := referenceFenceLanguages[strings.ToLower(filepath.Ext(path))]
	textTokens := provider.EstimateTokens(text)
	if textTokens > maxInlineTokens {
		return referenceExpansion{block: oversizedTextReferenceBlock(ctx, ref, path, textTokens)}
	}
	return referenceExpansion{block: fmt.Sprintf("\U0001F4C4 %s (%d tokens)\n```%s\n%s\n```", ref.Raw, textTokens, lang, text)}
}

// Captured child output with bounded pipes, no stdin and the kill past the output cap reported as a failure
type quietResult struct {
	returncode int
	stdout     string
	stderr     string
}

// Error a child raises when it runs past the timeout, the Python TimeoutExpired
type quietTimeoutError struct{ cmd []string }

func (e *quietTimeoutError) Error() string {
	return fmt.Sprintf("Command '%v' timed out", e.cmd)
}

// Runs a command with both pipes drained to their end before the exit is collected, the child killed past the cap
func runQuiet(cmd []string, cwd string, timeout time.Duration, env []string) (*quietResult, error) {
	c := exec.Command(cmd[0], cmd[1:]...)
	c.Dir = cwd
	c.Stdin = nil
	if env != nil {
		c.Env = env
	}
	hideConsoleWindow(c)
	stdout, err := c.StdoutPipe()
	if err != nil {
		return nil, err
	}
	stderr, err := c.StderrPipe()
	if err != nil {
		return nil, err
	}
	if err := c.Start(); err != nil {
		return nil, err
	}
	var truncated sync.Once
	wasTruncated := false
	sinks := make([][]byte, 2)
	var wg sync.WaitGroup
	for i, stream := range []io.Reader{stdout, stderr} {
		wg.Add(1)
		go func(i int, stream io.Reader) {
			defer wg.Done()
			total := 0
			chunk := make([]byte, 1<<16)
			for {
				n, err := stream.Read(chunk)
				if n > 0 {
					total += n
					if total <= maxQuietOutputBytes {
						sinks[i] = append(sinks[i], chunk[:n]...)
					} else {
						truncated.Do(func() { wasTruncated = true })
						c.Process.Kill()
						io.Copy(io.Discard, stream)
						return
					}
				}
				if err != nil {
					return
				}
			}
		}(i, stream)
	}
	drained := make(chan struct{})
	go func() { wg.Wait(); close(drained) }()
	deadline := time.NewTimer(timeout)
	defer deadline.Stop()
	timedOut := false
	select {
	case <-drained:
	case <-deadline.C:
		timedOut = true
		c.Process.Kill()
		<-drained
	}
	done := make(chan error, 1)
	go func() { done <- c.Wait() }()
	var waitErr error
	if timedOut {
		<-done
		return nil, &quietTimeoutError{cmd: cmd}
	}
	select {
	case waitErr = <-done:
	case <-deadline.C:
		c.Process.Kill()
		<-done
		return nil, &quietTimeoutError{cmd: cmd}
	}
	returncode := 0
	if waitErr != nil {
		var exit *exec.ExitError
		if errors.As(waitErr, &exit) {
			returncode = exit.ExitCode()
			if returncode < 0 {
				returncode = -1
			}
		} else {
			return nil, waitErr
		}
	}
	if wasTruncated && returncode == 0 {
		returncode = outputCapExceededReturnCode
	}
	return &quietResult{returncode: returncode, stdout: strings.ToValidUTF8(string(sinks[0]), "\ufffd"), stderr: strings.ToValidUTF8(string(sinks[1]), "\ufffd")}, nil
}

// @diff, @staged and @git over a hardened noninteractive git, failures and timeouts as warnings
func expandGitReference(ref ContextReference, cwd string, args []string, label string) referenceExpansion {
	env := tools.NoninteractiveRepoGitEnv(cwd, nil)
	var environ []string
	if env != nil {
		for k, v := range env {
			environ = append(environ, k+"="+v)
		}
		sort.Strings(environ)
	}
	result, err := quietRunnerFn(append([]string{"git"}, tools.HardenGitArgv(args)...), cwd, 30*time.Second, environ)
	if err != nil {
		var timeout *quietTimeoutError
		if errors.As(err, &timeout) {
			return referenceExpansion{warning: fmt.Sprintf("%s: git command timed out (30s)", ref.Raw)}
		}
		return referenceExpansion{warning: fmt.Sprintf("%s: %s", ref.Raw, err.Error())}
	}
	if result.returncode != 0 {
		reason := strings.TrimSpace(result.stderr)
		if reason == "" {
			reason = "git command failed"
		}
		return referenceExpansion{warning: fmt.Sprintf("%s: %s", ref.Raw, reason)}
	}
	content := strings.TrimSpace(result.stdout)
	if content == "" {
		content = "(no output)"
	}
	return referenceExpansion{block: fmt.Sprintf("\U0001F9FE %s (%d tokens)\n```diff\n%s\n```", label, provider.EstimateTokens(content), content)}
}

// Extracted text of a url through the given fetcher or the web_extract tool
func fetchURLContent(ctx context.Context, url string, fetcher URLFetcher) (string, error) {
	if fetcher == nil {
		fetcher = defaultURLFetcher
	}
	content, err := fetcher(ctx, url)
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(content), nil
}

// First extracted document of web_extract in markdown, empty when none came back
func defaultURLFetcher(ctx context.Context, url string) (string, error) {
	raw := tools.WebExtractTool(ctx, referenceToolsEnv(ctx), []any{url}, "markdown", nil)
	var parsed map[string]any
	if err := json.Unmarshal([]byte(raw), &parsed); err != nil {
		return "", err
	}
	docs, _ := parsed["results"].([]any)
	if len(docs) == 0 {
		return "", nil
	}
	first, _ := docs[0].(map[string]any)
	content := first["content"]
	if !pyrepr.Truthy(content) {
		content = first["raw_content"]
	}
	if !pyrepr.Truthy(content) {
		content = ""
	}
	return strings.TrimSpace(pyrepr.Str(content)), nil
}

// Tool environment of the active home for the path guards and the web extract tool
func referenceToolsEnv(ctx context.Context) *tools.Env {
	h := home.Dir(ctx)
	cwd, _ := os.Getwd()
	return &tools.Env{Home: h, Cwd: cwd}
}

func isUnderPath(path, root string) bool {
	if path == root {
		return true
	}
	return strings.HasPrefix(path, strings.TrimRight(root, string(filepath.Separator))+string(filepath.Separator))
}

// Composer paste roots of the active home and the global root, the one anchored admission besides the workspace
func composerPasteRoots(ctx context.Context) []string {
	var out []string
	for _, dir := range tools.IcebathDirs(referenceToolsEnv(ctx)) {
		out = append(out, filepath.Join(dir, ComposerPastesDirname))
	}
	return out
}

// Whether the path sits in a gateway staged cache root of the active home
func agentStagedPath(ctx context.Context, path string) (staged bool) {
	defer func() {
		if r := recover(); r != nil {
			staged = false
		}
	}()
	for _, m := range tools.GetCacheDirectoryMounts(referenceToolsEnv(ctx), "") {
		root, err := pypath.Resolve(pypath.Expanduser(m.HostPath))
		if err != nil {
			continue
		}
		if isUnderPath(path, root) {
			return true
		}
	}
	return false
}

// Resolves a reference target against cwd and refuses one outside the workspace and the staging roots
func resolveReferencePath(ctx context.Context, cwd, target, allowedRoot string) (string, error) {
	if tools.IsNTNamespacePath(target) {
		return "", &plugins.HostBuiltinError{Type: "ValueError", Message: "path uses a Windows NT/device namespace prefix and cannot be attached"}
	}
	joined := pypath.Expanduser(target)
	if !filepath.IsAbs(joined) {
		joined = filepath.Join(cwd, joined)
	}
	resolved, err := pypath.Resolve(joined)
	if err != nil {
		return "", err
	}
	if allowedRoot != "" && !isUnderPath(resolved, allowedRoot) {
		admitted := false
		for _, root := range composerPasteRoots(ctx) {
			if isUnderPath(resolved, root) {
				admitted = true
			}
		}
		if !admitted && !agentStagedPath(ctx, resolved) {
			return "", &plugins.HostBuiltinError{Type: "ValueError", Message: "path is outside the allowed workspace"}
		}
	}
	return resolved, nil
}

// Refuses credential and internal paths, failing closed when the canonical deny list cannot be consulted
func ensureReferencePathAllowed(ctx context.Context, path string) error {
	userHome, err := pypath.Resolve(pypath.Expanduser("~"))
	if err != nil {
		return err
	}
	icebathHome, err := pypath.Resolve(home.Dir(ctx))
	if err != nil {
		return err
	}
	blockedExact := map[string]bool{filepath.Join(icebathHome, ".env"): true}
	for _, rel := range sensitiveHomeFiles {
		blockedExact[filepath.Join(userHome, filepath.FromSlash(rel))] = true
	}
	var blockedDirs []string
	for _, rel := range tools.HomeCredentialDirs() {
		blockedDirs = append(blockedDirs, filepath.Join(userHome, filepath.FromSlash(rel)))
	}
	for _, rel := range sensitiveIcebathDirs {
		blockedDirs = append(blockedDirs, filepath.Join(icebathHome, rel))
	}
	if blockedExact[path] {
		return &plugins.HostBuiltinError{Type: "ValueError", Message: "path is a sensitive credential file and cannot be attached"}
	}
	for _, dir := range blockedDirs {
		if isUnderPath(path, dir) {
			return &plugins.HostBuiltinError{Type: "ValueError", Message: "path is a sensitive credential or internal Hermes path and cannot be attached"}
		}
	}
	blocked, err := canonicalReadBlock(ctx, path)
	if err != nil {
		return err
	}
	if blocked {
		return &plugins.HostBuiltinError{Type: "ValueError", Message: "path is a sensitive credential or internal Hermes path and cannot be attached"}
	}
	return nil
}

// The canonical read deny list verdict, a failed lookup refusing the path instead of falling through
func canonicalReadBlock(ctx context.Context, path string) (blocked bool, err error) {
	defer func() {
		if r := recover(); r != nil {
			blocked, err = false, &plugins.HostBuiltinError{Type: "ValueError", Message: "path could not be verified against the credential deny-list and cannot be attached"}
		}
	}()
	reason, lookupErr := referenceReadBlockErrorFn(ctx, path)
	if lookupErr != nil {
		var builtin *plugins.HostBuiltinError
		if errors.As(lookupErr, &builtin) && builtin.Type == "ValueError" {
			return false, lookupErr
		}
		return false, &plugins.HostBuiltinError{Type: "ValueError", Message: "path could not be verified against the credential deny-list and cannot be attached"}
	}
	return reason != "", nil
}

// Trailing punctuation and unbalanced closers dropped from a bare reference value
func stripTrailingReferencePunctuation(value string) string {
	stripped := strings.TrimRight(value, trailingReferencePunctuation)
	for len(stripped) > 0 {
		last := stripped[len(stripped)-1]
		opener, isCloser := referenceOpeners[last]
		if !isCloser || strings.Count(stripped, string(last)) <= strings.Count(stripped, string(opener)) {
			break
		}
		stripped = stripped[:len(stripped)-1]
	}
	return stripped
}

// A value wrapped in matching backticks or quotes unwrapped
func stripReferenceWrappers(value string) string {
	if len(value) >= 2 && value[0] == value[len(value)-1] && strings.ContainsRune("`\"'", rune(value[0])) {
		return value[1 : len(value)-1]
	}
	return value
}

// Path and optional line range of an @file value
func parseFileReferenceValue(value string) (string, *int, *int) {
	m, err := fileValuePattern.re.FindStringMatch(value)
	if err != nil || m == nil {
		return stripReferenceWrappers(value), nil, nil
	}
	group := func(name string) string { return fileValuePattern.group(m, name) }
	start := group("start")
	if start == "" {
		return stripReferenceWrappers(value), nil, nil
	}
	path := group("qpath")
	if path == "" {
		path = group("path")
	}
	s, _ := strconv.Atoi(start)
	e := s
	if end := group("end"); end != "" {
		e, _ = strconv.Atoi(end)
	}
	return path, &s, &e
}

// Mime type says binary unless the name has a text extension, else a NUL in the first 4096 bytes
func isBinaryReferenceFile(path string) (bool, error) {
	mime, _, _ := pymimetypes.GuessType(filepath.Base(path), true)
	if mime != "" && !strings.HasPrefix(mime, "text/") {
		textual := false
		for _, ext := range referenceTextExtensions {
			if strings.HasSuffix(filepath.Base(path), ext) {
				textual = true
			}
		}
		if !textual {
			return true, nil
		}
	}
	head, err := referenceReadPrefixFn(path, 4096)
	if err != nil {
		return false, err
	}
	return bytes.IndexByte(head, 0) >= 0, nil
}

// First n bytes of a file, the sniff that never materializes the whole file
func readFilePrefix(path string, n int) ([]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	head := make([]byte, n)
	read, err := io.ReadFull(f, head)
	if err != nil && err != io.EOF && err != io.ErrUnexpectedEOF {
		return nil, err
	}
	return head[:read], nil
}

// Relative listing of a folder, shown against cwd, then the allowed root, else absolute
func buildFolderListing(path, cwd string, limit int, displayBase string) string {
	shown := ""
	for _, base := range []string{cwd, displayBase} {
		if base == "" {
			continue
		}
		if rel, err := filepath.Rel(base, path); err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) && !filepath.IsAbs(rel) {
			shown = filepath.ToSlash(rel) + "/"
			break
		}
	}
	if shown == "" {
		shown = path + "/"
	}
	lines := []string{shown}
	entries := iterVisibleEntries(path, cwd, limit)
	for _, entry := range entries {
		rel, _ := filepath.Rel(path, entry)
		depth := len(strings.Split(filepath.ToSlash(rel), "/"))
		indent := strings.Repeat("  ", max(depth-1, 0))
		if info, err := os.Stat(entry); err == nil && info.IsDir() {
			lines = append(lines, fmt.Sprintf("%s- %s/", indent, filepath.Base(entry)))
		} else {
			lines = append(lines, fmt.Sprintf("%s- %s (%s)", indent, filepath.Base(entry), fileMetadata(entry)))
		}
	}
	if len(entries) >= limit {
		lines = append(lines, "- ...")
	}
	return strings.Join(lines, "\n")
}

// Files under a folder through rg --files, falling back to a walk that skips hidden entries
func iterVisibleEntries(path, cwd string, limit int) []string {
	rg, err := quietRunnerFn([]string{"rg", "--files", path}, cwd, 10*time.Second, nil)
	if err == nil && rg.returncode == 0 {
		var output []string
		seenDirs := map[string]bool{}
		var lines []string
		for _, ln := range strings.Split(rg.stdout, "\n") {
			if strings.TrimSpace(ln) != "" {
				lines = append(lines, strings.TrimSpace(ln))
			}
		}
		if len(lines) > limit {
			lines = lines[:limit]
		}
		for _, line := range lines {
			full := line
			if !filepath.IsAbs(full) {
				full = filepath.Join(cwd, full)
			}
			for parent := filepath.Dir(full); ; parent = filepath.Dir(parent) {
				if seenDirs[parent] || !isUnderPath(parent, path) {
					if parent == filepath.Dir(parent) {
						break
					}
					continue
				}
				seenDirs[parent] = true
				output = append(output, parent)
				if parent == filepath.Dir(parent) {
					break
				}
			}
			output = append(output, full)
		}
		unique := map[string]bool{}
		var existing []string
		for _, p := range output {
			if unique[p] {
				continue
			}
			unique[p] = true
			if _, err := os.Stat(p); err == nil {
				existing = append(existing, p)
			}
		}
		sort.Slice(existing, func(i, j int) bool {
			di, dj := isDirPath(existing[i]), isDirPath(existing[j])
			if di != dj {
				return di
			}
			return existing[i] < existing[j]
		})
		return existing
	}
	var output []string
	var walk func(root string) bool
	walk = func(root string) bool {
		entries, err := os.ReadDir(root)
		if err != nil {
			return false
		}
		var dirs, files []string
		for _, e := range entries {
			name := e.Name()
			if e.IsDir() {
				if !strings.HasPrefix(name, ".") && name != "__pycache__" {
					dirs = append(dirs, name)
				}
			} else if !strings.HasPrefix(name, ".") {
				files = append(files, name)
			}
		}
		sort.Strings(dirs)
		sort.Strings(files)
		for _, name := range append(dirs, files...) {
			output = append(output, filepath.Join(root, name))
			if len(output) >= limit {
				return true
			}
		}
		for _, d := range dirs {
			if walk(filepath.Join(root, d)) {
				return true
			}
		}
		return false
	}
	walk(path)
	return output
}

func isDirPath(p string) bool {
	info, err := os.Stat(p)
	return err == nil && info.IsDir()
}

// Host path translated to the path the active terminal backend sees, the host path when none applies
func agentVisiblePath(ctx context.Context, path string) (out string) {
	defer func() {
		if r := recover(); r != nil {
			out = path
		}
	}()
	return tools.ToAgentVisibleCachePath(referenceToolsEnv(ctx), path)
}

// Shared on disk block of a file that was not inlined, pointing the model at the path and a nudge
func onDiskReferenceBlock(ctx context.Context, ref ContextReference, path, descriptor, reason, guidance string) string {
	size := "unknown size"
	if info, err := os.Stat(path); err == nil {
		size = tools.SizefmtBytes(info.Size())
	}
	return fmt.Sprintf("\U0001F4CE %s (%s, %s) — %s It is available on disk at `%s`. %s", ref.Raw, descriptor, size, reason, agentVisiblePath(ctx, path), guidance)
}

func binaryReferenceBlock(ctx context.Context, ref ContextReference, path string) string {
	mime, _, _ := pymimetypes.GuessType(filepath.Base(path), true)
	if mime == "" {
		mime = "application/octet-stream"
	}
	return onDiskReferenceBlock(ctx, ref, path, mime, "binary file, not inlined as text.",
		"Use your tools to work with it (read or convert it, extract its text, or view/render it as needed); do not tell the user the file type is unsupported.")
}

func oversizedTextReferenceBlock(ctx context.Context, ref ContextReference, path string, textTokens int) string {
	return onDiskReferenceBlock(ctx, ref, path, fmt.Sprintf("text file, approximately %d tokens", textTokens), "too large to inline safely.",
		"Use read_file with a narrow line range, search_files, or terminal/code tools to inspect only the relevant parts; do not load the entire file into context.")
}

// Size or line count summary of a folder entry, bytes past the count cap or for binaries and live databases
func fileMetadata(path string) string {
	info, err := os.Stat(path)
	if err != nil {
		return "unknown size"
	}
	size := info.Size()
	out := ""
	var inner error
	guard := state.OfflineFileAccess(path, "inspect folder entry", func() {
		binary, err := isBinaryReferenceFile(path)
		if err != nil {
			inner = err
			return
		}
		if binary || size > lineCountMaxBytes {
			out = fmt.Sprintf("%d bytes", size)
			return
		}
		f, err := os.Open(path)
		if err != nil {
			inner = err
			return
		}
		defer f.Close()
		lines := 0
		chunk := make([]byte, 1<<20)
		for {
			n, err := f.Read(chunk)
			lines += bytes.Count(chunk[:n], []byte{'\n'})
			if err != nil {
				break
			}
		}
		out = fmt.Sprintf("%d lines", lines+1)
	})
	if guard != nil || inner != nil {
		return fmt.Sprintf("%d bytes", size)
	}
	return out
}
