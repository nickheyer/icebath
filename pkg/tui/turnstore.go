package tui

import (
	"errors"
	"io/fs"
	"math"
	"math/rand/v2"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"
)

const spawnHistoryLimit = 10

// Tool call in flight, the row the live trail shows until it completes
type activeTool struct {
	Context     string
	ID          string
	Labels      []toolLabel
	Name        string
	VerboseArgs string
	StartedAt   time.Time
}

// Activity line of a turn and its tone, info, warn or error
type activityItem struct {
	ID   int
	Text string
	Tone string
}

// Tail entry of a subagent's tool output
type subagentOutput struct {
	IsError bool
	Preview string
	Tool    string
}

// Progress of one delegated subagent, optional numbers nil until an event carries them
type subagentProgress struct {
	APICalls        *float64
	CostUSD         *float64
	DelegationID    string
	Depth           int
	DurationSeconds *float64
	FilesRead       []string
	FilesWritten    []string
	Goal            string
	ID              string
	Index           int
	InputTokens     *float64
	Iteration       *float64
	Model           string
	Notes           []string
	OutputTail      []subagentOutput
	OutputTokens    *float64
	ParentID        string
	ReasoningTokens *float64
	StartedAt       *float64
	Status          string
	Summary         string
	TaskCount       int
	Thinking        []string
	ToolCount       int
	Tools           []string
	Toolsets        []string
}

// Status bar notice, sticky until cleared or ttl expiring after ttl_ms once shown
type notice struct {
	ID    string
	Key   string
	Kind  string
	Level string
	Text  string
	TTLms *float64
}

// Session level UI state the turn core reads and writes
type uiState struct {
	Busy          bool
	Compacting    bool
	Info          map[string]any
	Notice        *notice
	ShowReasoning bool
	Sid           string
	Status        string
	Streaming     bool
}

// UI state of a fresh session
func newUIState() uiState {
	return uiState{Status: tr("status.summoning"), Streaming: true}
}

// Live state of the turn in flight
type turnState struct {
	Activity           []activityItem
	Outcome            string
	Reasoning          string
	ReasoningActive    bool
	ReasoningStreaming bool
	ReasoningTokens    int
	StreamPendingTools []string
	StreamSegments     []msg
	Streaming          string
	Subagents          []subagentProgress
	TodoCollapsed      bool
	Todos              []todoItem
	ToolTokens         int
	Tools              []activeTool
	TurnTrail          []string
}

// Finished fan out kept for replay
type spawnSnapshot struct {
	FinishedAt float64
	FromDisk   bool
	ID         string
	Label      string
	Path       string
	SessionID  string
	StartedAt  float64
	Subagents  []subagentProgress
}

// App state shared by the turn core, the ui, turn and spawn history stores of ui-tui
type tuiStore struct {
	ui           uiState
	turn         turnState
	spawnHistory []spawnSnapshot
	now          func() time.Time
}

// Store of a fresh session on wall time
func newTUIStore() *tuiStore {
	return &tuiStore{ui: newUIState(), now: time.Now}
}

// Collapses or expands the live task list
func (s *tuiStore) toggleTodoCollapsed() { s.turn.TodoCollapsed = !s.turn.TodoCollapsed }

// Moves the turn's task list into a transcript trail row, collapsed when every item finished
func (s *tuiStore) archiveTodosAtTurnEnd() []msg {
	if len(s.turn.Todos) == 0 {
		return nil
	}
	archived := msg{Kind: "trail", Role: "system", Todos: s.turn.Todos}
	if isTodoDone(s.turn.Todos) {
		archived.TodoCollapsedByDefault = true
	} else {
		archived.TodoIncomplete = true
	}
	s.turn.TodoCollapsed, s.turn.Todos = false, nil
	return []msg{archived}
}

// Clears the turn state
func (s *tuiStore) resetTurnState() { s.turn = turnState{} }

// Milliseconds since the epoch of the store clock
func (s *tuiStore) nowMillis() float64 { return float64(s.now().UnixMilli()) }

// Commits a finished fan out to the history, the newest ten kept, an empty one dropped
func (s *tuiStore) pushSnapshot(subagents []subagentProgress, sessionID string, startedAt *float64) {
	if len(subagents) == 0 {
		return
	}
	now := s.nowMillis()
	started := math.Inf(1)
	if startedAt != nil {
		started = *startedAt
	} else {
		for _, sa := range subagents {
			at := now
			if sa.StartedAt != nil {
				at = *sa.StartedAt
			}
			started = math.Min(started, at)
		}
	}
	if math.IsInf(started, 0) || math.IsNaN(started) {
		started = now
	}
	snap := spawnSnapshot{FinishedAt: now, ID: "snap-" + strconv.FormatInt(int64(now), 36), Label: summarizeSpawnLabel(subagents),
		SessionID: sessionID, StartedAt: started, Subagents: slices.Clone(subagents)}
	s.spawnHistory = append([]spawnSnapshot{snap}, s.spawnHistory...)[:min(len(s.spawnHistory)+1, spawnHistoryLimit)]
}

// Label of a fan out, its first two root goals or the agent count
func summarizeSpawnLabel(subagents []subagentProgress) string {
	var top []string
	for _, sa := range subagents {
		if len(top) == 2 {
			break
		}
		rooted := sa.ParentID == "" || !slices.ContainsFunc(subagents, func(o subagentProgress) bool { return o.ID == sa.ParentID })
		if rooted {
			goal := sa.Goal
			if goal == "" {
				goal = "subagent"
			}
			top = append(top, goal)
		}
	}
	if label := strings.Join(top, " · "); label != "" {
		return label
	}
	if len(subagents) == 1 {
		return "1 agent"
	}
	return strconv.Itoa(len(subagents)) + " agents"
}

var knownSubagentStatuses = []string{"completed", "error", "failed", "interrupted", "queued", "running", "timeout"}

// Status lowered and checked against the known set, the fallback otherwise
func normalizeSubagentStatus(status any, fallback string) string {
	s, isStr := status.(string)
	if !isStr {
		return fallback
	}
	if lowered := strings.ToLower(s); slices.Contains(knownSubagentStatuses, lowered) {
		return lowered
	}
	return fallback
}

// Commits a spawn tree loaded from disk to the history, seconds on disk as milliseconds here
func (s *tuiStore) pushDiskSnapshot(r map[string]any, path string) {
	raw, _ := r["subagents"].([]any)
	normalised := make([]subagentProgress, 0, len(raw))
	for _, item := range raw {
		normalised = append(normalised, normaliseSubagent(item))
	}
	if len(normalised) == 0 {
		return
	}
	nowSeconds := s.nowMillis() / 1000
	seconds := func(keys ...string) float64 {
		for _, k := range keys {
			if v, has := r[k]; has && v != nil {
				return jsNumberOf(v)
			}
		}
		return nowSeconds
	}
	label, _ := r["label"].(string)
	if label == "" {
		label = strconv.Itoa(len(normalised)) + " subagents"
	}
	sessionID, _ := r["session_id"].(string)
	snap := spawnSnapshot{FinishedAt: seconds("finished_at") * 1000, FromDisk: true, ID: "disk-" + path, Label: label, Path: path,
		SessionID: sessionID, StartedAt: seconds("started_at", "finished_at") * 1000, Subagents: normalised}
	s.spawnHistory = append([]spawnSnapshot{snap}, s.spawnHistory...)[:min(len(s.spawnHistory)+1, spawnHistoryLimit)]
}

// Subagent of a stored spawn tree, every field checked for its type
func normaliseSubagent(raw any) subagentProgress {
	o, _ := raw.(map[string]any)
	str := func(k string) (string, bool) { v, isStr := o[k].(string); return v, isStr }
	num := func(k string) *float64 {
		if v, isNum := o[k].(float64); isNum {
			return &v
		}
		return nil
	}
	intOr := func(k string, fallback int) int {
		if v, isNum := o[k].(float64); isNum {
			return int(v)
		}
		return fallback
	}
	strs := func(k string) []string {
		list, isList := o[k].([]any)
		if !isList {
			return nil
		}
		out := []string{}
		for _, item := range list {
			if s, isStr := item.(string); isStr {
				out = append(out, s)
			}
		}
		return out
	}
	sa := subagentProgress{APICalls: num("apiCalls"), CostUSD: num("costUsd"), Depth: intOr("depth", 0), DurationSeconds: num("durationSeconds"),
		FilesRead: strs("filesRead"), FilesWritten: strs("filesWritten"), Index: intOr("index", 0), InputTokens: num("inputTokens"),
		Iteration: num("iteration"), OutputTokens: num("outputTokens"), ReasoningTokens: num("reasoningTokens"), StartedAt: num("startedAt"),
		Status: normalizeSubagentStatus(o["status"], "completed"), TaskCount: intOr("taskCount", 1), ToolCount: intOr("toolCount", 0),
		Notes: strs("notes"), Thinking: strs("thinking"), Tools: strs("tools"), Toolsets: strs("toolsets")}
	for _, list := range []*[]string{&sa.Notes, &sa.Thinking, &sa.Tools} {
		if *list == nil {
			*list = []string{}
		}
	}
	if sa.Goal, _ = str("goal"); sa.Goal == "" {
		if _, isStr := str("goal"); !isStr {
			sa.Goal = "subagent"
		}
	}
	if id, isStr := str("id"); isStr {
		sa.ID = id
	} else {
		sa.ID = "sa-" + randomBase36(6)
	}
	sa.Model, _ = str("model")
	sa.ParentID, _ = str("parentId")
	sa.Summary, _ = str("summary")
	if tail, isList := o["outputTail"].([]any); isList {
		sa.OutputTail = []subagentOutput{}
		for _, item := range tail {
			entry, _ := item.(map[string]any)
			isErr, _ := entry["isError"].(bool)
			preview, _ := entry["preview"].(string)
			tool, _ := entry["tool"].(string)
			sa.OutputTail = append(sa.OutputTail, subagentOutput{IsError: isErr, Preview: preview, Tool: tool})
		}
	}
	return sa
}

// Random lowercase base 36 text of n digits
func randomBase36(n int) string {
	const digits = "0123456789abcdefghijklmnopqrstuvwxyz"
	b := make([]byte, n)
	for i := range b {
		b[i] = digits[rand.IntN(len(digits))]
	}
	return string(b)
}

// Where a submission goes, the session and the profile home it was typed against
type submissionDestination struct {
	Sid         string
	StoredSid   string
	Profile     string
	ProfileHome string
	OwnerHome   string
}

// Real path of a home, a missing tail kept as written under its resolved parent
func canonicalHome(path string) (string, error) {
	real, err := filepath.EvalSymlinks(path)
	if err == nil {
		return real, nil
	}
	if !errors.Is(err, fs.ErrNotExist) {
		return "", err
	}
	parent := filepath.Dir(path)
	if parent == path {
		return path, nil
	}
	resolved, err := canonicalHome(parent)
	if err != nil {
		return "", err
	}
	return filepath.Join(resolved, filepath.Base(path)), nil
}

// Destination of a submission typed now, the session, profile and owner home in view
func (s *tuiStore) captureDestination() (submissionDestination, error) {
	root := os.Getenv("ICEBATH_HOME")
	if _, set := os.LookupEnv("ICEBATH_HOME"); !set {
		userHome, err := os.UserHomeDir()
		if err != nil {
			return submissionDestination{}, err
		}
		root = filepath.Join(userHome, ".icebath")
	}
	abs, err := filepath.Abs(root)
	if err != nil {
		return submissionDestination{}, err
	}
	profileHome, err := canonicalHome(abs)
	if err != nil {
		return submissionDestination{}, err
	}
	info := s.ui.Info
	d := submissionDestination{Sid: s.ui.Sid, Profile: "default", ProfileHome: profileHome, OwnerHome: profileHome}
	if stored, _ := info["stored_session_id"].(string); stored != "" {
		d.StoredSid = stored
	}
	if profile, _ := info["profile_name"].(string); profile != "" {
		d.Profile = profile
	}
	if attachURLEnv() != "" {
		if owner, _ := info["profile_id"].(string); owner != "" {
			d.OwnerHome = owner
		}
	}
	return d, nil
}

// Whether the destination is still the one in view, the same session, profile and home
func (s *tuiStore) isCurrentDestination(d submissionDestination) bool {
	current, err := s.captureDestination()
	return err == nil && current.Sid == d.Sid && current.Profile == d.Profile && current.ProfileHome == d.ProfileHome
}
