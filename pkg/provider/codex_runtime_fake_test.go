package provider

import "sync"

type fakeCodexCall struct {
	name string
	args []any
}

// Records every hook fire of the runtime against a stand in agent, unset hooks stay nil
type fakeCodexAgent struct {
	mu                                                          sync.Mutex
	calls                                                       []fakeCodexCall
	sessionID, provider, requestedProvider, model, baseURL, key string
	platform, ephemeral, cached, cwd                            string
	reasoning                                                   map[string]any
	validTools                                                  map[string]bool
	hideCommentary, hold, interruptRequested                    bool
	interruptMessage                                            any
	itersSinceSkill, skillNudgeInterval                         int
	session                                                     *CodexAppServerSession
	sessionPrompt, sessionModelProvider                         *string
	db                                                          CodexSessionDB
	dbCreated, checkpointRequired, lastCompactionInPlace        bool
	usage                                                       map[string]int
	apiCalls                                                    int
	costUSD                                                     float64
	costStatus, costSource, streamedText                        string
	compressor                                                  CodexContextCompressor
	anchor                                                      *UsageAnchor
	anchorSet                                                   int
	flush                                                       func([]map[string]any) (bool, error)
	hooks                                                       map[string]bool
	panics                                                      map[string]bool
}

func newFakeCodexAgent(hooks ...string) *fakeCodexAgent {
	a := &fakeCodexAgent{usage: map[string]int{}, hooks: map[string]bool{}, panics: map[string]bool{}, validTools: map[string]bool{}}
	for _, h := range hooks {
		a.hooks[h] = true
	}
	return a
}

func (a *fakeCodexAgent) record(name string, args ...any) {
	a.mu.Lock()
	a.calls = append(a.calls, fakeCodexCall{name, args})
	boom := a.panics[name]
	a.mu.Unlock()
	if boom {
		panic("boom")
	}
}

func (a *fakeCodexAgent) callsOf(name string) [][]any {
	a.mu.Lock()
	defer a.mu.Unlock()
	var out [][]any
	for _, c := range a.calls {
		if c.name == name {
			out = append(out, c.args)
		}
	}
	return out
}

func (a *fakeCodexAgent) SessionID() string                        { return a.sessionID }
func (a *fakeCodexAgent) Provider() string                         { return a.provider }
func (a *fakeCodexAgent) RequestedProvider() string                { return a.requestedProvider }
func (a *fakeCodexAgent) Model() string                            { return a.model }
func (a *fakeCodexAgent) BaseURL() string                          { return a.baseURL }
func (a *fakeCodexAgent) APIKey() string                           { return a.key }
func (a *fakeCodexAgent) Platform() string                         { return a.platform }
func (a *fakeCodexAgent) ReasoningConfig() map[string]any          { return a.reasoning }
func (a *fakeCodexAgent) EphemeralSystemPrompt() string            { return a.ephemeral }
func (a *fakeCodexAgent) CachedSystemPrompt() string               { return a.cached }
func (a *fakeCodexAgent) SessionCwd() string                       { return a.cwd }
func (a *fakeCodexAgent) ValidToolNames() map[string]bool          { return a.validTools }
func (a *fakeCodexAgent) ShowCommentary() bool                     { return !a.hideCommentary }
func (a *fakeCodexAgent) HoldStream() bool                         { return a.hold }
func (a *fakeCodexAgent) InterruptRequested() bool                 { return a.interruptRequested }
func (a *fakeCodexAgent) InterruptMessage() any                    { return a.interruptMessage }
func (a *fakeCodexAgent) ClearInterrupt()                          { a.record("clear_interrupt"); a.interruptRequested = false }
func (a *fakeCodexAgent) ItersSinceSkill() int                     { return a.itersSinceSkill }
func (a *fakeCodexAgent) SetItersSinceSkill(n int)                 { a.itersSinceSkill = n }
func (a *fakeCodexAgent) SkillNudgeInterval() int                  { return a.skillNudgeInterval }
func (a *fakeCodexAgent) CodexSession() *CodexAppServerSession     { return a.session }
func (a *fakeCodexAgent) SetCodexSession(s *CodexAppServerSession) { a.session = s }
func (a *fakeCodexAgent) CodexSessionPrompt() *string              { return a.sessionPrompt }
func (a *fakeCodexAgent) SetCodexSessionPrompt(p *string)          { a.sessionPrompt = p }
func (a *fakeCodexAgent) CodexSessionModelProvider() *string       { return a.sessionModelProvider }
func (a *fakeCodexAgent) SetCodexSessionModelProvider(p *string)   { a.sessionModelProvider = p }
func (a *fakeCodexAgent) CodexSessionDB() CodexSessionDB           { return a.db }
func (a *fakeCodexAgent) SessionDBCreated() bool                   { return a.dbCreated }
func (a *fakeCodexAgent) EnsureDBSession() error {
	a.record("ensure_db_session")
	a.dbCreated = true
	return nil
}
func (a *fakeCodexAgent) SessionSource() string { return "cli" }
func (a *fakeCodexAgent) FlushMessagesToSessionDB(m []map[string]any) (bool, error) {
	a.record("flush", len(m))
	if a.flush != nil {
		return a.flush(m)
	}
	return true, nil
}
func (a *fakeCodexAgent) AddSessionAPICalls(n int)               { a.apiCalls += n }
func (a *fakeCodexAgent) AddSessionUsage(key string, value int)  { a.usage[key] += value }
func (a *fakeCodexAgent) AddSessionEstimatedCostUSD(usd float64) { a.costUSD += usd }
func (a *fakeCodexAgent) SetSessionCost(status, source string) {
	a.costStatus, a.costSource = status, source
}
func (a *fakeCodexAgent) SetLastCompactionInPlace(on bool)               { a.lastCompactionInPlace = on }
func (a *fakeCodexAgent) SetCurrentStreamedAssistantText(t string)       { a.streamedText = t }
func (a *fakeCodexAgent) CompressionCheckpointRequired() bool            { return a.checkpointRequired }
func (a *fakeCodexAgent) CodexContextCompressor() CodexContextCompressor { return a.compressor }
func (a *fakeCodexAgent) SetUsageAnchor(anchor *UsageAnchor)             { a.anchor = anchor; a.anchorSet++ }
func (a *fakeCodexAgent) EmitCompactionStatus()                          { a.record("emit_compaction_status") }
func (a *fakeCodexAgent) EmitDiagnosticStatus(text string)               { a.record("emit_diagnostic_status", text) }
func (a *fakeCodexAgent) PublishProviderNativeCompaction()               { a.record("publish_provider_native") }

func (a *fakeCodexAgent) EventCallback() func(string, map[string]any) {
	if !a.hooks["event_callback"] {
		return nil
	}
	return func(event string, data map[string]any) { a.record("event_callback", event, data) }
}
func (a *fakeCodexAgent) ToolProgressCallback() func(string, string, any, any, *CodexToolProgressExtra) {
	if !a.hooks["tool_progress_callback"] {
		return nil
	}
	return func(event, name string, preview, args any, extra *CodexToolProgressExtra) {
		a.record("tool_progress_callback", event, name, preview, args, extra)
	}
}
func (a *fakeCodexAgent) ToolStartCallback() func(string, string, any) {
	if !a.hooks["tool_start_callback"] {
		return nil
	}
	return func(id, name string, args any) { a.record("tool_start_callback", id, name, args) }
}
func (a *fakeCodexAgent) ToolCompleteCallback() func(string, string, any, string) {
	if !a.hooks["tool_complete_callback"] {
		return nil
	}
	return func(id, name string, args any, result string) {
		a.record("tool_complete_callback", id, name, args, result)
	}
}
func (a *fakeCodexAgent) StreamDeltaHook(attr string) func(string) {
	if !a.hooks[attr] {
		return nil
	}
	return func(text string) { a.record(attr, text) }
}
func (a *fakeCodexAgent) InterimAssistantMessageHook() func(map[string]any) {
	if !a.hooks["_emit_interim_assistant_message"] {
		return nil
	}
	return func(m map[string]any) { a.record("_emit_interim_assistant_message", m) }
}
func (a *fakeCodexAgent) TouchActivityHook() func(string) {
	if !a.hooks["_touch_activity"] {
		return nil
	}
	return func(desc string) { a.record("_touch_activity", desc) }
}
func (a *fakeCodexAgent) SyncExternalMemoryForTurnHook() func(any, any, bool, []map[string]any) {
	if !a.hooks["_sync_external_memory_for_turn"] {
		return nil
	}
	return func(orig, final any, interrupted bool, m []map[string]any) {
		a.record("_sync_external_memory_for_turn", orig, final, interrupted, len(m))
	}
}
func (a *fakeCodexAgent) SpawnBackgroundReviewHook() func([]map[string]any, bool, bool) {
	if !a.hooks["_spawn_background_review"] {
		return nil
	}
	return func(m []map[string]any, mem, skills bool) { a.record("_spawn_background_review", len(m), mem, skills) }
}
