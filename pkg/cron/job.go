package cron

import (
	"bytes"
	"encoding/json"
	"fmt"
	"math"
	"sort"
	"strconv"
	"strings"

	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
)

// Schedule is the parsed schedule a job fires on
type Schedule struct {
	Kind    string
	Expr    string
	Minutes int
	// Minutes key present with a number, so zero minutes stays distinct from absent
	HasMinutes bool
	RunAt      string
	Display    string
	// Legacy or hand edited keys such as value
	Extra map[string]json.RawMessage
	// Text of a schedule that was not an object on disk
	Raw string
}

// Reports whether the schedule is a cron or interval schedule
func (s *Schedule) IsRecurring() bool {
	return s != nil && (s.Kind == "cron" || s.Kind == "interval")
}

// Reports whether the schedule is a one shot
func (s *Schedule) IsOnce() bool { return s != nil && s.Kind == "once" }

func (s Schedule) MarshalJSON() ([]byte, error) {
	if s.Raw != "" && s.Kind == "" {
		return json.Marshal(s.Raw)
	}
	var kv []kvPair
	kv = append(kv, kvPair{"kind", s.Kind})
	if s.Kind == "cron" || s.Expr != "" {
		kv = append(kv, kvPair{"expr", s.Expr})
	}
	if s.Kind == "interval" {
		kv = append(kv, kvPair{"minutes", s.Minutes})
	}
	if s.Kind == "once" {
		kv = append(kv, kvPair{"run_at", s.RunAt})
	}
	if s.Display != "" || s.Kind != "" {
		kv = append(kv, kvPair{"display", s.Display})
	}
	for _, k := range sortedRawKeys(s.Extra) {
		kv = append(kv, kvPair{k, s.Extra[k]})
	}
	if s.Kind == "" && len(kv) == 2 && s.Display == "" && len(s.Extra) == 0 {
		return []byte("{}"), nil
	}
	return encodeOrdered(kv)
}

func (s *Schedule) UnmarshalJSON(data []byte) error {
	trimmed := bytes.TrimSpace(data)
	*s = Schedule{}
	if len(trimmed) == 0 || string(trimmed) == "null" {
		return nil
	}
	if trimmed[0] != '{' {
		var text any
		if err := json.Unmarshal(trimmed, &text); err != nil {
			return err
		}
		s.Raw = fmt.Sprintf("%v", text)
		return nil
	}
	var m map[string]json.RawMessage
	if err := json.Unmarshal(trimmed, &m); err != nil {
		return err
	}
	for k, v := range m {
		switch k {
		case "kind":
			s.Kind, _ = decodeText(v)
		case "expr":
			s.Expr, _ = decodeText(v)
		case "minutes":
			if n, ok := decodeNumber(v); ok {
				s.Minutes = int(n)
				s.HasMinutes = true
			}
		case "run_at":
			s.RunAt, _ = decodeText(v)
		case "display":
			s.Display, _ = decodeText(v)
		default:
			if s.Extra == nil {
				s.Extra = map[string]json.RawMessage{}
			}
			s.Extra[k] = v
		}
	}
	return nil
}

// Repeat is the run budget of a job, nil times meaning forever
type Repeat struct {
	Times     *int `json:"times"`
	Completed int  `json:"completed"`
}

// Claim marks ownership of one fire or one shot run by a process
type Claim struct {
	At        string `json:"at,omitempty"`
	By        string `json:"by,omitempty"`
	Outage    bool   `json:"outage,omitempty"`
	ExtraSlot string `json:"extra_slot,omitempty"`
}

// PendingSlot is a recurring occurrence taken off the schedule but not yet claimed
type PendingSlot struct {
	ScheduledAt string `json:"scheduled_at"`
	At          string `json:"at"`
	By          string `json:"by"`
}

// Dispatch records the scheduled versus actual timing of the last recurring dispatch
type Dispatch struct {
	ScheduledAt     string  `json:"scheduled_at"`
	DispatchedAt    string  `json:"dispatched_at"`
	LatenessSeconds float64 `json:"lateness_seconds"`
	Kind            string  `json:"kind"`
}

// Stamp is a timestamped detail such as the last failure or a fire forward failure
type Stamp struct {
	At     string `json:"at"`
	Detail string `json:"detail"`
}

// MonitorState is the last observed monitor output hash
type MonitorState struct {
	LastOutputHash string `json:"last_output_hash,omitempty"`
	LastChangedAt  string `json:"last_changed_at,omitempty"`
}

// RetryState is the unreachable model re run ladder position
type RetryState struct {
	Attempt int     `json:"attempt"`
	At      string  `json:"at"`
	Expr    *string `json:"expr"`
}

// NullString is a key that may be present with a null value
type NullString struct {
	Valid bool
	S     string
}

func (n NullString) MarshalJSON() ([]byte, error) {
	if !n.Valid {
		return []byte("null"), nil
	}
	return json.Marshal(n.S)
}

func (n *NullString) UnmarshalJSON(data []byte) error {
	if string(bytes.TrimSpace(data)) == "null" {
		*n = NullString{}
		return nil
	}
	s, ok := decodeText(data)
	if !ok {
		*n = NullString{}
		return nil
	}
	*n = NullString{Valid: true, S: s}
	return nil
}

// Job is one stored cron job record
type Job struct {
	ID                     string
	Name                   string
	Prompt                 string
	Skills                 []string
	Skill                  *string
	Model                  *string
	Provider               *string
	BaseURL                *string
	Script                 *string
	NoAgent                bool
	MonitorScript          *string
	MonitorURL             *string
	MonitorState           *MonitorState
	ContextFrom            []string
	Schedule               *Schedule
	ScheduleDisplay        string
	Repeat                 *Repeat
	Enabled                bool
	State                  string
	PausedAt               *string
	PausedReason           *string
	CreatedAt              string
	NextRunAt              *string
	LastRunAt              *string
	LastStatus             *string
	LastError              *string
	LastDeliveryError      *string
	LastDeliveryUnverified []string
	LastFailure            *Stamp
	FailureStreak          int
	Deliver                *string
	Origin                 map[string]any
	EnabledToolsets        []string
	Workdir                *string
	// Keys persisted only when explicitly set
	AttachToSession      *bool
	ReasoningEffort      *string
	FailureDeliver       *string
	Interpreter          *string
	Timezone             *string
	GraceMinutes         *int
	CatchUp              *bool
	FireClaim            *Claim
	RunClaim             *Claim
	PendingSlot          *PendingSlot
	ManualRunAt          *string
	ManualRunRestoreAt   *NullString
	ManualRunPrompt      *string
	LastDispatch         *Dispatch
	LastFireError        *Stamp
	PreflightAlerted     bool
	QuotaHoldUntil       *string
	QuotaHoldCronExpr    *string
	UnreachableRetry     *RetryState
	CanonicalCompletions []string
	LastDeliveryQueued   any
	// Keys this package does not know, preserved verbatim
	Extra map[string]json.RawMessage
	// Read views attached by listings and the due scan, never persisted
	LatestExecution  map[string]any
	LiveExecution    map[string]any
	ScheduledInstant *string
	ExecutionID      *string
	// Markers a run sets for its own bookkeeping, never persisted
	run runMarks
	// Record arrived with enabled absent, which reads as true
	enabledAbsent bool
	// Key order the record was loaded with, kept on save the way a Python dict keeps insertion order
	keyOrder []string
	// Written with exactly the keyOrder keys, the way a raw jobs.json dict saves
	onlyStoredKeys bool
}

type kvPair struct {
	key   string
	value any
}

func sortedRawKeys(m map[string]json.RawMessage) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// Encodes ordered pairs as one compact JSON object without HTML escaping
func encodeOrdered(pairs []kvPair) ([]byte, error) {
	var buf bytes.Buffer
	buf.WriteByte('{')
	for i, p := range pairs {
		if i > 0 {
			buf.WriteByte(',')
		}
		k, _ := json.Marshal(p.key)
		buf.Write(k)
		buf.WriteByte(':')
		v, err := marshalNoEscape(p.value)
		if err != nil {
			return nil, fmt.Errorf("field %s: %w", p.key, err)
		}
		buf.Write(v)
	}
	buf.WriteByte('}')
	return buf.Bytes(), nil
}

// Marshals a value with HTML escaping off and no trailing newline
func marshalNoEscape(v any) ([]byte, error) {
	if raw, ok := v.(json.RawMessage); ok {
		if len(raw) == 0 {
			return []byte("null"), nil
		}
		return raw, nil
	}
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return nil, err
	}
	return bytes.TrimRight(buf.Bytes(), "\n"), nil
}

func strOrNull(p *string) any {
	if p == nil {
		return nil
	}
	return *p
}

func (j *Job) MarshalJSON() ([]byte, error) {
	skills := j.Skills
	if skills == nil {
		skills = []string{}
	}
	var repeat any
	if j.Repeat != nil {
		repeat = j.Repeat
	}
	var schedule any
	if j.Schedule != nil {
		schedule = *j.Schedule
	}
	var contextFrom any
	if j.ContextFrom != nil {
		contextFrom = j.ContextFrom
	}
	var unverified any
	if j.LastDeliveryUnverified != nil {
		unverified = j.LastDeliveryUnverified
	}
	var toolsets any
	if j.EnabledToolsets != nil {
		toolsets = j.EnabledToolsets
	}
	var origin any
	if j.Origin != nil {
		origin = j.Origin
	}
	var monitorState any
	if j.MonitorState != nil {
		monitorState = j.MonitorState
	}
	var lastFailure any
	if j.LastFailure != nil {
		lastFailure = j.LastFailure
	}
	pairs := []kvPair{
		{"id", j.ID}, {"name", j.Name}, {"prompt", j.Prompt}, {"skills", skills}, {"skill", strOrNull(j.Skill)},
		{"model", strOrNull(j.Model)}, {"provider", strOrNull(j.Provider)}, {"base_url", strOrNull(j.BaseURL)},
		{"script", strOrNull(j.Script)}, {"no_agent", j.NoAgent}, {"monitor_script", strOrNull(j.MonitorScript)},
		{"monitor_url", strOrNull(j.MonitorURL)}, {"monitor_state", monitorState}, {"context_from", contextFrom},
		{"schedule", schedule}, {"schedule_display", j.ScheduleDisplay}, {"repeat", repeat}, {"enabled", j.Enabled},
		{"state", j.State}, {"paused_at", strOrNull(j.PausedAt)}, {"paused_reason", strOrNull(j.PausedReason)},
		{"created_at", j.CreatedAt}, {"next_run_at", strOrNull(j.NextRunAt)}, {"last_run_at", strOrNull(j.LastRunAt)},
		{"last_status", strOrNull(j.LastStatus)}, {"last_error", strOrNull(j.LastError)},
		{"last_delivery_error", strOrNull(j.LastDeliveryError)}, {"last_delivery_unverified", unverified},
		{"last_failure", lastFailure}, {"failure_streak", j.FailureStreak}, {"deliver", strOrNull(j.Deliver)},
		{"origin", origin}, {"enabled_toolsets", toolsets}, {"workdir", strOrNull(j.Workdir)},
	}
	if j.AttachToSession != nil {
		pairs = append(pairs, kvPair{"attach_to_session", *j.AttachToSession})
	}
	if j.ReasoningEffort != nil {
		pairs = append(pairs, kvPair{"reasoning_effort", *j.ReasoningEffort})
	}
	if j.FailureDeliver != nil {
		pairs = append(pairs, kvPair{"failure_deliver", *j.FailureDeliver})
	}
	if j.Interpreter != nil {
		pairs = append(pairs, kvPair{"interpreter", *j.Interpreter})
	}
	if j.Timezone != nil {
		pairs = append(pairs, kvPair{"timezone", *j.Timezone})
	}
	if j.GraceMinutes != nil {
		pairs = append(pairs, kvPair{"grace_minutes", *j.GraceMinutes})
	}
	if j.CatchUp != nil {
		pairs = append(pairs, kvPair{"catch_up", *j.CatchUp})
	}
	if j.FireClaim != nil {
		pairs = append(pairs, kvPair{"fire_claim", j.FireClaim})
	}
	if j.RunClaim != nil {
		pairs = append(pairs, kvPair{"run_claim", j.RunClaim})
	}
	if j.PendingSlot != nil {
		pairs = append(pairs, kvPair{"pending_slot", j.PendingSlot})
	}
	if j.ManualRunAt != nil {
		pairs = append(pairs, kvPair{"manual_run_at", *j.ManualRunAt})
	}
	if j.ManualRunRestoreAt != nil {
		pairs = append(pairs, kvPair{"manual_run_restore_at", *j.ManualRunRestoreAt})
	}
	if j.ManualRunPrompt != nil {
		pairs = append(pairs, kvPair{"manual_run_prompt", *j.ManualRunPrompt})
	}
	if j.LastDispatch != nil {
		pairs = append(pairs, kvPair{"last_dispatch", j.LastDispatch})
	}
	if j.LastFireError != nil {
		pairs = append(pairs, kvPair{"last_fire_error", j.LastFireError})
	}
	if j.PreflightAlerted {
		pairs = append(pairs, kvPair{"preflight_alerted", true})
	}
	if j.QuotaHoldUntil != nil {
		pairs = append(pairs, kvPair{"quota_hold_until", *j.QuotaHoldUntil})
	}
	if j.QuotaHoldCronExpr != nil {
		pairs = append(pairs, kvPair{"quota_hold_cron_expr", *j.QuotaHoldCronExpr})
	}
	if j.UnreachableRetry != nil {
		pairs = append(pairs, kvPair{"unreachable_retry", j.UnreachableRetry})
	}
	if j.CanonicalCompletions != nil {
		pairs = append(pairs, kvPair{"canonical_completions", j.CanonicalCompletions})
	}
	if j.LastDeliveryQueued != nil {
		pairs = append(pairs, kvPair{"last_delivery_queued", j.LastDeliveryQueued})
	}
	for _, k := range sortedRawKeys(j.Extra) {
		pairs = append(pairs, kvPair{k, j.Extra[k]})
	}
	ordered := orderPairs(pairs, j.keyOrder)
	if j.onlyStoredKeys {
		stored := ordered[:0]
		for _, p := range ordered {
			if containsString(j.keyOrder, p.key) {
				stored = append(stored, p)
			}
		}
		ordered = stored
	}
	return encodeOrdered(ordered)
}

// Text view of a JSON scalar the way Python str() renders it, false for objects and null
func decodeText(raw json.RawMessage) (string, bool) {
	var v any
	if err := json.Unmarshal(raw, &v); err != nil {
		return "", false
	}
	return scalarText(v)
}

// Text of a decoded scalar, false for null and containers
func scalarText(v any) (string, bool) {
	switch t := v.(type) {
	case nil:
		return "", false
	case string:
		return t, true
	case bool:
		if t {
			return "True", true
		}
		return "False", true
	case float64:
		if t == math.Trunc(t) && math.Abs(t) < 1e15 {
			return strconv.FormatInt(int64(t), 10), true
		}
		return strconv.FormatFloat(t, 'g', -1, 64), true
	case json.Number:
		return t.String(), true
	}
	return "", false
}

func decodeNumber(raw json.RawMessage) (float64, bool) {
	var v any
	if err := json.Unmarshal(raw, &v); err != nil {
		return 0, false
	}
	switch t := v.(type) {
	case float64:
		return t, true
	case bool:
		if t {
			return 1, true
		}
		return 0, true
	case string:
		f, err := strconv.ParseFloat(strings.TrimSpace(t), 64)
		if err != nil {
			return 0, false
		}
		return f, true
	}
	return 0, false
}

func decodeBool(raw json.RawMessage) (bool, bool) {
	var v any
	if err := json.Unmarshal(raw, &v); err != nil {
		return false, false
	}
	return Truthy(v), true
}

// Truthy mirrors Python truthiness for decoded JSON values
func Truthy(v any) bool {
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
	case []string:
		return len(t) > 0
	}
	return true
}

func decodeStringPtr(raw json.RawMessage) *string {
	s, ok := decodeText(raw)
	if !ok {
		return nil
	}
	return &s
}

// Non blank string items of a JSON list or a bare string
func decodeStringList(raw json.RawMessage) ([]string, bool) {
	var v any
	if err := json.Unmarshal(raw, &v); err != nil {
		return nil, false
	}
	return anyStringList(v)
}

// String items of a decoded list or bare string, false for null and other shapes
func anyStringList(v any) ([]string, bool) {
	switch t := v.(type) {
	case nil:
		return nil, false
	case string:
		return []string{t}, true
	case []any:
		out := make([]string, 0, len(t))
		for _, item := range t {
			if s, ok := scalarText(item); ok {
				out = append(out, s)
			}
		}
		return out, true
	case []string:
		return t, true
	}
	return nil, false
}

func (j *Job) UnmarshalJSON(data []byte) error {
	var m map[string]json.RawMessage
	if err := json.Unmarshal(data, &m); err != nil {
		return err
	}
	*j = Job{Enabled: true, enabledAbsent: true, Extra: map[string]json.RawMessage{}}
	for k, raw := range m {
		if err := j.setField(k, raw); err != nil {
			return err
		}
	}
	if len(j.Extra) == 0 {
		j.Extra = nil
	}
	if v, err := pyjson.LoadsBytes(data); err == nil {
		if o, ok := v.(*pyjson.Object); ok {
			j.keyOrder = o.Keys
		}
	}
	return nil
}

// Assigns one raw JSON field onto the record, unknown keys landing in Extra
func (j *Job) setField(k string, raw json.RawMessage) error {
	isNull := string(bytes.TrimSpace(raw)) == "null"
	switch k {
	case "id":
		j.ID, _ = decodeText(raw)
	case "name":
		j.Name, _ = decodeText(raw)
	case "prompt":
		j.Prompt, _ = decodeText(raw)
	case "skills":
		j.Skills, _ = decodeStringList(raw)
	case "skill":
		j.Skill = decodeStringPtr(raw)
	case "model":
		j.Model = decodeStringPtr(raw)
	case "provider":
		j.Provider = decodeStringPtr(raw)
	case "base_url":
		j.BaseURL = decodeStringPtr(raw)
	case "script":
		j.Script = decodeStringPtr(raw)
	case "no_agent":
		j.NoAgent, _ = decodeBool(raw)
	case "monitor_script":
		j.MonitorScript = decodeStringPtr(raw)
	case "monitor_url":
		j.MonitorURL = decodeStringPtr(raw)
	case "monitor_state":
		if !isNull {
			var ms MonitorState
			if json.Unmarshal(raw, &ms) == nil {
				j.MonitorState = &ms
			}
		}
	case "context_from":
		j.ContextFrom, _ = decodeStringList(raw)
	case "schedule":
		var s Schedule
		if err := json.Unmarshal(raw, &s); err != nil {
			return err
		}
		if !isNull {
			j.Schedule = &s
		}
	case "schedule_display":
		j.ScheduleDisplay, _ = decodeText(raw)
	case "repeat":
		if !isNull {
			j.Repeat = decodeRepeat(raw)
		}
	case "enabled":
		j.enabledAbsent = false
		if isNull {
			j.Enabled = true
			j.enabledAbsent = true
		} else {
			j.Enabled, _ = decodeBool(raw)
		}
	case "state":
		j.State, _ = decodeText(raw)
	case "paused_at":
		j.PausedAt = decodeStringPtr(raw)
	case "paused_reason":
		j.PausedReason = decodeStringPtr(raw)
	case "created_at":
		j.CreatedAt, _ = decodeText(raw)
	case "next_run_at":
		j.NextRunAt = decodeStringPtr(raw)
	case "last_run_at":
		j.LastRunAt = decodeStringPtr(raw)
	case "last_status":
		j.LastStatus = decodeStringPtr(raw)
	case "last_error":
		j.LastError = decodeStringPtr(raw)
	case "last_delivery_error":
		j.LastDeliveryError = decodeStringPtr(raw)
	case "last_delivery_unverified":
		j.LastDeliveryUnverified, _ = decodeStringList(raw)
	case "last_failure":
		if !isNull {
			var st Stamp
			if json.Unmarshal(raw, &st) == nil {
				j.LastFailure = &st
			}
		}
	case "failure_streak":
		if n, ok := decodeNumber(raw); ok {
			j.FailureStreak = int(n)
		}
	case "deliver":
		if !isNull {
			if list, ok := decodeStringList(raw); ok && !bytes.HasPrefix(bytes.TrimSpace(raw), []byte(`"`)) {
				parts := []string{}
				for _, p := range list {
					if p = strings.TrimSpace(p); p != "" {
						parts = append(parts, p)
					}
				}
				joined := strings.Join(parts, ",")
				j.Deliver = &joined
			} else {
				j.Deliver = decodeStringPtr(raw)
			}
		}
	case "origin":
		if !isNull {
			var o map[string]any
			if json.Unmarshal(raw, &o) == nil {
				j.Origin = o
			} else {
				j.putExtra("origin", raw)
			}
		}
	case "enabled_toolsets":
		if !isNull {
			if list, ok := decodeStringList(raw); ok {
				if list == nil {
					list = []string{}
				}
				j.EnabledToolsets = list
			}
		}
	case "workdir":
		j.Workdir = decodeStringPtr(raw)
	case "attach_to_session":
		if !isNull {
			b, _ := decodeBool(raw)
			j.AttachToSession = &b
		}
	case "reasoning_effort":
		j.ReasoningEffort = decodeStringPtr(raw)
	case "failure_deliver":
		j.FailureDeliver = decodeStringPtr(raw)
	case "interpreter":
		j.Interpreter = decodeStringPtr(raw)
	case "timezone":
		if !isNull {
			j.Timezone = decodeStringPtr(raw)
		} else {
			empty := ""
			j.Timezone = &empty
		}
	case "grace_minutes":
		if isNull {
			j.GraceMinutes = nil
			delete(j.Extra, "grace_minutes")
		} else {
			delete(j.Extra, "grace_minutes")
			if n, ok := decodeNumber(raw); ok && n == math.Trunc(n) {
				v := int(n)
				j.GraceMinutes = &v
			} else {
				j.putExtra("grace_minutes", raw)
			}
		}
	case "catch_up":
		if !isNull {
			var b bool
			if json.Unmarshal(raw, &b) == nil {
				j.CatchUp = &b
			} else {
				j.putExtra("catch_up", raw)
			}
		}
	case "fire_claim":
		j.FireClaim = decodeClaim(raw)
	case "run_claim":
		j.RunClaim = decodeClaim(raw)
	case "pending_slot":
		if !isNull {
			var ps PendingSlot
			if json.Unmarshal(raw, &ps) == nil {
				j.PendingSlot = &ps
			}
		}
	case "manual_run_at":
		j.ManualRunAt = decodeStringPtr(raw)
	case "manual_run_restore_at":
		var ns NullString
		_ = json.Unmarshal(raw, &ns)
		j.ManualRunRestoreAt = &ns
	case "manual_run_prompt":
		j.ManualRunPrompt = decodeStringPtr(raw)
	case "last_dispatch":
		if !isNull {
			var d Dispatch
			if json.Unmarshal(raw, &d) == nil {
				j.LastDispatch = &d
			}
		}
	case "last_fire_error":
		if !isNull {
			var st Stamp
			if json.Unmarshal(raw, &st) == nil {
				j.LastFireError = &st
			}
		}
	case "preflight_alerted":
		j.PreflightAlerted, _ = decodeBool(raw)
	case "quota_hold_until":
		j.QuotaHoldUntil = decodeStringPtr(raw)
	case "quota_hold_cron_expr":
		j.QuotaHoldCronExpr = decodeStringPtr(raw)
	case "unreachable_retry":
		if !isNull {
			var rs RetryState
			if json.Unmarshal(raw, &rs) == nil {
				j.UnreachableRetry = &rs
			}
		}
	case "canonical_completions":
		j.CanonicalCompletions, _ = decodeStringList(raw)
	case "last_delivery_queued":
		if !isNull {
			var v any
			if json.Unmarshal(raw, &v) == nil {
				j.LastDeliveryQueued = v
			}
		}
	case "_model_unreachable":
		var v any
		_ = json.Unmarshal(raw, &v)
		j.run.modelUnreachable = pyTruthy(v)
	case "_quota_hold_seconds":
		j.run.quotaHoldSeconds = 0
		if n, ok := decodeNumber(raw); ok {
			j.run.quotaHoldSeconds = n
		}
	default:
		j.putExtra(k, raw)
	}
	return nil
}

// Keeps an unmodelled key verbatim, allocating the map a record built without decoding lacks
func (j *Job) putExtra(k string, raw json.RawMessage) {
	if j.Extra == nil {
		j.Extra = map[string]json.RawMessage{}
	}
	j.Extra[k] = raw
}

// Decodes a repeat object, normalizing a hand edited completed counter to a non negative int
func decodeRepeat(raw json.RawMessage) *Repeat {
	var m map[string]json.RawMessage
	if err := json.Unmarshal(raw, &m); err != nil {
		return &Repeat{}
	}
	r := &Repeat{}
	if t, ok := m["times"]; ok {
		if n, ok := decodeNumber(t); ok && n == math.Trunc(n) {
			v := int(n)
			r.Times = &v
		}
	}
	if c, ok := m["completed"]; ok {
		if n, ok := decodeNumber(c); ok && !math.IsInf(n, 0) && !math.IsNaN(n) {
			r.Completed = int(math.Max(math.Trunc(n), 0))
		}
	}
	return r
}

func decodeClaim(raw json.RawMessage) *Claim {
	if string(bytes.TrimSpace(raw)) == "null" {
		return nil
	}
	var m map[string]json.RawMessage
	if err := json.Unmarshal(raw, &m); err != nil {
		return nil
	}
	c := &Claim{}
	c.At, _ = decodeText(m["at"])
	c.By, _ = decodeText(m["by"])
	if o, ok := m["outage"]; ok {
		c.Outage, _ = decodeBool(o)
	}
	c.ExtraSlot, _ = decodeText(m["extra_slot"])
	return c
}

// Reports whether the completed counter was repaired while decoding
func repeatNeedsRepair(raw json.RawMessage) bool {
	var m map[string]json.RawMessage
	if err := json.Unmarshal(raw, &m); err != nil {
		return false
	}
	c, ok := m["completed"]
	if !ok {
		return false
	}
	var v any
	if err := json.Unmarshal(c, &v); err != nil {
		return true
	}
	f, isNum := v.(float64)
	return !isNum || f != math.Trunc(f) || f < 0 || math.IsInf(f, 0)
}

// Clone returns a deep copy of the record
func (j *Job) Clone() *Job {
	data, err := json.Marshal(j)
	if err != nil {
		return nil
	}
	var out Job
	if err := json.Unmarshal(data, &out); err != nil {
		return nil
	}
	if j.enabledAbsent {
		out.enabledAbsent = true
	}
	return &out
}

// Label shown in logs, the name then the id
func (j *Job) Label() string {
	if j.Name != "" {
		return j.Name
	}
	if j.ID != "" {
		return j.ID
	}
	return "?"
}

// Schedule kind, empty when the schedule is missing
func (j *Job) Kind() string {
	if j.Schedule == nil {
		return ""
	}
	return j.Schedule.Kind
}

func (j *Job) scheduleOrEmpty() *Schedule {
	if j.Schedule == nil {
		return &Schedule{}
	}
	return j.Schedule
}

func strPtr(s string) *string { return &s }

func intPtr(n int) *int { return &n }

func boolPtr(b bool) *bool { return &b }

func deref(p *string) string {
	if p == nil {
		return ""
	}
	return *p
}

// Loaded keys first in their loaded order, then keys the record gained since in canonical order
func orderPairs(pairs []kvPair, order []string) []kvPair {
	if len(order) == 0 {
		return pairs
	}
	byKey := make(map[string]kvPair, len(pairs))
	for _, p := range pairs {
		byKey[p.key] = p
	}
	out := make([]kvPair, 0, len(pairs))
	placed := make(map[string]bool, len(pairs))
	for _, k := range order {
		if p, ok := byKey[k]; ok && !placed[k] {
			out = append(out, p)
			placed[k] = true
		}
	}
	for _, p := range pairs {
		if !placed[p.key] {
			out = append(out, p)
		}
	}
	return out
}

// Encodes the dispatch stamp in Python key order with lateness as a Python float
func (d Dispatch) MarshalJSON() ([]byte, error) {
	return encodeOrdered([]kvPair{
		{"scheduled_at", d.ScheduledAt},
		{"dispatched_at", d.DispatchedAt},
		{"lateness_seconds", json.RawMessage(pyFloatJSON(d.LatenessSeconds))},
		{"kind", d.Kind},
	})
}

// Float as Python json.dumps writes it, Infinity and NaN spelled the way json accepts them back
func pyFloatJSON(f float64) string {
	switch {
	case math.IsNaN(f):
		return "NaN"
	case math.IsInf(f, 1):
		return "Infinity"
	case math.IsInf(f, -1):
		return "-Infinity"
	}
	return pyrepr.Float(f)
}
