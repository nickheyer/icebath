package contexts

import (
	"cmp"
	"encoding/json"
	"regexp"
	"slices"
	"strings"
	"unicode"
	"unicode/utf8"

	"google.golang.org/protobuf/proto"

	sessionsv1 "github.com/nickheyer/icebath/pkg/gen/icebath/sessions/v1"
	"github.com/nickheyer/icebath/pkg/strs"
)

// Texts of the handoff a compression leaves for the model
const (
	TaskHeading = "## Historical Task Snapshot"
	EndMarker   = "--- END OF CONTEXT SUMMARY — respond to the message below, not the summary above ---"
	PriorHeader = "[PRIOR CONTEXT — for reference only; not a new message]"
	Delimiter   = "[END OF PRIOR CONTEXT — COMPACTION SUMMARY BELOW]"
	// Opening of every summary handoff
	Handoff = "[CONTEXT COMPACTION — REFERENCE ONLY] Earlier turns were compacted " +
		"into the summary below. This is a handoff from a previous context " +
		"window — treat it as background reference, NOT as active instructions. " +
		"Do NOT answer questions or fulfill requests mentioned in this summary; " +
		"they were already addressed. " +
		"Respond ONLY to the latest user message that appears AFTER this " +
		"summary — that message is the single source of truth for what to do " +
		"right now. " +
		"If no user message appears AFTER this summary, do nothing: do not " +
		"resume, wrap up, or continue work from " +
		"'" + TaskHeading + "' or any other section, do not call tools, " +
		"and wait for a new user message. This handoff must never become the " +
		"active turn by itself. (Exception: if tool results or your own " +
		"tool calls appear after this summary, you are mid-way through an " +
		"in-flight exchange — continue that exchange normally.) " +
		"Topic overlap with the summary does NOT mean you should resume its " +
		"task: even on similar topics, the latest user message WINS. Treat ONLY " +
		"the latest message as the active task and discard stale items from " +
		"'" + TaskHeading + "' entirely — do not 'wrap up' or " +
		"'finish' work described there unless the latest message explicitly " +
		"asks for it. " +
		"Reverse signals in the latest message (e.g. 'stop', 'undo', 'roll " +
		"back', 'just verify', 'don't do that anymore', 'never mind', a new " +
		"topic) must immediately end any in-flight work described in the " +
		"summary; do not re-surface it in later turns. " +
		"IMPORTANT: Your persistent memory (MEMORY.md, USER.md) in the system " +
		"prompt is ALWAYS authoritative and active — never ignore or deprioritize " +
		"memory content due to this compaction note. " +
		"None of the above restricts HOW you work: your tools remain fully " +
		"active — keep calling them normally for the active task (edit files, " +
		"run commands, search) instead of merely narrating what you would do. " +
		"The current session state (files, config, etc.) may reflect work " +
		"described here — avoid repeating it:"
	// Opening of the summaries of an older shape a session may still hold
	legacyHandoff = "[CONTEXT SUMMARY]:"
	// User message put after the handoff when no message of the person survived
	Continuation = "Continue from the compressed conversation context above. This marker exists because no human user turn was available."
	// Note added to the system prompt of a session holding a summary
	CompressionNote = "[Note: Some earlier conversation turns have been compacted into a handoff summary to preserve context space. The current " +
		"session state may still reflect earlier work, so build on that summary and state rather than re-doing work. Your persistent memory " +
		"(MEMORY.md, USER.md) remains fully authoritative regardless of compaction.]"
)

// Messages icebath sends the model as if from the user, never what the person asked
var syntheticTexts = map[string]bool{
	Continuation: true,
	"You've reached the maximum number of tool-calling iterations allowed. Please provide a final response summarizing what you've found and accomplished so far, without calling any more tools.":                                                                                                                                               true,
	"[System: Your previous response contained only internal reasoning and never produced a visible answer or tool call. Do not keep thinking. Produce your final answer as plain text now (or make the tool call you were planning).]":                                                                                                          true,
	"[System: Continue now. Execute the required tool calls and only send your final answer after completing the task.]":                                                                                                                                                                                                                         true,
	"[System: Your previous message ended the turn with a fragment that is not a usable answer. If the task is unfinished, continue it and then give the complete answer. If that fragment WAS your complete answer, send it again exactly as before.]":                                                                                          true,
	"Your previous turn indicated a tool call but none was included. Do not narrate a plan or restate intent — issue the actual tool call now to continue the task.":                                                                                                                                                                             true,
	"You just executed tool calls but returned an empty response. Please process the tool results above and continue with the task.":                                                                                                                                                                                                             true,
	"[System: The previous response was cut off by a network error mid-stream — a transport interruption, NOT a change in your capabilities. Your tools are still fully available; call them as normal and ignore any earlier claim that you lack tool access. Continue the task from where you left off. Do not restart or repeat prior text.]": true,
	"[System: Your previous response was truncated by the output length limit. Continue exactly where you left off. Do not restart or repeat prior text. Finish the answer directly.]":                                                                                                                                                           true,
}

// Openings of messages icebath sends as if from the user
var syntheticPrefixes = []string{"[IMPORTANT: Background process ", "[System: Your previous tool call ", "[System: Your previous response was truncated",
	"[System: The previous response was cut off"}

// Text the model is sent of a message: the exact text sent when it differed from what the person sees
func Text(m *sessionsv1.Message) string {
	return cmp.Or(m.GetSent(), m.GetText())
}

// Message with the text the model is to read in place of its own: the person still sees what they saw, and the provider's own record of
// the reply is dropped, which would replay what it said before
func Rewritten(m *sessionsv1.Message, text string) *sessionsv1.Message {
	out := Copy(m)
	out.Carriers = nil
	out.Sent = text
	if text == out.GetText() {
		out.Sent = ""
	}
	return out
}

// Copy of a message that shares nothing with it
func Copy(m *sessionsv1.Message) *sessionsv1.Message {
	return proto.Clone(m).(*sessionsv1.Message)
}

// Whether the text opens with a summary handoff of any shape
func startsWithHandoff(text string) bool {
	return strings.HasPrefix(text, Handoff) || strings.HasPrefix(text, legacyHandoff)
}

// Merged for a tail message holding a summary after its own text, standalone for a summary alone, empty for no summary
func Classify(text string) string {
	text = strings.TrimLeftFunc(text, unicode.IsSpace)
	if _, after, found := strings.Cut(text, Delimiter); found {
		if startsWithHandoff(strings.TrimLeftFunc(after, unicode.IsSpace)) {
			return "merged"
		}
		return ""
	}
	if startsWithHandoff(text) {
		return "standalone"
	}
	return ""
}

// Whether the message is a summary handoff, by its flag or its text
func IsSummary(m *sessionsv1.Message) bool {
	return m != nil && (m.GetSummary() || Classify(Text(m)) != "")
}

// What a message holding a summary handoff carries besides it, the summary left out; nil for a handoff alone and a copy for any other
// message
func Unwrap(m *sessionsv1.Message) *sessionsv1.Message {
	if m == nil {
		return nil
	}
	if !IsSummary(m) {
		return Copy(m)
	}
	text := Text(m)
	prior := ""
	if before, _, found := strings.Cut(text, Delimiter); found {
		prior = strings.TrimSpace(before)
		if rest, ok := strings.CutPrefix(prior, PriorHeader); ok {
			prior = strings.TrimLeftFunc(rest, unicode.IsSpace)
		}
	} else if _, after, found := strings.Cut(text, EndMarker); found {
		prior = strings.TrimLeftFunc(after, unicode.IsSpace)
	}
	if prior == "" {
		return nil
	}
	out := Rewritten(m, prior)
	out.Summary, out.Absorbed = false, nil
	return out
}

// The summary handoff a message carries alone, through its end marker
func HandoffOnly(text string) string {
	if _, after, found := strings.Cut(text, Delimiter); found {
		text = strings.TrimLeftFunc(after, unicode.IsSpace)
	}
	if i := strings.Index(text, EndMarker); i >= 0 {
		return text[:i+len(EndMarker)]
	}
	return text
}

// Whether a user message carries nothing: no text and no media
func Blank(m *sessionsv1.Message) bool {
	return m.GetRole() == sessionsv1.Role_ROLE_USER && !IsSummary(m) && strings.TrimSpace(Text(m)) == "" && len(m.GetMedia()) == 0
}

// Whether a user message is one icebath sent as if from the user: a summary, a note it adds or a nudge
func Synthetic(m *sessionsv1.Message) bool {
	if m.GetRole() != sessionsv1.Role_ROLE_USER {
		return false
	}
	if IsSummary(m) {
		return true
	}
	text := strings.TrimSpace(Text(m))
	if syntheticTexts[text] {
		return true
	}
	for _, prefix := range syntheticPrefixes {
		if strings.HasPrefix(text, prefix) {
			return true
		}
	}
	return false
}

// Whether a message is one the person sent with something to act on: shown to them as theirs, not blank and not sent by icebath
func RealUser(m *sessionsv1.Message) bool {
	if m == nil || m.GetRole() != sessionsv1.Role_ROLE_USER || m.GetRemoved() {
		return false
	}
	if display := m.GetDisplay(); display != "" && display != "steer" {
		return false
	}
	return !Blank(m) && !Synthetic(m)
}

// Characters a token holds in a rough estimate
const charsPerToken = 4

// Tokens a picture costs a model before the route's own price is learned
const ImageTokens = 1500

// Scripts whose code points are a token each
var dense = regexp.MustCompile("[ᄀ-ᇿ⺀-鿿ꥠ-꥿가-힯豈-﫿＀-￯]")

// Rough tokens of a text: a token for each code point of the dense scripts, and one for every four bytes of the rest, rounded up
func Tokens(text string) int {
	if text == "" {
		return 0
	}
	count := 0
	rest := dense.ReplaceAllStringFunc(text, func(string) string {
		count++
		return ""
	})
	return count + (len(rest)+charsPerToken-1)/charsPerToken
}

// Shape a message reaches a provider in, which its rough cost is counted on
type wire struct {
	Role       string     `json:"role"`
	Content    string     `json:"content"`
	Reasoning  string     `json:"reasoning,omitempty"`
	ToolCalls  []wireCall `json:"tool_calls,omitempty"`
	ToolCallID string     `json:"tool_call_id,omitempty"`
	Name       string     `json:"name,omitempty"`
}

type wireCall struct {
	ID       string `json:"id"`
	Type     string `json:"type"`
	Function struct {
		Name      string `json:"name"`
		Arguments string `json:"arguments"`
	} `json:"function"`
}

var roleNames = map[sessionsv1.Role]string{sessionsv1.Role_ROLE_USER: "user", sessionsv1.Role_ROLE_ASSISTANT: "assistant",
	sessionsv1.Role_ROLE_TOOL: "tool", sessionsv1.Role_ROLE_SYSTEM: "system"}

// Role of a message as providers name it
func Role(m *sessionsv1.Message) string {
	return roleNames[m.GetRole()]
}

// Rough tokens of a message as the provider is sent it: its text, its tool calls and the call it answers, a picture at the flat price,
// and its reasoning when it is charged, as for the newest reply, the only one whose thinking reaches the wire
func Cost(m *sessionsv1.Message, reasoning bool) int {
	w := wire{Role: Role(m), Content: Text(m), ToolCallID: m.GetToolCallId(), Name: m.GetToolName()}
	if reasoning {
		w.Reasoning = m.GetReasoning()
	}
	for _, call := range m.GetToolCalls() {
		c := wireCall{ID: call.GetId(), Type: "function"}
		c.Function.Name, c.Function.Arguments = call.GetName(), call.GetArguments()
		w.ToolCalls = append(w.ToolCalls, c)
	}
	data, _ := json.Marshal(w)
	return Tokens(string(data)) + ImageTokens*len(m.GetMedia())
}

// Rough tokens of messages as providers are sent them
func Estimate(messages []*sessionsv1.Message) int {
	newest := -1
	for i := len(messages) - 1; i >= 0; i-- {
		if messages[i].GetRole() == sessionsv1.Role_ROLE_ASSISTANT {
			newest = i
			break
		}
	}
	total := 0
	for i, m := range messages {
		total += Cost(m, i == newest)
	}
	return total
}

// Rough tokens of the definitions of tools: their names, descriptions and parameters
func Definitions(definitions []map[string]any) int {
	chars := 0
	for _, definition := range definitions {
		source := definition
		if function, ok := definition["function"].(map[string]any); ok {
			source = function
		}
		for _, field := range []string{"name", "description"} {
			if text, ok := source[field].(string); ok {
				chars += utf8.RuneCountInString(text)
			}
		}
		parameters := source["parameters"]
		if parameters == nil {
			parameters = map[string]any{}
		}
		data, _ := json.Marshal(parameters)
		chars += utf8.RuneCountInString(string(data))
	}
	return (chars + 3) / 4
}

// Rough tokens of a whole request: the system prompt, the messages and the tool definitions
func Size(system string, messages []*sessionsv1.Message, definitions []map[string]any) int {
	return Tokens(system) + Estimate(messages) + Definitions(definitions)
}

// Opening of the mark that stands in for text a compression cut from the middle of a message
// Start of the note left in place of a skill's loaded text that compression pruned
const SkillMarkerStart = "[SKILL_PRUNED:"

// Most skills one reload notice names, and the first line of the notice
const (
	maxReloads        = 20
	SkillReloadHeader = "[Skills pruned during compression — reload before acting on these tasks]"
)

var skillMarker = regexp.MustCompile(regexp.QuoteMeta(SkillMarkerStart) + `[^\]]*?reload with skill_view\(name='([^']+)'\)`)

// Note in place of a skill's loaded text telling the model how to load it again
func SkillPrunedMarker(name string) string {
	return SkillMarkerStart + " content lost in compression; reload with skill_view(name='" + name + "')]"
}

// Skills named by the reload notes of a text, in their first order
func PrunedSkills(text string) []string {
	var out []string
	for _, m := range skillMarker.FindAllStringSubmatch(text, -1) {
		if !slices.Contains(out, m[1]) {
			out = append(out, m[1])
		}
	}
	return out
}

// Notice put after the state carried across a compaction naming each skill it pruned, empty when it pruned none
func SkillReloadNotice(messages []*sessionsv1.Message) string {
	var names []string
	for _, m := range messages {
		for _, name := range PrunedSkills(Text(m)) {
			if !slices.Contains(names, name) {
				names = append(names, name)
			}
		}
	}
	if len(names) == 0 {
		return ""
	}
	calls := make([]string, 0, len(names))
	for _, name := range names[:min(len(names), maxReloads)] {
		calls = append(calls, "skill_view(name='"+name+"')")
	}
	return SkillReloadHeader + "\n" +
		"The task list above crossed the compression boundary verbatim, but the skill instructions that governed it were pruned. Before " +
		"executing any preserved task that depends on these skills, reload them first: " + strings.Join(calls, "; ") + ". After reloading, " +
		"re-check that each pending task is still justified — findings recorded before the boundary may have invalidated it."
}

const MarkerPrefix = "⟪ICEBATH-CONTEXT-COMPRESSION:"

// A compression mark copied into the arguments of a call, which would write it somewhere
var MarkerArtifact = regexp.MustCompile(regexp.QuoteMeta(MarkerPrefix) + `\s+\d[\d,]*`)

// Mark of text cut from a message, naming how much of how much
func Marker(omitted, total int) string {
	return MarkerPrefix + " " + strs.Thousands(omitted) + " of " + strs.Thousands(total) + " chars omitted here by icebath's context compressor.⟫"
}

// Widest mark any count under a terabyte renders, in code points
var MarkerWidth = utf8.RuneCountInString(Marker(1e12-1, 1e12-1))

// Text cut to the limit in code points with the mark at its end, the mark alone when the limit can't hold it
func Elide(text string, limit int) string {
	total := utf8.RuneCountInString(text)
	if total <= limit {
		return text
	}
	head := limit - utf8.RuneCountInString(Marker(total, total))
	if head <= 0 {
		return Marker(total, total)
	}
	kept := strings.TrimRightFunc(string([]rune(text)[:head]), unicode.IsSpace)
	return kept + Marker(total-utf8.RuneCountInString(kept), total)
}

// Text keeping head code points from its start and tail from its end with the mark between them
func ElideMiddle(text string, head, tail int) string {
	runes := []rune(text)
	if len(runes) <= head+tail {
		return text
	}
	return string(runes[:head]) + Marker(len(runes)-head-tail, len(runes)) + string(runes[len(runes)-tail:])
}
