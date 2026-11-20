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

	"github.com/nickheyer/icebath/pkg/tuigateway"
)

const spawnHistoryLimit = 10

// Tool call in flight, the row the live trail shows until it completes
type activeTool struct {
	Context     string
	ID          string
	Labels      []toolLabel
	Name        string
	Progress    string
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
	Battery                    bool
	BatteryStatus              *batteryInfo
	BgTasks                    map[string]bool
	Busy                       bool
	BusyInputMode              string
	Compact                    bool
	Compacting                 bool
	DestructiveSlashConfirm    bool
	DetailsMode                string
	DetailsModeCommandOverride bool
	FocusView                  bool
	GatewayConnected           *bool
	IndicatorStyle             string
	MouseTracking              string
	PasteCollapseLines         int
	PasteCollapseChars         int
	Sections                   map[string]string
	SessionTitle               string
	StatusBar                  string
	StatusBarFields            map[string]bool
	Timestamps                 bool
	Info                       *tuigateway.SessionInfo
	InlineDiffs                bool
	InlineImages               bool
	Notice                     *notice
	ShowReasoning              bool
	Sid                        string
	Status                     string
	StoredSid                  string
	Streaming                  bool
	Theme                      theme
	Usage                      *tuigateway.Usage
	Arena                      any
	PetRenderMode              tuigateway.PetRenderMode
	// Whether a turn closes with the line of what it did, and whether the status line counts the tokens it produced so far
	TurnSummary      bool
	SpinnerTokenFlow bool
}

// UI state of a fresh session
func newUIState() uiState {
	return uiState{
		BgTasks: map[string]bool{}, BusyInputMode: defaultBusyMode, DestructiveSlashConfirm: true, DetailsMode: "collapsed", IndicatorStyle: defaultIndicator,
		InlineDiffs: true, InlineImages: true, MouseTracking: bootMouseTracking(), PasteCollapseLines: defaultPasteLines, PasteCollapseChars: defaultPasteChars, Sections: map[string]string{},
		Status: tr("status.summoning"), StatusBar: "top", Streaming: true, Theme: startupTheme(), Usage: zeroUsage(), TurnSummary: true, SpinnerTokenFlow: true,
	}
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

// App state shared by the turn core, holding the ui, turn and spawn history stores
type tuiStore struct {
	ui           uiState
	turn         turnState
	spawnHistory []spawnSnapshot
	overlay      overlayState
	now          func() time.Time

	serverRequests         map[string]*serverRequest
	delegation             delegationState
	petFlash               *petFlash
	goodVibesTick          int
	transcriptGen          int
	bubblesShown           map[string]bool
	voicePartial           string
	connectionOp           *connectionOperation
	settledOperations      []string
	dismissedOperations    []string
	controls               controlState
	loopTickGen            int
	processSnapshot        processSnapshot
	agentSnapshot          agentSnapshot
	pendingVoiceTranscript *string
	pendingMutations       map[string]pendingMutation
	themes                 themeSync
	lastAdmission          map[string]string
	wakeUserDisabled       bool
	// Chip display and paste bodies of the newest submissions, oldest first, until the row of each is painted
	sentPastes []stagedPastes
}

// Marks the next submit of this transcript as a voice turn
func (s *tuiStore) markNextSubmitVoice(text string) { s.pendingVoiceTranscript = &text }

// Voice transcript a submit carries as its voice turn, taken once
func (s *tuiStore) takeVoiceTranscript() *string {
	t := s.pendingVoiceTranscript
	s.pendingVoiceTranscript = nil
	return t
}

// Store of a fresh session on wall time
func newTUIStore() *tuiStore {
	return &tuiStore{ui: newUIState(), now: time.Now, themes: themeSync{configPinned: loadBootTheme().seededPin}}
}

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

// Finished tree of the session in view holding an agent and its place there, minus ones when none does
func (s *tuiStore) finishedTreeAgent(id string) (int, int) {
	for k, snap := range s.spawnHistory {
		if snap.SessionID != s.ui.Sid {
			continue
		}
		if i := slices.IndexFunc(snap.Subagents, func(sa subagentProgress) bool { return sa.ID == id }); i >= 0 {
			return k, i
		}
	}
	return -1, -1
}

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
func normalizeSubagentStatus(status, fallback string) string {
	if lowered := strings.ToLower(status); slices.Contains(knownSubagentStatuses, lowered) {
		return lowered
	}
	return fallback
}

// Commits a spawn tree loaded from disk to the history, seconds on disk as milliseconds here
func (s *tuiStore) pushDiskSnapshot(r tuigateway.SpawnTreeLoadResult, path string) {
	normalised := make([]subagentProgress, 0, len(r.Subagents))
	for _, item := range r.Subagents {
		normalised = append(normalised, normaliseSubagent(item))
	}
	if len(normalised) == 0 {
		return
	}
	nowSeconds := s.nowMillis() / 1000
	finished := nowSeconds
	if r.FinishedAt != nil {
		finished = *r.FinishedAt
	}
	started := finished
	if r.StartedAt != nil {
		started = *r.StartedAt
	}
	label := val(r.Label)
	if label == "" {
		label = strconv.Itoa(len(normalised)) + " subagents"
	}
	snap := spawnSnapshot{FinishedAt: finished * 1000, FromDisk: true, ID: "disk-" + path, Label: label, Path: path,
		SessionID: val(r.SessionID), StartedAt: started * 1000, Subagents: normalised}
	s.spawnHistory = append([]spawnSnapshot{snap}, s.spawnHistory...)[:min(len(s.spawnHistory)+1, spawnHistoryLimit)]
}

// Subagent of a stored spawn tree, a blank goal, id or task count given its default
func normaliseSubagent(o tuigateway.SpawnTreeSubagent) subagentProgress {
	sa := subagentProgress{APICalls: o.APICalls, CostUSD: o.CostUSD, Depth: int(o.Depth), DurationSeconds: o.DurationSeconds,
		FilesRead: o.FilesRead, FilesWritten: o.FilesWritten, Index: int(o.Index), InputTokens: o.InputTokens,
		Iteration: o.Iteration, OutputTokens: o.OutputTokens, ReasoningTokens: o.ReasoningTokens, StartedAt: o.StartedAt,
		Status: normalizeSubagentStatus(string(o.Status), "completed"), TaskCount: int(o.TaskCount), ToolCount: int(o.ToolCount),
		Notes: o.Notes, Thinking: o.Thinking, Tools: o.Tools, Toolsets: o.Toolsets, Goal: o.Goal, ID: o.ID, Model: o.Model,
		ParentID: val(o.ParentID), Summary: o.Summary}
	for _, list := range []*[]string{&sa.Notes, &sa.Thinking, &sa.Tools} {
		if *list == nil {
			*list = []string{}
		}
	}
	if sa.Goal == "" {
		sa.Goal = "subagent"
	}
	if sa.ID == "" {
		sa.ID = "sa-" + randomBase36(6)
	}
	if sa.TaskCount == 0 {
		sa.TaskCount = 1
	}
	if o.OutputTail != nil {
		sa.OutputTail = make([]subagentOutput, len(o.OutputTail))
		for i, entry := range o.OutputTail {
			sa.OutputTail[i] = subagentOutput{IsError: entry.IsError, Preview: entry.Preview, Tool: entry.Tool}
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
	info := infoOf(s.ui.Info)
	d := submissionDestination{Sid: s.ui.Sid, Profile: "default", ProfileHome: profileHome, OwnerHome: profileHome}
	if stored := val(info.StoredSessionID); stored != "" {
		d.StoredSid = stored
	}
	if profile := val(info.ProfileName); profile != "" {
		d.Profile = profile
	}
	if attachURLEnv() != "" {
		if owner := val(info.ProfileID); owner != "" {
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
