package builtin

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"strconv"
	"strings"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/nickheyer/icebath/internal/config"
	"github.com/nickheyer/icebath/internal/tool"
	"github.com/nickheyer/icebath/internal/tool/redact"
)

const (
	maxChoices         = 4
	maxQuestions       = 5
	maxChoiceChars     = 8000
	RecommendedLabel   = "(Recommended)"
	clarifyUnavailable = "Clarify tool is not available in this execution context."
	clarifyShape       = "Pass questions=[{question, choices?, multi_select?}]; a single question is a one-entry array."

	FiniteClarifyNotice = "single-query mode: no user available to answer. Pick the best choices using your own " +
		"judgment, or make the most reasonable assumption you can, and continue."
)

const clarifyDescription = "Ask the user one or more questions when you need a decision or feedback. " +
	"Pass every question " +
	"in `questions` (1-5 entries) — a single question is a " +
	"one-entry array, and several INDEPENDENT questions belong in ONE " +
	"call (one form beats a chain of clarify calls; if one answer would " +
	"change another question, ask separately). Per question: " +
	"single-select (up to 4 choices — put your recommended " +
	"option FIRST, the UI marks it '(Recommended)' and auto-appends an " +
	"'Other' free-text row), multi-select (multi_select=true), or " +
	"open-ended (omit choices). Options go ONLY in `choices`, never " +
	"enumerated inside the question text (choices render as pickable " +
	"rows; options written into the question are dead prose the user " +
	"can't click). Result: {responses: [...], outcome} in question order. " +
	"Each response has status answered, skipped or unanswered " +
	"(user_response is null unless answered); outcome is submitted, " +
	"cancelled, timed_out or undelivered, with a notice saying why " +
	"when the wait ended without a submit. Prefer deciding " +
	"low-stakes questions yourself; don't use this for dangerous-command " +
	"confirmation (the terminal tool handles that)."

const clarifyParameters = `{"type": "object", "properties": {"questions": {"type": "array", "minItems": 1, "maxItems": 5, ` +
	`"description": "The question(s). Each: question text (options excluded), optional choices (recommended first; omit for free-text), ` +
	`optional multi_select. Responses come back in question order with the question text echoed.", ` +
	`"items": {"type": "object", "properties": {"question": {"type": "string"}, "choices": {"type": "array", "items": {"type": "string"}, "maxItems": 4}, ` +
	`"multi_select": {"type": "boolean"}}, "required": ["question"]}}}, "required": ["questions"]}`

type HumanInput interface {
	HumanInput(ctx context.Context, c *tool.Call, kind, prompt, sessionKey string, wait func() string)
}

type ClarifyOptions struct {
	Clarifier *Clarifier
	Redactor  *redact.Redactor
	Human     HumanInput
	Config    *config.Store
}

type clarifyTool struct {
	o ClarifyOptions
}

func Clarify(o ClarifyOptions) (tool.Tool, error) {
	switch {
	case o.Clarifier == nil:
		return tool.Tool{}, &tool.UnfilledError{Name: "clarify pending registry"}
	case o.Redactor == nil:
		return tool.Tool{}, &tool.UnfilledError{Name: "clarify display redactor"}
	case o.Human == nil:
		return tool.Tool{}, &tool.UnfilledError{Name: "human input hooks"}
	case o.Config == nil:
		return tool.Tool{}, &tool.UnfilledError{Name: "config store"}
	}
	t := &clarifyTool{o: o}
	return tool.Tool{
		Name:        "clarify",
		Toolset:     "clarify",
		Description: clarifyDescription,
		Parameters:  json.RawMessage(clarifyParameters),
		Handler:     t.handle,
		Emoji:       "❓",
		Concurrency: tool.Never,
	}, nil
}

func MarkRecommended(choices []string) []string {
	first := ""
	if len(choices) > 0 {
		first = strip(choices[0])
	}
	if len(choices) < 2 || first != StripRecommended(first) {
		return choices
	}
	return append([]string{first + " " + RecommendedLabel}, choices[1:]...)
}

func StripRecommended(text string) string {
	stripped := strip(text)
	runes := []rune(stripped)
	label := []rune(RecommendedLabel)
	if len(runes) >= len(label) && strings.EqualFold(string(runes[len(runes)-len(label):]), RecommendedLabel) {
		return strip(string(runes[:len(runes)-len(label)]))
	}
	return stripped
}

type clarifyEntry struct {
	qid     string
	text    string
	choices []string
	offered []string
	multi   bool
}

func normalizeQuestions(raw any) ([]clarifyEntry, string) {
	items, ok := raw.([]any)
	if !ok || len(items) == 0 {
		return nil, "questions must be a non-empty array. " + clarifyShape
	}
	if len(items) > maxQuestions {
		return nil, fmt.Sprintf("questions supports at most %d items.", maxQuestions)
	}
	out := make([]clarifyEntry, 0, len(items))
	for index, item := range items {
		obj, ok := item.(map[string]any)
		if !ok {
			return nil, fmt.Sprintf("questions[%d] must be an object. %s", index, clarifyShape)
		}
		text := ""
		if q := obj["question"]; truthy(q) {
			text = strip(pyStr(q))
		}
		if text == "" {
			return nil, fmt.Sprintf("questions[%d].question must be non-empty text.", index)
		}
		var cleaned []string
		if rawChoices, present := obj["choices"]; present && rawChoices != nil {
			list, ok := rawChoices.([]any)
			if !ok {
				return nil, fmt.Sprintf("questions[%d].choices must be a list of strings.", index)
			}
			choices := make([]string, len(list))
			for pos, v := range list {
				s, ok := v.(string)
				if !ok {
					return nil, fmt.Sprintf("questions[%d].choices must be a list of strings.", index)
				}
				choices[pos] = s
			}
			for pos, choice := range choices {
				if n := runeLen(strip(choice)); n > maxChoiceChars {
					return nil, fmt.Sprintf("questions[%d].choices[%d] is %d characters; the limit is %d. "+
						"Move detail into the question text and resend.", index, pos, n, maxChoiceChars)
				}
			}
			for _, choice := range choices {
				if s := strip(choice); s != "" {
					cleaned = append(cleaned, s)
				}
			}
			if len(cleaned) > maxChoices {
				cleaned = cleaned[:maxChoices]
			}
			if len(choices) > 0 && len(cleaned) == 0 {
				return nil, fmt.Sprintf("questions[%d].choices has %d entries but all are blank. "+
					"Send real labels, or omit choices to ask open-ended.", index, len(choices))
			}
		}
		e := clarifyEntry{qid: fmt.Sprintf("q%d", index), text: text, multi: truthy(obj["multi_select"]) && len(cleaned) > 0}
		if len(cleaned) > 0 {
			e.offered = cleaned
			e.choices = MarkRecommended(append([]string(nil), cleaned...))
		}
		out = append(out, e)
	}
	return out, ""
}

func (t *clarifyTool) prose(ctx context.Context, texts []string) []string {
	if len(texts) == 0 {
		return texts
	}
	out := make([]string, 0, len(texts))
	for _, raw := range texts {
		text := t.o.Redactor.Approval(ctx, raw, false, true)
		label, n := text, 1
		for contains(out, label) {
			n++
			label = fmt.Sprintf("%s (%d)", text, n)
		}
		out = append(out, label)
	}
	return out
}

func (t *clarifyTool) shown(ctx context.Context, entries []clarifyEntry) []clarifyEntry {
	shown := make([]clarifyEntry, len(entries))
	for i, e := range entries {
		s := e
		s.text = t.prose(ctx, []string{e.text})[0]
		s.offered = t.prose(ctx, e.offered)
		s.choices = nil
		if len(s.offered) > 0 {
			s.choices = MarkRecommended(append([]string(nil), s.offered...))
		}
		shown[i] = s
	}
	return shown
}

func (t *clarifyTool) handle(ctx context.Context, c *tool.Call, args tool.Args) (tool.Result, error) {
	entries, problem := normalizeQuestions(args["questions"])
	if problem != "" {
		return tool.Failure(problem), nil
	}
	if c.Surface == nil {
		return tool.Failure(clarifyUnavailable), nil
	}
	shown := t.shown(ctx, entries)
	texts := make([]string, len(shown))
	for i, s := range shown {
		texts[i] = s.text
	}
	var res tool.Result
	t.o.Human.HumanInput(ctx, c, "clarify", strings.Join(texts, "\n"), c.SessionKey, func() string {
		reply, err := t.ask(ctx, c, shown)
		switch {
		case err != nil:
			res = tool.Failure("Failed to get user input: " + err.Error())
			return "error"
		case reply.Outcome == "":
			res = tool.Failure("Failed to get user input: 'outcome'")
			return "error"
		}
		res = clarifyResult(entries, reply, shown)
		return reply.Outcome
	})
	return res, nil
}

func questionsOf(shown []clarifyEntry) []tool.ClarifyQuestion {
	out := make([]tool.ClarifyQuestion, len(shown))
	for i, s := range shown {
		out[i] = tool.ClarifyQuestion{ID: s.qid, Text: s.text, Choices: s.choices, Offered: s.offered, MultiSelect: s.multi}
	}
	return out
}

func (t *clarifyTool) ask(ctx context.Context, c *tool.Call, shown []clarifyEntry) (tool.ClarifyReply, error) {
	if c.Presence.SingleQuery {
		return FiniteClarifyReply(), nil
	}
	timeout := ClarifyTimeout(ctx, t.o.Config)
	reply, ok, err := c.Surface.PromptClarify(ctx, tool.ClarifyPrompt{ID: newClarifyID(), SessionKey: c.SessionKey,
		Questions: questionsOf(shown), Timeout: waitDuration(timeout)})
	if err != nil || ok {
		return reply, err
	}
	answers := map[string]tool.ClarifyAnswer{}
	out := tool.ClarifyReply{Answers: answers, Outcome: "submitted"}
	questions := questionsOf(shown)
	for index, q := range questions {
		raw, answered := t.card(ctx, c, q, timeout, index == len(questions)-1)
		if raw == Cancelled {
			out.Outcome = "cancelled"
			break
		}
		if !answered {
			out.Outcome = "timed_out"
			if raw == Undelivered || raw == UndeliveredDeclined || raw == UndeliveredNoSurface {
				out.Outcome = "undelivered"
			}
			out.Notice = raw
			break
		}
		if raw == Skipped {
			answers[q.ID] = tool.ClarifyAnswer{Skipped: true}
		} else {
			answers[q.ID] = tool.ClarifyAnswer{Text: raw}
		}
	}
	return out, nil
}

func (t *clarifyTool) card(ctx context.Context, c *tool.Call, q tool.ClarifyQuestion, timeout int, last bool) (string, bool) {
	key := c.SessionKey
	id := newClarifyID()
	t.o.Clarifier.Register(id, key, q.Text, q.Choices, q.MultiSelect)
	err := c.Surface.NotifyClarify(ctx, tool.ClarifyPrompt{ID: id, SessionKey: key, Questions: []tool.ClarifyQuestion{q},
		Timeout: waitDuration(timeout)})
	var response string
	answered := false
	switch {
	case errors.Is(err, ErrClarifyNoSurface):
		t.o.Clarifier.drop(id)
		return UndeliveredNoSurface, false
	case errors.Is(err, ErrClarifyDeclined):
		slog.WarnContext(ctx, "Clarify prompt DECLINED by the connector's egress guard; clearing registration")
		t.o.Clarifier.ClearSession(key)
		response = UndeliveredDeclined
	case err != nil:
		slog.WarnContext(ctx, "Clarify send failed definitively; clearing registration", "error", err)
		t.o.Clarifier.ClearSession(key)
		response = Undelivered
	default:
		got, ok, notice := t.o.Clarifier.await(ctx, c, id, timeout)
		switch {
		case notice != "":
			response = notice
		case got == Cancelled:
			response = got
		case !ok || got == "":
			response = fmt.Sprintf("[user did not respond within %dm]", int(float64(timeout)/60))
		default:
			response, answered = got, true
		}
	}
	c.Surface.SettleClarify(ctx, id, answered, last)
	return response, answered
}

func (k *Clarifier) drop(id string) {
	k.mu.Lock()
	defer k.mu.Unlock()
	p, ok := k.entries[id]
	if !ok {
		return
	}
	delete(k.entries, id)
	ids := k.sessions[p.SessionKey]
	for i, v := range ids {
		if v == id {
			ids = append(ids[:i:i], ids[i+1:]...)
			break
		}
	}
	if len(ids) == 0 {
		delete(k.sessions, p.SessionKey)
	} else {
		k.sessions[p.SessionKey] = ids
	}
	p.finish()
}

func waitDuration(timeout int) time.Duration {
	if timeout <= 0 {
		return 0
	}
	return time.Duration(timeout) * time.Second
}

type clarifyResponse struct {
	Question       string   `json:"question"`
	ChoicesOffered []string `json:"choices_offered"`
	Status         string   `json:"status"`
	UserResponse   any      `json:"user_response"`
}

type clarifyOutput struct {
	Responses []clarifyResponse `json:"responses"`
	Outcome   string            `json:"outcome"`
	Notice    string            `json:"notice,omitempty"`
}

func cleanAnswer(a tool.ClarifyAnswer, multi bool) any {
	if a.List == nil && !multi {
		return StripRecommended(a.Text)
	}
	items := a.List
	if items == nil {
		var parsed []any
		if err := json.Unmarshal([]byte(a.Text), &parsed); err == nil {
			for _, v := range parsed {
				items = append(items, pyStr(v))
			}
			if items == nil {
				items = []string{}
			}
		} else {
			items = []string{a.Text}
		}
	}
	out := []string{}
	for _, item := range items {
		if strip(item) != "" {
			out = append(out, StripRecommended(item))
		}
	}
	return out
}

func responseStatus(qid string, answers map[string]tool.ClarifyAnswer, multi bool) (string, any) {
	a, present := answers[qid]
	if present && !a.Skipped && (a.List != nil || a.Text != "") {
		switch v := cleanAnswer(a, multi).(type) {
		case string:
			if v != "" {
				return "answered", v
			}
		case []string:
			if len(v) > 0 {
				return "answered", v
			}
		}
	}
	if present {
		return "skipped", nil
	}
	return "unanswered", nil
}

func clarifyResult(entries []clarifyEntry, reply tool.ClarifyReply, shown []clarifyEntry) tool.Result {
	out := clarifyOutput{Responses: []clarifyResponse{}, Outcome: reply.Outcome, Notice: reply.Notice}
	for i, e := range entries {
		seen := e
		if i < len(shown) {
			seen = shown[i]
		}
		status, value := responseStatus(e.qid, reply.Answers, e.multi)
		back := map[string]string{}
		for j := 0; j < len(seen.offered) && j < len(e.offered); j++ {
			back[seen.offered[j]] = e.offered[j]
		}
		if len(back) > 0 {
			switch v := value.(type) {
			case string:
				if raw, ok := back[v]; ok {
					value = raw
				}
			case []string:
				mapped := make([]string, len(v))
				for j, s := range v {
					mapped[j] = s
					if raw, ok := back[s]; ok {
						mapped[j] = raw
					}
				}
				value = mapped
			}
		}
		out.Responses = append(out.Responses, clarifyResponse{Question: e.text, ChoicesOffered: e.offered, Status: status, UserResponse: value})
	}
	return tool.JSON(out)
}

func FiniteClarifyReply() tool.ClarifyReply {
	return tool.ClarifyReply{Answers: map[string]tool.ClarifyAnswer{}, Outcome: "undelivered", Notice: FiniteClarifyNotice}
}

func SettleBatch(locked map[string]tool.ClarifyAnswer, frame *tool.ClarifyReply, ended string) tool.ClarifyReply {
	known := func(o string) bool { return o == "submitted" || o == "timed_out" || o == "cancelled" }
	answers := make(map[string]tool.ClarifyAnswer, len(locked))
	for k, v := range locked {
		answers[k] = v
	}
	if ended != "" {
		if !known(ended) {
			ended = "cancelled"
		}
		return tool.ClarifyReply{Answers: answers, Outcome: ended}
	}
	if frame != nil && frame.Answers != nil {
		for k, v := range frame.Answers {
			answers[k] = v
		}
		outcome := frame.Outcome
		if !known(outcome) {
			outcome = "submitted"
		}
		return tool.ClarifyReply{Answers: answers, Outcome: outcome, Notice: frame.Notice}
	}
	return tool.ClarifyReply{Answers: answers, Outcome: "cancelled"}
}

func ClarifyTimeout(ctx context.Context, st *config.Store) int {
	cfg, err := st.Load(ctx)
	if err != nil {
		return 3600
	}
	if n := cfg.UnknownRoot("clarify"); n != nil {
		switch n.Kind {
		case yaml.MappingNode:
			for i := 0; i+1 < len(n.Content); i += 2 {
				if n.Content[i].Value == "timeout" {
					if v := n.Content[i+1]; v.ShortTag() != "!!null" {
						return yamlInt(v)
					}
					break
				}
			}
		case yaml.SequenceNode:
			if len(n.Content) > 0 {
				return 3600
			}
		case yaml.ScalarNode:
			if yamlTruthy(n) {
				return 3600
			}
		default:
			return 3600
		}
	}
	return cfg.Agent.ClarifyTimeout
}

func yamlInt(v *yaml.Node) int {
	if v.Kind != yaml.ScalarNode {
		return 3600
	}
	switch v.ShortTag() {
	case "!!int":
		var n int64
		if err := v.Decode(&n); err == nil {
			return int(n)
		}
	case "!!float":
		var f float64
		if err := v.Decode(&f); err == nil && !math.IsNaN(f) && !math.IsInf(f, 0) {
			return int(f)
		}
	case "!!bool":
		var b bool
		if err := v.Decode(&b); err == nil && b {
			return 1
		}
		return 0
	case "!!str":
		s := strings.ReplaceAll(strip(v.Value), "_", "")
		if isInt(strip(v.Value)) {
			if n, err := strconv.Atoi(s); err == nil {
				return n
			}
		}
	}
	return 3600
}

func yamlTruthy(n *yaml.Node) bool {
	switch n.ShortTag() {
	case "!!null":
		return false
	case "!!bool":
		var b bool
		return n.Decode(&b) == nil && b
	case "!!int", "!!float":
		var f float64
		return n.Decode(&f) == nil && f != 0
	}
	return n.Value != ""
}
