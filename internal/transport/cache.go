package transport

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"regexp"
	"slices"
	"strings"
	"sync"
	"unicode"
	"unicode/utf8"

	"github.com/nickheyer/icebath/internal/config"
	"github.com/nickheyer/icebath/internal/provider"
	transportconfig "github.com/nickheyer/icebath/internal/transport/config"
)

// Scaffolds kept for cache boundaries, and the characters they may hold together
const (
	scaffoldEntries = 32
	scaffoldChars   = 4 * 1024 * 1024
)

// Static scaffolds of user messages built from a template, such as a skill command's, whose end is where a prompt cache boundary goes
type scaffolds struct {
	mu   sync.Mutex
	list []string
}

func (s *scaffolds) add(prefix string) {
	if prefix == "" {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.list = append(slices.DeleteFunc(s.list, func(p string) bool { return p == prefix }), prefix)
	if len(s.list) > scaffoldEntries {
		s.list = s.list[len(s.list)-scaffoldEntries:]
	}
	total := 0
	for _, p := range s.list {
		total += utf8.RuneCountInString(p)
	}
	for len(s.list) > 1 && total > scaffoldChars {
		total -= utf8.RuneCountInString(s.list[0])
		s.list = s.list[1:]
	}
}

// Longest scaffold the content starts with and goes past with more than space, kept as the latest; empty for none
func (s *scaffolds) find(content string) string {
	if s == nil {
		return ""
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	best, at := "", -1
	for i, p := range s.list {
		if strings.HasPrefix(content, p) && strings.TrimSpace(content[len(p):]) != "" && len(p) > len(best) {
			best, at = p, i
		}
	}
	if at >= 0 {
		s.list = append(slices.Delete(s.list, at, at+1), best)
	}
	return best
}

// How a request's prompt is cached: whether it carries cache markers, whether they go on blocks as the Anthropic API places them, the
// time the provider keeps the cache, the part of the system prompt that stays the same, and where markers may go
type Caching struct {
	On     bool
	Native bool
	TTL    string
	// Start of the system prompt that stays the same from session to session, marked on its own
	Static string
	// Tool results may carry markers on their parts, which a LiteLLM proxy refuses
	ToolParts bool
	// The tool list is marked and the latest completed exchanges with it, as the Anthropic API itself takes them
	DirectTools bool
	scaffolds   *scaffolds
}

// Sources whose sessions run at a machine's pace, whose prompts are cached for five minutes when the cache time is auto
var machinePaced = []string{"subagent", "cron", "oneshot", "webhook", "kanban", "api", "tool", "batch"}

// Words the cache time setting takes for off
var cacheOff = []string{transportconfig.CacheOff, "false", "disabled", "no", "none"}

// How the request to the route is cached: off when the setting turns caching off or the route takes no markers. The kind decides first
// for its own models; else the Anthropic API and Claude on any Anthropic endpoint get blocks marked as Anthropic places them, and Claude and
// Kimi through OpenRouter, and Claude through a LiteLLM proxy, get markers on the OpenAI message shape
func (r *Registry) Caching(cfg *config.Config, request *provider.Request, route provider.Route, spec provider.Spec) Caching {
	ttl := strings.ToLower(strings.TrimSpace(cfg.GetString(transportconfig.PromptCacheTTL)))
	if slices.Contains(cacheOff, ttl) {
		return Caching{}
	}
	if ttl == transportconfig.CacheAuto {
		source := strings.ToLower(strings.TrimSpace(request.Source))
		if source == "" && cronRun.MatchString(request.SessionID) {
			source = "cron"
		}
		ttl = transportconfig.CacheLong
		if slices.Contains(machinePaced, source) {
			ttl = transportconfig.CacheShort
		}
	}
	if ttl != transportconfig.CacheLong {
		ttl = transportconfig.CacheShort
	}
	model := strings.ToLower(request.Model)
	anthropic := route.Mode == provider.ModeAnthropicMessages
	litellm := route.Mode == provider.ModeChatCompletions && strings.Contains(model, "claude") && litellmRoute(route)
	c := Caching{TTL: ttl, Static: request.StaticSystemPrefix, ToolParts: !litellmRoute(route),
		DirectTools: anthropic && provider.BaseHost(route.BaseURL) == "api.anthropic.com", scaffolds: r.shared().scaffolds}
	if spec.CacheTTL != nil && ttl == transportconfig.CacheLong {
		c.TTL = spec.CacheTTL(request.Model)
	} else if ttl == transportconfig.CacheLong && strings.Contains(model, "qwen") {
		c.TTL = transportconfig.CacheShort
	}
	if c.TTL != transportconfig.CacheLong {
		c.TTL = transportconfig.CacheShort
	}
	if route.Mode != provider.ModeAnthropicMessages && route.Mode != provider.ModeChatCompletions {
		return Caching{}
	}
	if capability, ok := spec.ModelCapabilities[request.Model]["prompt_caching"].(bool); ok {
		c.On, c.Native = capability, capability && anthropic
		return c
	}
	if spec.Caching != nil {
		if on, native, ok := spec.Caching(request.Model, route.Mode); ok {
			c.On, c.Native = on, native
			return c
		}
	}
	claude := strings.Contains(model, "claude")
	kimi := strings.Contains(model, "kimi") || strings.Contains(model, "moonshot") || strings.HasPrefix(provider.BareModel(model), "k3")
	switch {
	case anthropic && (route.Spec == "anthropic" || provider.BaseHost(route.BaseURL) == "api.anthropic.com"), anthropic && claude:
		c.On, c.Native = true, true
	case provider.BaseHostMatches(route.BaseURL, "openrouter.ai") && (claude || kimi), litellm:
		c.On = true
	}
	return c
}

// Whether a route is a LiteLLM proxy, by a whole litellm token in its provider's name or its host
func litellmRoute(route provider.Route) bool {
	return litellmToken(strings.ToLower(route.Provider), ":-_/") || litellmToken(provider.BaseHost(route.BaseURL), ".-")
}

func litellmToken(value, delimiters string) bool {
	return slices.Contains(strings.FieldsFunc(value, func(r rune) bool {
		return strings.ContainsRune(delimiters, r) || r >= 0x1c && r <= 0x1f || unicode.IsSpace(r)
	}), "litellm")
}

// Cache marker of the time the cache is kept
func (c Caching) marker() map[string]any {
	if c.TTL == transportconfig.CacheLong {
		return map[string]any{"type": "ephemeral", "ttl": "1h"}
	}
	return map[string]any{"type": "ephemeral"}
}

// Messages and tools of a request, in the OpenAI shape, with the cache markers the caching places: the static part of the system prompt and
// the rest of it, then the latest messages that can carry one, four markers in all; with direct tools the tool list and the last two completed
// exchanges instead. A request that isn't cached gets copies without markers
func (c Caching) Plan(messages, tools []map[string]any) ([]map[string]any, []map[string]any) {
	planned := make([]map[string]any, len(tools))
	for i, tool := range tools {
		copied := deepCopy(tool).(map[string]any)
		delete(copied, "cache_control")
		planned[i] = copied
	}
	out := make([]map[string]any, len(messages))
	for i, message := range messages {
		out[i] = deepCopy(message).(map[string]any)
	}
	stripMarkers(out, c.scaffolds)
	if !c.On {
		return out, planned
	}
	marker := c.marker()
	if c.DirectTools && len(planned) > 0 {
		if len(out) > 0 && out[0]["role"] == "system" {
			c.markSystem(out[0], marker, false, false)
		}
		planned[len(planned)-1]["cache_control"] = deepCopy(marker)
		ends := c.exchanges(out)
		if len(ends) > 2 {
			ends = ends[len(ends)-2:]
		}
		for _, at := range ends {
			c.mark(out[at], deepCopy(marker).(map[string]any))
		}
		c.splitScaffolds(out)
		return out, planned
	}
	used := 0
	if len(out) > 0 && out[0]["role"] == "system" {
		used = c.markSystem(out[0], marker, true, true)
	}
	var carriers []int
	for i, message := range out {
		if message["role"] != "system" && c.carries(message) {
			carriers = append(carriers, i)
		}
	}
	if budget := max(4-used, 0); len(carriers) > budget {
		carriers = carriers[len(carriers)-budget:]
	}
	for _, at := range carriers {
		c.mark(out[at], deepCopy(marker).(map[string]any))
	}
	c.splitScaffolds(out)
	return out, planned
}

// Marks the static start of the system prompt and, when asked, the rest of it; the whole prompt when it holds no static start and fallback
// is asked. The markers it placed
func (c Caching) markSystem(message, marker map[string]any, rest, fallback bool) int {
	content, text := message["content"].(string)
	if c.Static != "" && text && strings.HasPrefix(content, c.Static) {
		if suffix := content[len(c.Static):]; strings.TrimSpace(suffix) != "" {
			var tail map[string]any
			if rest {
				tail = marker
			}
			message["content"] = []any{textPart(c.Static, marker), textPart(suffix, tail)}
			if rest {
				return 2
			}
			return 1
		}
	} else if !fallback {
		return 0
	}
	c.mark(message, marker)
	return 1
}

func textPart(text string, marker map[string]any) map[string]any {
	part := map[string]any{"type": "text", "text": text}
	if marker != nil {
		part["cache_control"] = deepCopy(marker)
	}
	return part
}

// Puts the marker on the message in whatever shape its content has: on the message itself when it has no text, on its last part, or on its
// text made a part
func (c Caching) mark(message, marker map[string]any) {
	role, _ := message["role"].(string)
	if role == "tool" && !c.Native && !c.ToolParts {
		return
	}
	content := message["content"]
	text, isText := content.(string)
	if role == "tool" && c.Native || content == nil || isText && text == "" {
		if c.Native || role != "tool" && role != "assistant" {
			message["cache_control"] = marker
		}
		return
	}
	if isText {
		if parts := c.scaffold(message, marker); parts != nil {
			message["content"] = parts
		} else {
			message["content"] = []any{textPart(text, marker)}
		}
		return
	}
	if parts, ok := content.([]any); ok && len(parts) > 0 {
		if last, ok := parts[len(parts)-1].(map[string]any); ok {
			last["cache_control"] = marker
		}
	}
}

// Whether a marker on the message is one the provider honours
func (c Caching) carries(message map[string]any) bool {
	if c.Native {
		return true
	}
	if message["role"] == "tool" && !c.ToolParts {
		return false
	}
	switch content := message["content"].(type) {
	case []any:
		if len(content) > 0 {
			_, ok := content[len(content)-1].(map[string]any)
			return ok
		}
	case string:
		return content != ""
	}
	return false
}

// Ends of the completed tool runs and turns of the messages, where a marker may go
func (c Caching) exchanges(messages []map[string]any) []int {
	run := func(start int) int {
		end := start
		for end < len(messages) && messages[end]["role"] == "tool" {
			end++
		}
		return end
	}
	var ends []int
	for i := 0; i < len(messages); {
		message := messages[i]
		role, _ := message["role"].(string)
		calls, _ := message["tool_calls"].([]any)
		switch {
		case role == "system":
			i++
		case role == "assistant" && len(calls) > 0:
			end := run(i + 1)
			if end > i+1 && c.carries(messages[end-1]) {
				ends = append(ends, end-1)
			}
			i = end
		case role == "tool":
			i = run(i)
		default:
			text, isText := message["content"].(string)
			open := role == "user" && i+1 < len(messages) || role == "assistant" && (message["content"] == nil || isText && text == "")
			if !open && c.carries(message) {
				ends = append(ends, i)
			}
			i++
		}
	}
	return ends
}

// Scaffold and the rest of a text user message whose start is a known scaffold, the scaffold carrying the marker; nil for any other message
func (c Caching) scaffold(message, marker map[string]any) []any {
	content, ok := message["content"].(string)
	if !ok || message["role"] != "user" {
		return nil
	}
	prefix := c.scaffolds.find(content)
	if prefix == "" {
		return nil
	}
	return []any{textPart(prefix, marker), textPart(content[len(prefix):], nil)}
}

// Splits every text user message that starts with a scaffold, unmarked
func (c Caching) splitScaffolds(messages []map[string]any) {
	for _, message := range messages {
		if parts := c.scaffold(message, nil); parts != nil {
			message["content"] = parts
		}
	}
}

// Takes the markers a message already carries off it and puts back the shape it had before a plan split it: plain text parts joined again
// when the plan made them, a scaffold split undone
func stripMarkers(messages []map[string]any, known *scaffolds) {
	for _, message := range messages {
		delete(message, "cache_control")
		parts, ok := message["content"].([]any)
		if !ok {
			continue
		}
		role, _ := message["role"].(string)
		pair := role == "user" && len(parts) == 2
		firstMarked := false
		if pair {
			first, ok1 := parts[0].(map[string]any)
			second, ok2 := parts[1].(map[string]any)
			pair = ok1 && ok2
			if pair {
				_, m0 := first["cache_control"]
				_, m1 := second["cache_control"]
				firstMarked = m0 && !m1
			}
		}
		for _, p := range parts {
			if part, ok := p.(map[string]any); ok {
				delete(part, "cache_control")
			}
		}
		plain := len(parts) > 0
		var joined strings.Builder
		for _, p := range parts {
			part, ok := p.(map[string]any)
			text, isText := part["text"].(string)
			kind, hasKind := part["type"].(string)
			if !ok || !isText || hasKind && kind != "text" || len(part) > 2 || len(part) == 2 && !hasKind {
				plain = false
				break
			}
			joined.WriteString(text)
		}
		if !plain {
			continue
		}
		split := firstMarked
		if !split && pair {
			first := parts[0].(map[string]any)
			prefix := known.find(joined.String())
			split = prefix != "" && prefix == first["text"]
		}
		if len(parts) == 1 || role == "system" && len(parts) == 2 || split {
			message["content"] = joined.String()
		}
	}
}

// Copy of JSON shaped data that shares nothing with it
func deepCopy(v any) any {
	switch t := v.(type) {
	case map[string]any:
		out := make(map[string]any, len(t))
		for k, value := range t {
			out[k] = deepCopy(value)
		}
		return out
	case []any:
		out := make([]any, len(t))
		for i, value := range t {
			out[i] = deepCopy(value)
		}
		return out
	case []map[string]any:
		out := make([]any, len(t))
		for i, value := range t {
			out[i] = deepCopy(value)
		}
		return out
	case json.RawMessage:
		var decoded any
		if json.Unmarshal(t, &decoded) == nil {
			return decoded
		}
		return string(t)
	}
	return v
}

var cronRun = regexp.MustCompile(`^(cron_.+)_\d{8}_\d{6}$`)

// Scope of the provider's caches a session shares: a cron run's job, so every run of it shares one, else the session
func Scope(session string) string {
	if m := cronRun.FindStringSubmatch(session); m != nil {
		return m[1]
	}
	return session
}

// Prompt cache key of at most 64 characters, a hash of a longer one; empty for none
func BoundedKey(key string) string {
	key = strings.TrimSpace(key)
	if len(key) <= 64 {
		return key
	}
	sum := sha256.Sum256([]byte(key))
	return "pck_" + hex.EncodeToString(sum[:])[:24]
}

// Prompt cache key of the scope, the instructions and the tools sorted by name, so requests that share them share the provider's cache;
// empty without instructions or tools
func CacheKey(instructions string, tools []map[string]any, scope string) string {
	if instructions == "" && len(tools) == 0 {
		return ""
	}
	named := func(tool map[string]any) string {
		if name := toolName(tool); name != "" {
			return name
		}
		kind, _ := tool["type"].(string)
		return kind
	}
	sorted := slices.Clone(tools)
	slices.SortStableFunc(sorted, func(a, b map[string]any) int { return strings.Compare(named(a), named(b)) })
	part := ""
	if len(sorted) > 0 {
		raw, _ := json.Marshal(sorted)
		part = string(raw)
	}
	sum := sha256.Sum256([]byte(scope + "\x00" + instructions + "\x00" + part))
	return "pck_" + hex.EncodeToString(sum[:])[:24]
}
