// Keeps a session's standing goal, judges each turn against it and decides whether the loop continues
package goal

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"
)

const (
	DefaultMaxTurns           = 20
	DefaultGateTimeoutSeconds = 300
	DefaultGateMaxRetries     = 3
	DefaultPauseReason        = "user-paused"

	StatusActive  = "active"
	StatusPaused  = "paused"
	StatusDone    = "done"
	StatusCleared = "cleared"
)

var contractLabels = [5]string{"Outcome", "Verification", "Constraints", "Boundaries", "Stop when blocked"}

var contractAliases = map[string]int{
	"outcome": 0, "goal": 0, "done": 0, "done when": 0,
	"verification": 1, "verify": 1, "verified by": 1, "evidence": 1, "proof": 1,
	"constraints": 2, "constraint": 2, "preserve": 2, "must not": 2, "do not change": 2,
	"boundaries": 3, "boundary": 3, "scope": 3, "allowed": 3, "files": 3,
	"stop when": 4, "stop_when": 4, "blocked": 4, "stop if blocked": 4, "give up when": 4,
}

var contractKeys = [5]string{"outcome", "verification", "constraints", "boundaries", "stop_when"}

type Contract struct {
	Outcome      string `json:"outcome"`
	Verification string `json:"verification"`
	Constraints  string `json:"constraints"`
	Boundaries   string `json:"boundaries"`
	StopWhen     string `json:"stop_when"`
}

func (c *Contract) slots() [5]*string {
	return [5]*string{&c.Outcome, &c.Verification, &c.Constraints, &c.Boundaries, &c.StopWhen}
}

func (c Contract) Empty() bool {
	for _, v := range c.slots() {
		if trimSpace(*v) != "" {
			return false
		}
	}
	return true
}

func (c Contract) Render() string {
	var lines []string
	for i, v := range c.slots() {
		if t := trimSpace(*v); t != "" {
			lines = append(lines, "- "+contractLabels[i]+": "+t)
		}
	}
	return strings.Join(lines, "\n")
}

func ContractFrom(v any) Contract {
	var c Contract
	m, ok := v.(map[string]any)
	if !ok {
		return c
	}
	for i, slot := range c.slots() {
		if v := m[contractKeys[i]]; truthy(v) {
			*slot = trimSpace(textOf(v))
		}
	}
	return c
}

func ParseContract(text string) (string, Contract) {
	var c Contract
	if text == "" {
		return "", c
	}
	var headline []string
	var parts [5][]string
	for _, raw := range strings.FieldsFunc(text, isLineBreak) {
		line := trimSpace(raw)
		if line == "" {
			continue
		}
		if prefix, value, found := strings.Cut(line, ":"); found {
			if idx, ok := contractAliases[strings.ToLower(trimSpace(prefix))]; ok && trimSpace(value) != "" {
				parts[idx] = append(parts[idx], trimSpace(value))
				continue
			}
		}
		headline = append(headline, line)
	}
	for i, slot := range c.slots() {
		*slot = trimSpace(strings.Join(parts[i], " "))
	}
	return trimSpace(strings.Join(headline, " ")), c
}

func renderExtraCriteria(subgoals []string) string {
	lines := make([]string, len(subgoals))
	for i, s := range subgoals {
		lines[i] = fmt.Sprintf("- Extra criterion %d: %s", i+1, s)
	}
	return strings.Join(lines, "\n")
}

type Gate struct {
	Command        string `json:"command"`
	TimeoutSeconds int    `json:"timeout_seconds"`
	MaxRetries     int    `json:"max_retries"`
	Attempts       int    `json:"attempts"`
	LastExitCode   *int   `json:"last_exit_code"`
	LastOutputTail string `json:"last_output_tail"`
}

func gateFrom(m map[string]any) (Gate, error) {
	g := Gate{Command: textOf(m["command"]), TimeoutSeconds: DefaultGateTimeoutSeconds, MaxRetries: DefaultGateMaxRetries}
	var err error
	if truthy(m["timeout_seconds"]) {
		if g.TimeoutSeconds, err = intOf(m["timeout_seconds"]); err != nil {
			return g, err
		}
	}
	if truthy(m["max_retries"]) {
		if g.MaxRetries, err = intOf(m["max_retries"]); err != nil {
			return g, err
		}
	}
	if truthy(m["attempts"]) {
		if g.Attempts, err = intOf(m["attempts"]); err != nil {
			return g, err
		}
	}
	if v, ok := m["last_exit_code"]; ok && v != nil {
		code, err := intOf(v)
		if err != nil {
			return g, err
		}
		g.LastExitCode = &code
	}
	if truthy(m["last_output_tail"]) {
		g.LastOutputTail = textOf(m["last_output_tail"])
	}
	return g, nil
}

type State struct {
	Goal                         string   `json:"goal"`
	Status                       string   `json:"status"`
	TurnsUsed                    int      `json:"turns_used"`
	MaxTurns                     int      `json:"max_turns"`
	CreatedAt                    float64  `json:"created_at"`
	LastTurnAt                   float64  `json:"last_turn_at"`
	LastVerdict                  *string  `json:"last_verdict"`
	LastReason                   *string  `json:"last_reason"`
	PausedReason                 *string  `json:"paused_reason"`
	ConsecutiveParseFailures     int      `json:"consecutive_parse_failures"`
	ConsecutiveTransportFailures int      `json:"consecutive_transport_failures"`
	Subgoals                     []string `json:"subgoals"`
	WaitingOnPID                 *int     `json:"waiting_on_pid"`
	WaitingOnSession             *string  `json:"waiting_on_session"`
	WaitingUntil                 float64  `json:"waiting_until"`
	WaitingOnDelegations         int      `json:"waiting_on_delegations"`
	WaitingReason                *string  `json:"waiting_reason"`
	WaitingSince                 float64  `json:"waiting_since"`
	Contract                     Contract `json:"contract"`
	Gates                        []Gate   `json:"gates"`
}

func (s State) HasContract() bool { return !s.Contract.Empty() }

func (s State) RenderSubgoals() string {
	lines := make([]string, len(s.Subgoals))
	for i, t := range s.Subgoals {
		lines[i] = fmt.Sprintf("- %d. %s", i+1, t)
	}
	return strings.Join(lines, "\n")
}

func (s *State) clearWait() {
	s.WaitingOnPID = nil
	s.WaitingOnSession = nil
	s.WaitingUntil = 0
	s.WaitingOnDelegations = 0
	s.WaitingReason = nil
	s.WaitingSince = 0
}

func (s State) clone() State {
	out := s
	out.LastVerdict = cloneText(s.LastVerdict)
	out.LastReason = cloneText(s.LastReason)
	out.PausedReason = cloneText(s.PausedReason)
	out.WaitingOnSession = cloneText(s.WaitingOnSession)
	out.WaitingReason = cloneText(s.WaitingReason)
	if s.WaitingOnPID != nil {
		pid := *s.WaitingOnPID
		out.WaitingOnPID = &pid
	}
	out.Subgoals = append([]string{}, s.Subgoals...)
	out.Gates = make([]Gate, len(s.Gates))
	for i, g := range s.Gates {
		out.Gates[i] = g
		if g.LastExitCode != nil {
			code := *g.LastExitCode
			out.Gates[i].LastExitCode = &code
		}
	}
	return out
}

func (s State) Encode() (string, error) {
	out := s
	if out.Subgoals == nil {
		out.Subgoals = []string{}
	}
	if out.Gates == nil {
		out.Gates = []Gate{}
	}
	var b bytes.Buffer
	enc := json.NewEncoder(&b)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(out); err != nil {
		return "", err
	}
	return strings.TrimSuffix(b.String(), "\n"), nil
}

func Decode(raw string) (State, error) {
	var data map[string]any
	if err := json.Unmarshal([]byte(raw), &data); err != nil {
		return State{}, err
	}
	if data == nil {
		return State{}, errStateNotObject
	}
	s := State{Status: StatusActive, MaxTurns: DefaultMaxTurns, Subgoals: []string{}, Gates: []Gate{}}
	if v, ok := data["goal"]; ok {
		s.Goal = textOf(v)
	}
	if v, ok := data["status"]; ok {
		s.Status = textOf(v)
	}
	var err error
	if truthy(data["max_turns"]) {
		if s.MaxTurns, err = intOf(data["max_turns"]); err != nil {
			return State{}, err
		}
	}
	for key, dst := range map[string]*int{"turns_used": &s.TurnsUsed, "consecutive_parse_failures": &s.ConsecutiveParseFailures,
		"consecutive_transport_failures": &s.ConsecutiveTransportFailures, "waiting_on_delegations": &s.WaitingOnDelegations} {
		if truthy(data[key]) {
			if *dst, err = intOf(data[key]); err != nil {
				return State{}, err
			}
		}
	}
	for key, dst := range map[string]*float64{"created_at": &s.CreatedAt, "last_turn_at": &s.LastTurnAt,
		"waiting_until": &s.WaitingUntil, "waiting_since": &s.WaitingSince} {
		if truthy(data[key]) {
			if *dst, err = floatOf(data[key]); err != nil {
				return State{}, err
			}
		}
	}
	s.LastVerdict = optionalText(data["last_verdict"])
	s.LastReason = optionalText(data["last_reason"])
	s.PausedReason = optionalText(data["paused_reason"])
	s.WaitingReason = optionalText(data["waiting_reason"])
	if list, ok := data["subgoals"].([]any); ok {
		for _, item := range list {
			if t := trimSpace(textOf(item)); t != "" {
				s.Subgoals = append(s.Subgoals, t)
			}
		}
	}
	if truthy(data["waiting_on_pid"]) {
		pid, err := intOf(data["waiting_on_pid"])
		if err != nil {
			return State{}, err
		}
		s.WaitingOnPID = &pid
	}
	if truthy(data["waiting_on_session"]) {
		id := textOf(data["waiting_on_session"])
		s.WaitingOnSession = &id
	}
	s.Contract = ContractFrom(data["contract"])
	if list, ok := data["gates"].([]any); ok {
		for _, item := range list {
			m, ok := item.(map[string]any)
			if !ok || trimSpace(textOf(m["command"])) == "" {
				continue
			}
			g, err := gateFrom(m)
			if err != nil {
				return State{}, err
			}
			s.Gates = append(s.Gates, g)
		}
	}
	return s, nil
}

func Restate(raw string) (status, encoded, cleared string, err error) {
	s, err := Decode(raw)
	if err != nil {
		return "", "", "", err
	}
	if encoded, err = s.Encode(); err != nil {
		return "", "", "", err
	}
	status = s.Status
	s.Status = StatusCleared
	if cleared, err = s.Encode(); err != nil {
		return "", "", "", err
	}
	return status, encoded, cleared, nil
}

var (
	errStateNotObject = errors.New("stored goal state is not a JSON object")
	errNotInteger     = errors.New("value is not an integer")
	errNotNumber      = errors.New("value is not a number")
)

func truthy(v any) bool {
	switch t := v.(type) {
	case nil:
		return false
	case bool:
		return t
	case float64:
		return t != 0
	case string:
		return t != ""
	case []any:
		return len(t) > 0
	case map[string]any:
		return len(t) > 0
	}
	return true
}

func intOf(v any) (int, error) {
	switch t := v.(type) {
	case bool:
		if t {
			return 1, nil
		}
		return 0, nil
	case float64:
		if math.IsNaN(t) || math.IsInf(t, 0) {
			return 0, errNotInteger
		}
		return int(t), nil
	case string:
		n, err := strconv.Atoi(strings.ReplaceAll(trimSpace(t), "_", ""))
		if err != nil {
			return 0, fmt.Errorf("%w: %q", errNotInteger, t)
		}
		return n, nil
	}
	return 0, errNotInteger
}

func floatOf(v any) (float64, error) {
	switch t := v.(type) {
	case bool:
		if t {
			return 1, nil
		}
		return 0, nil
	case float64:
		return t, nil
	case string:
		f, err := strconv.ParseFloat(trimSpace(t), 64)
		if err != nil {
			return 0, fmt.Errorf("%w: %q", errNotNumber, t)
		}
		return f, nil
	}
	return 0, errNotNumber
}

func textOf(v any) string {
	switch t := v.(type) {
	case nil:
		return ""
	case string:
		return t
	case bool:
		if t {
			return "True"
		}
		return "False"
	case float64:
		if t == math.Trunc(t) && math.Abs(t) < 1e16 {
			return strconv.FormatInt(int64(t), 10)
		}
		return strconv.FormatFloat(t, 'g', -1, 64)
	}
	b, err := json.Marshal(v)
	if err != nil {
		return ""
	}
	return string(b)
}

func optionalText(v any) *string {
	if v == nil {
		return nil
	}
	t := textOf(v)
	return &t
}

func cloneText(p *string) *string {
	if p == nil {
		return nil
	}
	v := *p
	return &v
}

func text(p *string) string {
	if p == nil {
		return ""
	}
	return *p
}

func ptr[T any](v T) *T { return &v }

func now() float64 { return float64(time.Now().UnixNano()) / 1e9 }

func isSpace(r rune) bool { return unicode.IsSpace(r) || (r >= 0x1c && r <= 0x1f) }

func isLineBreak(r rune) bool {
	switch r {
	case '\n', '\r', '\v', '\f', 0x1c, 0x1d, 0x1e, 0x85, 0x2028, 0x2029:
		return true
	}
	return false
}

func trimSpace(s string) string { return strings.TrimFunc(s, isSpace) }

func truncate(s string, limit int) string {
	if s == "" {
		return ""
	}
	if utf8.RuneCountInString(s) <= limit {
		return s
	}
	return string([]rune(s)[:limit]) + "… [truncated]"
}

func tail(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[len(r)-n:])
}
