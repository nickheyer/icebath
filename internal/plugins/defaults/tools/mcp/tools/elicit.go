package tools

import (
	"cmp"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"maps"
	"math"
	"net"
	"net/mail"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/dlclark/regexp2/v2"
	"golang.org/x/net/idna"
	"golang.org/x/net/publicsuffix"

	"github.com/nickheyer/icebath/internal/hooks"
	"github.com/nickheyer/icebath/internal/mcp"
	"github.com/nickheyer/icebath/internal/tools"
	"github.com/nickheyer/icebath/pkg/httpx"
	"github.com/nickheyer/icebath/pkg/strs"
)

const (
	// Fields of a form asked at once, and times one may be asked again after an answer its schema refused
	formBatch    = 5
	formAttempts = 3
)

// Answers a server asking the person for input: form fields as questions to the session whose call runs, a link to open as a question
// to accept it, nothing at all where no one can be asked
type elicitor struct {
	server *server
	config elicitConfig

	mu      sync.Mutex
	pending map[string]chan struct{}
	metrics map[string]int
}

func (e *elicitor) Capabilities() map[string]any {
	return map[string]any{"form": map[string]any{}, "url": map[string]any{}}
}

// Requests, accepted, declined and errors, for the server's status
func (e *elicitor) Metrics() map[string]int {
	e.mu.Lock()
	defer e.mu.Unlock()
	return maps.Clone(e.metrics)
}

func (e *elicitor) count(metric string) {
	e.mu.Lock()
	e.metrics[metric]++
	e.mu.Unlock()
}

func (e *elicitor) Elicit(ctx context.Context, params *mcp.ElicitParams) (*mcp.ElicitResult, *mcp.RPCError) {
	e.count("requests")
	name := e.server.name
	switch params.Mode {
	case "url":
		return e.link(ctx, params)
	case "", "form":
	default:
		e.count("errors")
		return nil, &mcp.RPCError{Code: mcp.CodeInvalidParams, Message: fmt.Sprintf("Invalid params: elicitation mode %q is neither form nor url", params.Mode)}
	}
	message := cmp.Or(params.Message, fmt.Sprintf("The MCP server %s asks for your approval.", name))
	slog.Info("MCP server elicitation request", "server", name, "message", strs.Head(mcp.Redact(message), 200))
	if props, _ := params.RequestedSchema["properties"].(map[string]any); len(props) > 0 {
		fields, failure := formFields(params)
		if failure != nil {
			e.count("errors")
			return nil, failure
		}
		return e.form(ctx, message, fields)
	}
	answer := e.ask(ctx, message, summary(params, name))
	if answer == "accept" {
		return &mcp.ElicitResult{Action: answer, Content: map[string]any{}}, nil
	}
	return &mcp.ElicitResult{Action: answer}, nil
}

// What a consent without fields asks about, the schema's properties listed
func summary(params *mcp.ElicitParams, server string) string {
	props, _ := params.RequestedSchema["properties"].(map[string]any)
	if len(props) == 0 {
		return fmt.Sprintf("The MCP server %s asks for your approval.", server)
	}
	var lines []string
	for _, name := range order(props, params.Order) {
		lines = append(lines, "  "+name)
	}
	return fmt.Sprintf("The MCP server %s asks for:\n%s", server, strings.Join(lines, "\n"))
}

// Property names in the order the server wrote them, any it left out of that order last by name
func order(props map[string]any, written []string) []string {
	out := slices.DeleteFunc(slices.Clone(written), func(n string) bool { _, ok := props[n]; return !ok })
	rest := slices.DeleteFunc(slices.Sorted(maps.Keys(props)), func(n string) bool { return slices.Contains(out, n) })
	return append(out, rest...)
}

// Asks the person of the call running, accept or decline; decline where no one can be asked, cancel when they dismissed it or time ran out
func (e *elicitor) ask(ctx context.Context, message, detail string) string {
	base, env := e.server.callContext(ctx)
	if env == nil || env.Clarify == nil {
		slog.Info("MCP server elicitation declined: no one can answer in this session", "server", e.server.name)
		e.count("declined")
		return "decline"
	}
	askCtx, cancel := context.WithTimeout(base, e.config.Timeout)
	defer cancel()
	// The approval hooks hear of the consent asked and of the answer, as of any approval
	points, turn := e.server.pool.m.helpers.Hooks, hooks.TurnOf(base)
	asked := hooks.Approval{Command: message, Description: detail, Key: "mcp_elicitation", Keys: []string{"mcp_elicitation"}, SessionKey: turn.Session,
		Surface: "mcp-elicitation", Session: turn.Session, Turn: turn.ID, Call: hooks.CallOf(base)}
	points.Fire(base, env.Config, asked)
	reply, err := env.Clarify(askCtx, []tools.Question{{ID: "consent", Text: message + "\n\n" + detail, Choices: []string{"Accept", "Decline"}}})
	answer, choice := "decline", "deny"
	switch {
	case err != nil:
		slog.Error("MCP server elicitation failed", "server", e.server.name, "error", err)
		e.count("errors")
		choice = "cancelled"
	case reply.Outcome == tools.TimedOut:
		e.count("errors")
		answer, choice = "cancel", "timeout"
	case reply.Outcome == tools.Cancelled:
		e.count("errors")
		answer, choice = "cancel", "cancelled"
	case reply.Outcome == tools.Submitted && len(reply.Answers["consent"]) > 0 && reply.Answers["consent"][0] == "Accept":
		e.count("accepted")
		answer, choice = "accept", "once"
	default:
		e.count("declined")
	}
	points.Fire(context.WithoutCancel(base), env.Config, hooks.Approved{Approval: asked, Choice: choice})
	return answer
}

// One field of a form a server asks for
type formField struct {
	name     string
	spec     map[string]any
	required bool
	choices  []string
	consts   []any
	multi    bool
	pattern  *regexp2.Regexp
}

// Fields of a flat requested schema in the server's order, an invalid params error for a schema outside the primitive subset
func formFields(params *mcp.ElicitParams) ([]*formField, *mcp.RPCError) {
	props, _ := params.RequestedSchema["properties"].(map[string]any)
	required := map[string]bool{}
	if list, ok := params.RequestedSchema["required"].([]any); ok {
		for _, r := range list {
			if name, ok := r.(string); ok {
				required[name] = true
			}
		}
	}
	invalid := func(format string, args ...any) *mcp.RPCError {
		return &mcp.RPCError{Code: mcp.CodeInvalidParams, Message: "Invalid params: " + fmt.Sprintf(format, args...)}
	}
	var fields []*formField
	for _, name := range order(props, params.Order) {
		spec, ok := props[name].(map[string]any)
		if !ok {
			return nil, invalid("property %q of requestedSchema is not an object", name)
		}
		f := &formField{name: name, spec: spec, required: required[name]}
		switch kind, _ := spec["type"].(string); kind {
		case "string", "number", "integer":
			if enum, ok := spec["enum"].([]any); ok {
				for _, v := range enum {
					f.choices, f.consts = append(f.choices, fmt.Sprint(v)), append(f.consts, v)
				}
			} else if one, ok := spec["oneOf"].([]any); ok {
				f.choices, f.consts = titled(one)
			}
		case "boolean":
			f.choices, f.consts = []string{"yes", "no"}, []any{true, false}
		case "array":
			items, _ := spec["items"].(map[string]any)
			if enum, ok := items["enum"].([]any); ok {
				for _, v := range enum {
					f.choices, f.consts = append(f.choices, fmt.Sprint(v)), append(f.consts, v)
				}
			} else if options, ok := items["anyOf"].([]any); ok {
				f.choices, f.consts = titled(options)
			}
			if len(f.choices) == 0 {
				return nil, invalid("array property %q of requestedSchema offers no enum items", name)
			}
			f.multi = true
		default:
			return nil, invalid("property %q of requestedSchema has type %q outside string, number, integer, boolean and enum array", name, kind)
		}
		if pattern, ok := spec["pattern"].(string); ok && pattern != "" {
			re, err := regexp2.Compile(pattern, regexp2.ECMAScript)
			if err != nil {
				return nil, invalid("pattern of property %q does not compile: %v", name, err)
			}
			f.pattern = re
		}
		fields = append(fields, f)
	}
	return fields, nil
}

// Titles and constants of a titled enum
func titled(options []any) ([]string, []any) {
	var titles []string
	var consts []any
	for _, o := range options {
		entry, _ := o.(map[string]any)
		if entry == nil {
			continue
		}
		title, _ := entry["title"].(string)
		titles, consts = append(titles, cmp.Or(title, fmt.Sprint(entry["const"]))), append(consts, entry["const"])
	}
	return titles, consts
}

// Question of a field: its label, whether it is required, its description, limits and default, and why the last answer was refused
func (f *formField) question(problem string) string {
	label := f.name
	if title, _ := f.spec["title"].(string); title != "" {
		label = title + ", the field " + f.name
	}
	if f.required {
		label += ", which is needed"
	}
	parts := []string{label + "."}
	if description, _ := f.spec["description"].(string); description != "" {
		parts = append(parts, strs.Full(description))
	}
	var limits []string
	for _, rule := range []struct{ key, words string }{{"minLength", "at least %v characters"}, {"maxLength", "at most %v characters"},
		{"minimum", "at least %v"}, {"maximum", "at most %v"}, {"minItems", "at least %v choices"}, {"maxItems", "at most %v choices"},
		{"format", "in the format %v"}} {
		if v, ok := f.spec[rule.key]; ok {
			limits = append(limits, fmt.Sprintf(rule.words, v))
		}
	}
	if pattern, _ := f.spec["pattern"].(string); pattern != "" {
		limits = append(limits, "matching "+pattern)
	}
	switch f.spec["type"] {
	case "integer":
		limits = append([]string{"a whole number"}, limits...)
	case "number":
		limits = append([]string{"a number"}, limits...)
	}
	if len(limits) > 0 {
		parts = append(parts, "It must be "+strings.Join(limits, ", ")+".")
	}
	if def, ok := f.spec["default"]; ok {
		parts = append(parts, fmt.Sprintf("Skip it to use %v.", def))
	}
	if problem != "" {
		parts = append(parts, "Your last answer was refused, because "+problem+".")
	}
	return strings.Join(parts, " ")
}

// Value of an answer checked against the field's schema, or why it does not fit
func (f *formField) value(answer []string) (any, string) {
	if f.multi {
		picked := answer
		if len(picked) == 1 && strings.Contains(picked[0], ",") && !slices.Contains(f.choices, picked[0]) {
			picked = strings.Split(picked[0], ",")
		}
		out := []any{}
		for _, p := range picked {
			v, ok := f.constOf(p)
			if !ok {
				return nil, fmt.Sprintf("“%s” is not one of %s", strings.TrimSpace(p), strings.Join(f.choices, ", "))
			}
			out = append(out, v)
		}
		if n, ok := f.limit("minItems"); ok && len(out) < n {
			return nil, fmt.Sprintf("it needs at least %s", strs.Count(n, "choice"))
		}
		if n, ok := f.limit("maxItems"); ok && len(out) > n {
			return nil, fmt.Sprintf("it allows at most %s", strs.Count(n, "choice"))
		}
		return out, ""
	}
	text := strings.TrimSpace(strings.Join(answer, " "))
	if len(f.choices) > 0 {
		v, ok := f.constOf(text)
		if !ok {
			return nil, fmt.Sprintf("“%s” is not one of %s", text, strings.Join(f.choices, ", "))
		}
		return v, ""
	}
	switch f.spec["type"] {
	case "integer":
		n, err := strconv.ParseInt(text, 10, 64)
		if err != nil {
			return nil, fmt.Sprintf("“%s” is not a whole number", text)
		}
		return n, f.within(float64(n))
	case "number":
		n, err := strconv.ParseFloat(text, 64)
		if err != nil || math.IsInf(n, 0) || math.IsNaN(n) {
			return nil, fmt.Sprintf("“%s” is not a number", text)
		}
		return n, f.within(n)
	}
	length := utf8.RuneCountInString(text)
	if n, ok := f.limit("minLength"); ok && length < n {
		return nil, fmt.Sprintf("it needs at least %s", strs.Count(n, "character"))
	}
	if n, ok := f.limit("maxLength"); ok && length > n {
		return nil, fmt.Sprintf("it allows at most %s", strs.Count(n, "character"))
	}
	if f.pattern != nil {
		if matched, err := f.pattern.MatchString(text); err != nil || !matched {
			return nil, "it does not match the pattern " + fmt.Sprint(f.spec["pattern"])
		}
	}
	return text, format(fmt.Sprint(f.spec["format"]), text)
}

// Constant a shown choice stands for, in any casing
func (f *formField) constOf(choice string) (any, bool) {
	choice = strings.TrimSpace(choice)
	for i, c := range f.choices {
		if strings.EqualFold(choice, c) {
			return f.consts[i], true
		}
	}
	if f.spec["type"] == "boolean" {
		switch strings.ToLower(choice) {
		case "true", "y":
			return true, true
		case "false", "n":
			return false, true
		}
	}
	return nil, false
}

func (f *formField) limit(key string) (int, bool) {
	n, ok := seconds(f.spec[key])
	return int(n), ok
}

// Refusal of a number outside the field's minimum or maximum
func (f *formField) within(n float64) string {
	if v, ok := seconds(f.spec["minimum"]); ok && n < v {
		return "it must be at least " + strs.Number(v)
	}
	if v, ok := seconds(f.spec["maximum"]); ok && n > v {
		return "it must be at most " + strs.Number(v)
	}
	return ""
}

// Refusal of a text outside its email, uri, date or date-time format
func format(kind, text string) string {
	switch kind {
	case "email":
		if addr, err := mail.ParseAddress(text); err != nil || addr.Address != text {
			return fmt.Sprintf("“%s” is not an email address", text)
		}
	case "uri":
		if u, err := url.Parse(text); err != nil || u.Scheme == "" {
			return fmt.Sprintf("“%s” is not a full address", text)
		}
	case "date":
		if _, err := time.Parse("2006-01-02", text); err != nil {
			return fmt.Sprintf("“%s” is not a date such as 2026-01-31", text)
		}
	case "date-time":
		if _, err := time.Parse(time.RFC3339, text); err != nil {
			return fmt.Sprintf("“%s” is not a date and time such as 2026-01-31T09:30:00Z", text)
		}
	}
	return ""
}

// Asks every field of a form, a few at a time, a field refused asked again; then the answers shown for the person to send or not
func (e *elicitor) form(ctx context.Context, message string, fields []*formField) (*mcp.ElicitResult, *mcp.RPCError) {
	base, env := e.server.callContext(ctx)
	name := e.server.name
	if env == nil || env.Clarify == nil {
		slog.Info("MCP server form elicitation declined: no one can answer questions in this session", "server", name)
		e.count("declined")
		return &mcp.ElicitResult{Action: "decline"}, nil
	}
	content := map[string]any{}
	problems, attempts := map[string]string{}, map[string]int{}
	open := slices.Clone(fields)
	for len(open) > 0 {
		batch := open[:min(formBatch, len(open))]
		open = open[len(batch):]
		questions := make([]tools.Question, len(batch))
		for i, f := range batch {
			text := f.question(problems[f.name])
			if i == 0 {
				text = fmt.Sprintf("The MCP server %s asks: %s\n%s", name, message, text)
			}
			questions[i] = tools.Question{ID: f.name, Text: text, Choices: f.choices, MultiSelect: f.multi}
		}
		askCtx, cancel := context.WithTimeout(base, e.config.Timeout)
		reply, err := env.Clarify(askCtx, questions)
		cancel()
		if err != nil || reply.Outcome != tools.Submitted {
			slog.Info("MCP server form elicitation ended without answers", "server", name, "outcome", reply.Outcome)
			e.count("errors")
			return &mcp.ElicitResult{Action: "cancel"}, nil
		}
		for _, f := range batch {
			answer := reply.Answers[f.name]
			if len(answer) == 0 || strings.TrimSpace(strings.Join(answer, "")) == "" {
				if def, ok := f.spec["default"]; ok {
					content[f.name] = def
				} else if f.required {
					slog.Info("MCP server form elicitation declined: a required field was skipped", "server", name, "field", f.name)
					e.count("declined")
					return &mcp.ElicitResult{Action: "decline"}, nil
				}
				continue
			}
			v, problem := f.value(answer)
			if problem == "" {
				content[f.name] = v
				continue
			}
			if attempts[f.name]++; attempts[f.name] >= formAttempts {
				slog.Info("MCP server form elicitation declined: a field was refused too often", "server", name, "field", f.name)
				e.count("declined")
				return &mcp.ElicitResult{Action: "decline"}, nil
			}
			problems[f.name] = problem
			open = append(open, f)
		}
	}
	review := []string{fmt.Sprintf("Your answers for the MCP server %s:", name)}
	for _, f := range fields {
		if v, ok := content[f.name]; ok {
			shown, _ := json.Marshal(v)
			review = append(review, "  "+f.name+": "+string(shown))
		}
	}
	if answer := e.ask(ctx, "Send these answers to the server?", strings.Join(review, "\n")); answer != "accept" {
		return &mcp.ElicitResult{Action: answer}, nil
	}
	return &mcp.ElicitResult{Action: "accept", Content: content}, nil
}

// Asks to open the link of a URL mode elicitation, showing all of it, its domain and the warnings a spoofed or unsafe link earns; an
// accepted step is waited on until the server reports it finished
func (e *elicitor) link(ctx context.Context, params *mcp.ElicitParams) (*mcp.ElicitResult, *mcp.RPCError) {
	name := e.server.name
	u, err := url.Parse(strings.TrimSpace(params.URL))
	if err != nil || u.Scheme != "http" && u.Scheme != "https" || u.Hostname() == "" || u.Opaque != "" {
		e.count("errors")
		slog.Warn("MCP server sent a url mode elicitation with an unusable url", "server", name, "url", strs.Head(mcp.Redact(params.URL), 200))
		return nil, &mcp.RPCError{Code: mcp.CodeInvalidParams,
			Message: fmt.Sprintf("Invalid params: a url mode elicitation needs an absolute http or https url with a host, got %q", params.URL)}
	}
	message := cmp.Or(params.Message, fmt.Sprintf("The MCP server %s asks you to open a link.", name))
	slog.Info("MCP server url elicitation request", "server", name, "host", u.Hostname(), "message", strs.Head(mcp.Redact(message), 200))
	var done chan struct{}
	if params.ElicitationID != "" {
		done = make(chan struct{})
		e.mu.Lock()
		e.pending[params.ElicitationID] = done
		e.mu.Unlock()
	}
	answer := e.ask(ctx, message, linkText(u, name))
	if answer != "accept" && done != nil {
		e.mu.Lock()
		if e.pending[params.ElicitationID] == done {
			delete(e.pending, params.ElicitationID)
		}
		e.mu.Unlock()
	}
	return &mcp.ElicitResult{Action: answer}, nil
}

// Consent text of a link: all of it, its registrable domain, and warnings for international letters, a user name before the host or no
// encryption
func linkText(u *url.URL, server string) string {
	host := strings.ToLower(u.Hostname())
	ascii, err := idna.Lookup.ToASCII(host)
	if err != nil {
		ascii = host
	}
	domain := ascii
	if net.ParseIP(ascii) == nil {
		if registrable, err := publicsuffix.EffectiveTLDPlusOne(ascii); err == nil {
			domain = registrable
		}
	}
	lines := []string{fmt.Sprintf("The MCP server %s asks you to open this link:", server), "  " + u.String(), "Site: " + domain}
	if ascii != domain {
		lines[2] += ", at " + ascii
	}
	if strings.Contains(ascii, "xn--") || ascii != host {
		shown, err := idna.Display.ToUnicode(ascii)
		if err != nil {
			shown = host
		}
		lines = append(lines, fmt.Sprintf("⚠ The site name uses letters of other alphabets. It reads %s and is really %s, so check that it is the site you expect.", shown, ascii))
	}
	if u.User != nil {
		lines = append(lines, fmt.Sprintf("⚠ The link puts a user name before the site, so the site it really opens is %s.", ascii))
	}
	if u.Scheme == "http" && !httpx.Loopback(ascii) {
		lines = append(lines, "⚠ The link is not encrypted, as it starts with http://.")
	}
	return strings.Join(append(lines, "Accept, then finish the step on that page. What you enter there goes to the site and never through icebath."), "\n")
}

// The server reported a browser step finished
func (e *elicitor) Complete(id string) {
	e.mu.Lock()
	done, ok := e.pending[id]
	delete(e.pending, id)
	e.mu.Unlock()
	if !ok {
		slog.Debug("MCP server reported an unknown or finished elicitation; ignored", "server", e.server.name, "id", strs.Head(id, 80))
		return
	}
	close(done)
	slog.Info("MCP server reports a browser step of an elicitation finished", "server", e.server.name, "id", strs.Head(id, 80))
}

func (e *elicitor) Done(id string) <-chan struct{} {
	e.mu.Lock()
	defer e.mu.Unlock()
	if done, ok := e.pending[id]; ok {
		return done
	}
	return nil
}

func (e *elicitor) Wait() time.Duration { return e.config.Timeout }
