package trajectorycompressor

import (
	"context"
	"fmt"
	"os"
	"reflect"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/nickheyer/icebath/pkg/errchain"
	"github.com/nickheyer/icebath/pkg/logging"
	"github.com/nickheyer/icebath/pkg/provider"
	"github.com/nickheyer/icebath/pkg/tools"
	anyutil "github.com/nickheyer/icebath/pkg/utils/any"
	errorutil "github.com/nickheyer/icebath/pkg/utils/error"
	jsonutil "github.com/nickheyer/icebath/pkg/utils/json"
	urlutil "github.com/nickheyer/icebath/pkg/utils/url"
)

// Ordered hostname to provider table, codex matched separately
var providerHosts = [][2]string{
	{"openrouter.ai", "openrouter"}, {"z.ai", "zai"}, {"moonshot.ai", "kimi-coding"},
	{"moonshot.cn", "kimi-coding"}, {"api.kimi.com", "kimi-coding"}, {"arcee.ai", "arcee"}, {"minimaxi.com", "minimax-cn"},
	{"minimax.io", "minimax"},
}

const (
	summaryFallback = "[CONTEXT SUMMARY]: [Summary generation failed - previous turns contained tool calls and responses that have been compressed to save context space.]"
	summaryPrefix   = "[CONTEXT SUMMARY]:"
)

// Compressor fits agent trajectories into a token budget by summarizing part of the middle
type Compressor struct {
	Config      *CompressionConfig
	Aggregate   *AggregateMetrics
	tokenizer   Encoder
	useCallLLM  bool
	llmProvider string
	client      provider.AuxClient
	logger      *logging.Logger
}

func runtimeError(format string, args ...any) error {
	return &errorutil.Error{Kind: "RuntimeError", Msg: fmt.Sprintf(format, args...)}
}

// Compressor with its tokenizer loaded and summarizer route resolved
func NewCompressor(ctx context.Context, cfg *CompressionConfig) (*Compressor, error) {
	c := &Compressor{Config: cfg, Aggregate: &AggregateMetrics{}}
	if err := c.initTokenizer(); err != nil {
		return nil, err
	}
	if err := c.initSummarizer(ctx); err != nil {
		return nil, err
	}
	c.logger = logging.GetLogger("trajectorycompressor")
	return c, nil
}

// Loads the configured tokenizer
func (c *Compressor) initTokenizer() error {
	tok, err := LoadTokenizer(c.Config.TokenizerName)
	if err != nil {
		return runtimeError("Failed to load tokenizer '%s': %s", c.Config.TokenizerName, err)
	}
	c.tokenizer = tok
	fmt.Printf("✅ Loaded tokenizer: %s\n", c.Config.TokenizerName)
	return nil
}

// Routes summaries through call_llm for a known provider host, else a raw client on base_url and api_key_env
func (c *Compressor) initSummarizer(ctx context.Context) error {
	prov, err := c.detectProvider()
	if err != nil {
		return err
	}
	c.useCallLLM = prov != ""
	if prov != "" {
		c.llmProvider = prov
		client, _, err := provider.ResolveProviderClient(ctx, prov, c.Config.SummarizationModel, provider.ProviderClientOptions{})
		if err != nil {
			return err
		}
		if client == nil {
			return runtimeError("Provider '%s' is not configured. Check your API key, or start icebath and type /setup model", prov)
		}
	} else {
		apiKey := os.Getenv(c.Config.APIKeyEnv)
		if apiKey == "" {
			return runtimeError("Missing API key. Set %s environment variable.", c.Config.APIKeyEnv)
		}
		c.client = provider.NewOpenAIClient(apiKey, provider.ToOpenAIBaseURL(c.Config.BaseURL), provider.OpenAIClientOptions{})
	}
	fmt.Printf("✅ Initialized summarizer client: %s\n", c.Config.SummarizationModel)
	fmt.Printf("   Max concurrent requests: %d\n", c.Config.MaxConcurrentRequests)
	return nil
}

// Provider name for the configured base_url, empty when the host is unknown
func (c *Compressor) detectProvider() (string, error) {
	url := c.Config.BaseURL
	host, err := urlutil.BaseHost(url)
	if err != nil {
		return "", err
	}
	if host == "chatgpt.com" && strings.Contains(strings.ToLower(url), "/backend-api/codex") {
		return "codex", nil
	}
	for _, ph := range providerHosts {
		ok, err := urlutil.BaseHostMatches(url, ph[0])
		if err != nil {
			return "", err
		}
		if ok {
			return ph[1], nil
		}
	}
	return "", nil
}

// Length of a turn value: the characters of text, the items of a list or a mapping, refused for any other value
func valueLen(v any) (int, error) {
	switch x := v.(type) {
	case string:
		return utf8.RuneCountInString(x), nil
	case []any:
		return len(x), nil
	case *jsonutil.Object:
		return x.Len(), nil
	}
	return 0, &errorutil.Error{Kind: "TypeError", Msg: "a turn value must be text, a list or a mapping, got " + errchain.Kind(v)}
}

// Token count of a turn value, zero when empty, a quarter of its length when it is not encodable text
func (c *Compressor) CountTokens(text any) (int, error) {
	if !anyutil.IsTrue(text) {
		return 0, nil
	}
	if s, ok := text.(string); ok {
		if ids, err := c.tokenizer.Encode(s); err == nil {
			return len(ids), nil
		}
	}
	l, err := valueLen(text)
	return l / 4, err
}

// A turn as the mapping it must be, refused for any other value
func turnDict(turn any) (*jsonutil.Object, error) {
	if o, ok := turn.(*jsonutil.Object); ok {
		return o, nil
	}
	return nil, &errorutil.Error{Kind: "AttributeError", Msg: "a turn must be a mapping, got " + errchain.Kind(turn)}
}

// Token counts of every turn value in order
func (c *Compressor) CountTurnTokens(traj []any) ([]int, error) {
	out := make([]int, len(traj))
	for i, turn := range traj {
		d, err := turnDict(turn)
		if err != nil {
			return nil, err
		}
		if out[i], err = c.CountTokens(d.ValueOr("value", "")); err != nil {
			return nil, err
		}
	}
	return out, nil
}

// Total tokens of a trajectory
func (c *Compressor) CountTrajectoryTokens(traj []any) (int, error) {
	counts, err := c.CountTurnTokens(traj)
	total := 0
	for _, n := range counts {
		total += n
	}
	return total, err
}

// Protected turn set with the compressible region after the head and before the tail
func (c *Compressor) findProtectedIndices(traj []any) (map[int]bool, int, int, error) {
	n := len(traj)
	firstSeen := map[string]int{}
	for i, turn := range traj {
		d, err := turnDict(turn)
		if err != nil {
			return nil, 0, 0, err
		}
		role := d.ValueOr("from", "")
		switch role.(type) {
		case []any, *jsonutil.Object:
			return nil, 0, 0, &errorutil.Error{Kind: "TypeError", Msg: fmt.Sprintf("turn %d: from must be a single value, got %s", i, errchain.Kind(role))}
		}
		if s, ok := role.(string); ok {
			if _, seen := firstSeen[s]; !seen {
				firstSeen[s] = i
			}
		}
	}
	protected := map[int]bool{}
	for _, rf := range []struct {
		role string
		on   bool
	}{{"system", c.Config.ProtectFirstSystem}, {"human", c.Config.ProtectFirstHuman}, {"gpt", c.Config.ProtectFirstGPT}, {"tool", c.Config.ProtectFirstTool}} {
		if i, ok := firstSeen[rf.role]; ok && rf.on {
			protected[i] = true
		}
	}
	for i := max(0, n-c.Config.ProtectLastNTurns); i < n; i++ {
		protected[i] = true
	}
	start, end := 0, n
	for i := range protected {
		if i < n/2 {
			start = max(start, i+1)
		} else {
			end = min(end, i)
		}
	}
	return protected, start, end, nil
}

// Whether a boundary at i splits no gpt tool_call and tool tool_response pair
func cleanBoundary(traj []any, i int) bool {
	if i >= len(traj) {
		return true
	}
	d, _ := traj[i].(*jsonutil.Object)
	if d == nil {
		return true
	}
	from, ok := d.Get("from")
	return !ok || from != "tool"
}

// Nearest clean boundary within [minIdx, maxIdx], forward preferred so an orphan tool turn joins its gpt turn
func snapBoundary(traj []any, idx, minIdx, maxIdx int) int {
	forward := idx
	for forward < maxIdx && !cleanBoundary(traj, forward) {
		forward++
	}
	if cleanBoundary(traj, forward) {
		return forward
	}
	backward := idx
	for backward > minIdx && !cleanBoundary(traj, backward) {
		backward--
	}
	return backward
}

// Turns [start, end) formatted for the summarization prompt with long values elided in the middle
func (c *Compressor) extractTurnContentForSummary(traj []any, start, end int) (string, error) {
	parts := make([]string, 0, end-start)
	for i := start; i < end; i++ {
		d, err := turnDict(traj[i])
		if err != nil {
			return "", err
		}
		value := d.ValueOr("value", "")
		n, err := valueLen(value)
		if err != nil {
			return "", err
		}
		text := anyutil.String(value)
		if n > 3000 {
			s, ok := value.(string)
			if !ok {
				return "", &errorutil.Error{Kind: "TypeError", Msg: fmt.Sprintf("turn %d: value is %s of %d items, only text can be shortened for the summary", i, errchain.Kind(value), n)}
			}
			text = tools.ElideMiddle(s, 1500, 500)
		}
		from := d.ValueOr("from", "unknown")
		fs, ok := from.(string)
		if !ok {
			return "", &errorutil.Error{Kind: "AttributeError", Msg: fmt.Sprintf("turn %d: from must be text, got %s", i, errchain.Kind(from))}
		}
		parts = append(parts, fmt.Sprintf("[Turn %d - %s]:\n%s", i, strings.ToUpper(fs), text))
	}
	return strings.Join(parts, "\n\n"), nil
}

// Summarization prompt asking for a neutral factual summary of about summary_target_tokens
func (c *Compressor) summaryPrompt(content string) string {
	return fmt.Sprintf(`Summarize the following agent conversation turns concisely. This summary will replace these turns in the conversation history.

Write the summary from a neutral perspective describing what the assistant did and learned. Include:
1. What actions the assistant took (tool calls, searches, file operations)
2. Key information or results obtained
3. Any important decisions or findings
4. Relevant data, file names, values, or outputs

Keep the summary factual and informative. Target approximately %d tokens.

---
TURNS TO SUMMARIZE:
%s
---

Write only the summary, starting with "[CONTEXT SUMMARY]:" prefix.`, c.Config.SummaryTargetTokens, content)
}

// Temperature a direct call sends, nil when the model manages temperature itself and the call must omit it
func EffectiveTemperatureForModel(model string, requested *float64, baseURL string) (*float64, error) {
	omit, fixed, err := provider.FixedTemperatureForModel(model, baseURL, "")
	if err != nil {
		return nil, err
	}
	if omit {
		return nil, nil
	}
	if fixed == nil {
		return requested, nil
	}
	return fixed, nil
}

// Temperature and create kwargs of one summary call, the raw client carrying temperature only when one is sent
func (c *Compressor) summaryRequest(prompt string) (*float64, map[string]any, error) {
	cfg := c.Config
	requested := cfg.Temperature
	temperature, err := EffectiveTemperatureForModel(cfg.SummarizationModel, &requested, cfg.BaseURL)
	if err != nil {
		return nil, nil, err
	}
	kwargs := map[string]any{"model": cfg.SummarizationModel, "messages": []any{map[string]any{"role": "user", "content": prompt}},
		"max_tokens": cfg.SummaryTargetTokens * 2}
	if !c.useCallLLM && temperature != nil {
		kwargs["temperature"] = *temperature
	}
	return temperature, kwargs, nil
}

// Lowercased finish_reason of the first choice, empty when absent
func responseFinishReason(response any) string {
	choices := reflect.ValueOf(provider.MessageField(response, "choices"))
	if !choices.IsValid() || (choices.Kind() != reflect.Slice && choices.Kind() != reflect.Array) || choices.Len() == 0 {
		return ""
	}
	reason := provider.MessageField(choices.Index(0).Interface(), "finish_reason")
	if !anyutil.IsTrue(reason) {
		return ""
	}
	return strings.ToLower(strings.TrimSpace(anyutil.String(reason)))
}

// Summary text carrying the CONTEXT SUMMARY prefix exactly once, a length stop failing the attempt
func finishSummary(response any) (string, error) {
	if responseFinishReason(response) == "length" {
		return "", runtimeError("trajectory summarization hit the output token cap (finish_reason=length); summary is incomplete")
	}
	msg, err := provider.FirstChoiceMessage(response)
	if err != nil {
		return "", err
	}
	content := provider.MessageField(msg, "content")
	text := ""
	if s, ok := content.(string); ok {
		text = s
	} else if anyutil.IsTrue(content) {
		text = anyutil.String(content)
	}
	text = strings.TrimSpace(text)
	if strings.HasPrefix(text, summaryPrefix) {
		return text, nil
	}
	if text == "" {
		return summaryPrefix, nil
	}
	return summaryPrefix + " " + text, nil
}

// Counts a failed attempt and returns the jittered backoff, false on the last attempt
func (c *Compressor) summaryAttemptFailed(m *TrajectoryMetrics, attempt int, err error) (time.Duration, bool) {
	m.SummarizationErrors++
	c.logger.Warning(fmt.Sprintf("Summarization attempt %d failed: %s", attempt+1, err))
	if attempt < c.Config.MaxRetries-1 {
		return provider.JitteredBackoff(attempt+1, provider.BackoffOptions{BaseDelay: time.Duration(c.Config.RetryDelay * float64(time.Second)),
			MaxDelay: 30 * time.Second, JitterRatio: 0.5}), true
	}
	return 0, false
}

// Summary of content with retries, the fallback summary after the last failure and nil when no attempt is allowed
func (c *Compressor) generateSummary(ctx context.Context, content string, m *TrajectoryMetrics, async bool) (any, error) {
	prompt := c.summaryPrompt(content)
	for attempt := 0; attempt < c.Config.MaxRetries; attempt++ {
		m.SummarizationAPICalls++
		summary, err := c.summaryAttempt(ctx, prompt, async)
		if err == nil {
			return summary, nil
		}
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		delay, retry := c.summaryAttemptFailed(m, attempt, err)
		if !retry {
			return summaryFallback, nil
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(delay):
		}
	}
	return nil, nil
}

// One summary call through call_llm or the raw client
func (c *Compressor) summaryAttempt(ctx context.Context, prompt string, async bool) (string, error) {
	temperature, kwargs, err := c.summaryRequest(prompt)
	if err != nil {
		return "", err
	}
	var response any
	if c.useCallLLM {
		call := map[string]any{"provider": c.llmProvider, "temperature": nil}
		if temperature != nil {
			call["temperature"] = *temperature
		}
		for k, v := range kwargs {
			call[k] = v
		}
		if async {
			response, err = provider.AsyncCallLLM(ctx, call)
		} else {
			response, err = provider.CallLLM(ctx, call)
		}
	} else {
		response, err = c.client.CreateChatCompletion(ctx, kwargs)
	}
	if err != nil {
		return "", err
	}
	return finishSummary(response)
}

// Region [start, until) to summarize, filling the pre compression metrics, false when nothing can shrink
func (c *Compressor) planCompression(traj []any, m *TrajectoryMetrics) (int, int, bool, error) {
	cfg := c.Config
	turnTokens, err := c.CountTurnTokens(traj)
	if err != nil {
		return 0, 0, false, err
	}
	total := 0
	for _, n := range turnTokens {
		total += n
	}
	m.OriginalTurns, m.CompressedTurns = len(traj), len(traj)
	m.OriginalTokens, m.CompressedTokens = total, total
	if total <= cfg.TargetMaxTokens {
		m.SkippedUnderTarget = true
		return 0, 0, false, nil
	}
	m.StillOverLimit = true
	_, start, end, err := c.findProtectedIndices(traj)
	if err != nil {
		return 0, 0, false, err
	}
	start = snapBoundary(traj, start, start, end)
	if start >= end {
		return 0, 0, false, nil
	}
	target := total - cfg.TargetMaxTokens + cfg.SummaryTargetTokens
	accumulated, until := 0, start
	for i := start; i < end; i++ {
		accumulated += turnTokens[i]
		until = i + 1
		if accumulated >= target {
			break
		}
	}
	if accumulated < target && until < end {
		until = end
	}
	until = snapBoundary(traj, until, start, end)
	region := 0
	for i := start; i < until; i++ {
		region += turnTokens[i]
	}
	if until <= start || region <= cfg.SummaryTargetTokens {
		return 0, 0, false, nil
	}
	m.TurnsCompressedStartIdx, m.TurnsCompressedEndIdx = start, until
	m.TurnsInCompressedRegion = until - start
	return start, until, true, nil
}

// Shallow copy of an ordered dict keeping its key order
func copyObject(o *jsonutil.Object) *jsonutil.Object {
	out := jsonutil.NewObject()
	for _, k := range o.Keys() {
		out.Set(k, o.Value(k))
	}
	return out
}

// Head with the summary notice on system turns, the summary human turn, then the verbatim tail, metrics finalized
func (c *Compressor) assembleCompressed(traj []any, start, until int, summary any, m *TrajectoryMetrics) ([]any, error) {
	compressed := make([]any, 0, start+1+len(traj)-until)
	for i, turn := range traj[:start] {
		t := copyObject(turn.(*jsonutil.Object))
		if from, _ := t.Get("from"); from == "system" && c.Config.AddSummaryNotice {
			value, ok := t.Get("value")
			if !ok {
				return nil, &errorutil.Error{Kind: "KeyError", Msg: fmt.Sprintf("turn %d: the system turn has no value to take the summary notice", i)}
			}
			s, isStr := value.(string)
			if !isStr {
				return nil, &errorutil.Error{Kind: "TypeError", Msg: fmt.Sprintf("turn %d: the system turn's value must be text to take the summary notice, got %s", i, errchain.Kind(value))}
			}
			t.Set("value", s+c.Config.SummaryNoticeText)
		}
		compressed = append(compressed, t)
	}
	head := jsonutil.NewObject()
	head.Set("from", "human")
	head.Set("value", summary)
	compressed = append(compressed, head)
	for _, turn := range traj[until:] {
		compressed = append(compressed, copyObject(turn.(*jsonutil.Object)))
	}
	tokens, err := c.CountTrajectoryTokens(compressed)
	if err != nil {
		return nil, err
	}
	m.CompressedTurns = len(compressed)
	m.CompressedTokens = tokens
	m.TurnsRemoved = m.OriginalTurns - m.CompressedTurns
	m.TokensSaved = m.OriginalTokens - m.CompressedTokens
	m.CompressionRatio = float64(m.CompressedTokens) / float64(max(m.OriginalTokens, 1))
	m.WasCompressed = true
	m.StillOverLimit = m.CompressedTokens > c.Config.TargetMaxTokens
	return compressed, nil
}

// Turns of a conversations value, which must be a list
func iterTurns(v any) ([]any, error) {
	if turns, ok := v.([]any); ok {
		return turns, nil
	}
	return nil, &errorutil.Error{Kind: "TypeError", Msg: "conversations must be a list of turns, got " + errchain.Kind(v)}
}

func (c *Compressor) compress(ctx context.Context, trajectory any, async bool) (any, *TrajectoryMetrics, error) {
	m := NewTrajectoryMetrics()
	traj, err := iterTurns(trajectory)
	if err != nil {
		return nil, nil, err
	}
	start, until, ok, err := c.planCompression(traj, m)
	if err != nil {
		return nil, nil, err
	}
	if !ok {
		return trajectory, m, nil
	}
	content, err := c.extractTurnContentForSummary(traj, start, until)
	if err != nil {
		return nil, nil, err
	}
	summary, err := c.generateSummary(ctx, content, m, async)
	if err != nil {
		return nil, nil, err
	}
	compressed, err := c.assembleCompressed(traj, start, until, summary, m)
	if err != nil {
		return nil, nil, err
	}
	return compressed, m, nil
}

// Compresses one trajectory into the target budget through the async call_llm surface
func (c *Compressor) CompressTrajectoryAsync(ctx context.Context, trajectory any) (any, *TrajectoryMetrics, error) {
	return c.compress(ctx, trajectory, true)
}

// Compresses the conversations of one JSONL entry, attaching per trajectory metrics when it was compressed
func (c *Compressor) ProcessEntry(ctx context.Context, entry any) (any, *TrajectoryMetrics, error) {
	obj, ok := entry.(*jsonutil.Object)
	if !ok {
		return entry, NewTrajectoryMetrics(), nil
	}
	conversations, has := obj.Get("conversations")
	if !has {
		return entry, NewTrajectoryMetrics(), nil
	}
	compressed, m, err := c.CompressTrajectoryAsync(ctx, conversations)
	if err != nil {
		return nil, nil, err
	}
	result := copyObject(obj)
	result.Set("conversations", compressed)
	if c.Config.MetricsPerTrajectory && m.WasCompressed {
		result.Set("compression_metrics", m.ToDict())
	}
	return result, m, nil
}
