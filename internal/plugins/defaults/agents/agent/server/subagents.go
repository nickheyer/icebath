package server

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"

	"connectrpc.com/connect/v2"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/nickheyer/icebath/internal/config"
	delegationconfig "github.com/nickheyer/icebath/internal/delegation/config"
	"github.com/nickheyer/icebath/pkg/fsx"
	agentv1 "github.com/nickheyer/icebath/pkg/gen/icebath/plugins/agent/v1"
	"github.com/nickheyer/icebath/pkg/strs"
)

const (
	// Finished subagents whose goal and delegation stay known, for what their processes are attributed to
	recentSubagents = 200
	// Hops a walk up the spawn tree takes before it gives up
	treeHops = 8
	// Newest bytes of a transcript a tail reads
	tailBytes = 16 << 10
	// Name of the index of the spawn trees kept for a session
	treesIndex = "_index.jsonl"
)

// Subagents running in this process, in the order they were spawned, and what is kept of those that finished
type spawnTree struct {
	mu   sync.Mutex
	live []*child
	// Goal and delegation of finished subagents, by id, the oldest dropped past the cap
	recent map[string]finished
	order  []string
	// New subagents are refused everywhere
	paused bool
	// Short numbers of the delegations of each session, such as set 2, by delegation id
	sets map[string]map[string]int
	// The cost of many parallel subagents was warned of
	warned bool
}

type finished struct {
	goal, delegation, owner string
}

func newSpawnTree() *spawnTree {
	return &spawnTree{recent: map[string]finished{}, sets: map[string]map[string]int{}}
}

func (t *spawnTree) add(c *child) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.live = append(t.live, c)
}

// Takes the finished subagent off the live list, keeping its goal and delegation
func (t *spawnTree) remove(c *child) {
	t.mu.Lock()
	defer t.mu.Unlock()
	i := slices.Index(t.live, c)
	if i < 0 {
		return
	}
	t.live = slices.Delete(t.live, i, i+1)
	t.recent[c.id] = finished{goal: c.goal, delegation: c.delegation, owner: c.owner}
	t.order = append(t.order, c.id)
	for len(t.order) > recentSubagents {
		delete(t.recent, t.order[0])
		t.order = t.order[1:]
	}
}

// Live subagent of the id, nil when none runs
func (t *spawnTree) find(id string) *child {
	t.mu.Lock()
	defer t.mu.Unlock()
	for _, c := range t.live {
		if c.id == id {
			return c
		}
	}
	return nil
}

// Live subagent whose session is the one given, nil when none
func (t *spawnTree) bySession(session string) *child {
	t.mu.Lock()
	defer t.mu.Unlock()
	for _, c := range t.live {
		if c.session() == session {
			return c
		}
	}
	return nil
}

func (t *spawnTree) snapshot() []*child {
	t.mu.Lock()
	defer t.mu.Unlock()
	return slices.Clone(t.live)
}

func (t *spawnTree) setPaused(paused bool) bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.paused = paused
	return paused
}

func (t *spawnTree) isPaused() bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.paused
}

// Short tag of a delegation within its session, such as set 2, numbered in the order the session's delegations came
func (t *spawnTree) tag(session, delegation string) string {
	if delegation == "" {
		return ""
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	sets := t.sets[session]
	if sets == nil {
		sets = map[string]int{}
		t.sets[session] = sets
	}
	n, ok := sets[delegation]
	if !ok {
		n = len(sets) + 1
		sets[delegation] = n
	}
	return fmt.Sprintf("set %d", n)
}

// Whether the subagent belongs to the spawn tree of the session: started by it or below one it started, or by a session its compression
// chain holds
func (a *Agent) owns(c *child, session string, lineage []string) bool {
	owner := c.owner
	for range treeHops {
		if owner == session || slices.Contains(lineage, owner) {
			return true
		}
		parent := a.tree.bySession(owner)
		if parent == nil {
			return false
		}
		owner = parent.owner
	}
	return false
}

// Sessions of the compression chain the session is in, for subagents started before it continued in a new session
func (a *Agent) lineage(ctx context.Context, session string) []string {
	store, err := a.store()
	if err != nil {
		return nil
	}
	chain, err := store.Lineage(ctx, session)
	if err != nil {
		return nil
	}
	return chain
}

// Live subagents of the session's spawn tree, in spawn order
func (a *Agent) owned(ctx context.Context, session string) []*child {
	lineage := a.lineage(ctx, session)
	var out []*child
	for _, c := range a.tree.snapshot() {
		if a.owns(c, session, lineage) {
			out = append(out, c)
		}
	}
	return out
}

// A live subagent as the spawn tree lists it
func (c *child) listed() *agentv1.LiveSubagent {
	c.mu.Lock()
	defer c.mu.Unlock()
	return &agentv1.LiveSubagent{Id: c.id, ParentId: c.parentID, Depth: int32(c.depth - 1), Goal: c.goal, DelegationId: c.delegation, Model: c.model,
		Started: timestamppb.New(c.started), Status: c.status, ToolCount: int32(c.tools), LastTool: c.tool, Steering: c.steering,
		Transcript: c.transcript.path, SessionId: c.session(), TaskIndex: int32(c.index), TaskCount: int32(c.count)}
}

// Live subagents of the session's spawn tree with the pause, the limits and the session's failed background delegations
func (a *Agent) Subagents(ctx context.Context, session string) (*agentv1.SubagentsResponse, error) {
	r, err := a.run(session)
	if err != nil {
		return nil, err
	}
	settings := a.settings(r)
	out := &agentv1.SubagentsResponse{Paused: a.tree.isPaused(), MaxSpawnDepth: int32(maxSpawnDepth(settings)),
		MaxConcurrentChildren: int32(maxChildren(settings))}
	for _, c := range a.owned(ctx, session) {
		out.Subagents = append(out.Subagents, c.listed())
	}
	out.Failed = a.async.failed(settings, session, a.lineage(ctx, session))
	return out, nil
}

// Stops one subagent of the session's spawn tree; whether it still ran
func (a *Agent) Interrupt(ctx context.Context, session, id string) bool {
	c := a.tree.find(id)
	if c == nil || !a.owns(c, session, a.lineage(ctx, session)) {
		return false
	}
	return c.interrupt("You stopped subagent " + id + " in the app.")
}

// Queues guidance for the session's agent, or one subagent of its spawn tree; why it was not queued when it was not
func (a *Agent) Steer(ctx context.Context, session, id, text string) (bool, string) {
	if text = strings.TrimSpace(text); text == "" {
		return false, "Type a message to send to the agent first."
	}
	if id == "" {
		r, err := a.run(session)
		if err != nil {
			return false, err.Error()
		}
		return a.steer(r, text)
	}
	c := a.tree.find(id)
	if c == nil || !a.owns(c, session, a.lineage(ctx, session)) {
		return false, "Subagent " + id + " is no longer running in this session."
	}
	if !c.steer(text) {
		return false, "Subagent " + id + " is finishing or has finished, so it takes no more messages."
	}
	return true, ""
}

// Queues the text for the session's running turn, delivered after its next tool results
func (a *Agent) steer(r *run, text string) (bool, string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if !r.busy {
		return false, "No reply is running, so there is nothing to steer. Send the text as a message instead."
	}
	r.steering = append(r.steering, text)
	return true, ""
}

// Newest part of the transcript of a subagent the session's spawn tree runs or ran, false when it left none
func (a *Agent) Tail(ctx context.Context, session, id string) (string, bool, bool, error) {
	path := ""
	if c := a.tree.find(id); c != nil && a.owns(c, session, a.lineage(ctx, session)) {
		path = c.transcript.path
	}
	if path == "" {
		var err error
		if path, err = a.transcriptOf(ctx, session, id); err != nil {
			return "", false, false, err
		}
	}
	if path == "" {
		return "", false, false, nil
	}
	f, err := os.Open(path)
	if err != nil {
		return "", false, false, nil
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return "", false, false, nil
	}
	start := max(info.Size()-tailBytes, 0)
	data := make([]byte, info.Size()-start)
	if _, err := f.ReadAt(data, start); err != nil && len(data) == 0 {
		return "", false, false, nil
	}
	text := strings.ToValidUTF8(string(data), "�")
	if start > 0 {
		if _, rest, ok := strings.Cut(text, "\n"); ok {
			text = rest
		}
	}
	return text, true, start > 0, nil
}

// Transcript a finished subagent of the session's conversation kept, found through the sessions it delegated to
func (a *Agent) transcriptOf(ctx context.Context, session, id string) (string, error) {
	store, err := a.store()
	if err != nil {
		return "", err
	}
	owners := append([]string{session}, a.lineage(ctx, session)...)
	for range treeHops {
		var next []string
		for _, owner := range owners {
			children, err := a.delegatedBy(ctx, store, owner)
			if err != nil {
				return "", err
			}
			for _, k := range children {
				if k.id == id {
					return k.transcript, nil
				}
				next = append(next, k.session)
			}
		}
		if len(next) == 0 {
			break
		}
		owners = next
	}
	return "", nil
}

// Folder the spawn trees of a session are kept in
func (a *Agent) treesDir(profile, session string) string {
	return filepath.Join(a.config.ProfileDir(profile), "spawn-trees", cmp.Or(safeName(session), "default"))
}

// File name part of a session id, nothing a path could climb out by
func safeName(name string) string {
	return strings.NewReplacer("/", "_", "\\", "_", "..", "_").Replace(name)
}

// Keeps a finished spawn tree of the session, a tree kept again under its time keeping its one index entry
func (a *Agent) SaveTree(session string, tree *agentv1.SpawnTree) (string, error) {
	if len(tree.GetSubagents()) == 0 {
		return "", connect.NewError(connect.CodeInvalidArgument, "There were no subagents, so nothing was saved.")
	}
	r, err := a.run(session)
	if err != nil {
		return "", err
	}
	if tree.Finished == nil {
		tree.Finished = timestamppb.Now()
	}
	tree.SessionId = session
	dir := a.treesDir(r.profile, session)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
	path := filepath.Join(dir, tree.GetFinished().AsTime().UTC().Format("20060102T150405")+".json")
	tree.Path = path
	_, statErr := os.Stat(path)
	data, err := protojson.Marshal(tree)
	if err != nil {
		return "", err
	}
	if err := fsx.Write(path, data, 0o600); err != nil {
		return "", errors.New("The subagent tree could not be saved. " + strs.Full(err.Error()))
	}
	if statErr == nil {
		return path, nil
	}
	entry := &agentv1.SpawnTree{SessionId: session, Started: tree.Started, Finished: tree.Finished, Label: tree.Label, Path: path}
	line, err := protojson.Marshal(entry)
	if err != nil {
		return path, nil
	}
	index, err := os.OpenFile(filepath.Join(dir, treesIndex), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return path, nil
	}
	defer index.Close()
	index.Write(append(line, '\n'))
	return path, nil
}

// Spawn trees kept for the session, or every session of its profile, newest first, with the subagents each holds
func (a *Agent) Trees(session string, every bool, limit int) ([]*agentv1.SpawnTree, []int32, error) {
	r, err := a.run(session)
	if err != nil {
		return nil, nil, err
	}
	root := filepath.Join(a.config.ProfileDir(r.profile), "spawn-trees")
	dirs := []string{a.treesDir(r.profile, session)}
	if every {
		dirs = nil
		entries, _ := os.ReadDir(root)
		for _, e := range entries {
			if e.IsDir() {
				dirs = append(dirs, filepath.Join(root, e.Name()))
			}
		}
	}
	type listed struct {
		tree  *agentv1.SpawnTree
		count int32
	}
	var found []listed
	for _, dir := range dirs {
		files, _ := filepath.Glob(filepath.Join(dir, "*.json"))
		for _, path := range files {
			tree, err := readTree(path)
			if err != nil {
				continue
			}
			count := int32(len(tree.GetSubagents()))
			tree.Subagents, tree.Path = nil, path
			found = append(found, listed{tree, count})
		}
	}
	slices.SortStableFunc(found, func(x, y listed) int { return y.tree.GetFinished().AsTime().Compare(x.tree.GetFinished().AsTime()) })
	if limit <= 0 {
		limit = 50
	}
	found = found[:min(limit, len(found))]
	trees, counts := make([]*agentv1.SpawnTree, len(found)), make([]int32, len(found))
	for i, k := range found {
		trees[i], counts[i] = k.tree, k.count
	}
	return trees, counts, nil
}

func readTree(path string) (*agentv1.SpawnTree, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	tree := &agentv1.SpawnTree{}
	if err := protojson.Unmarshal(data, tree); err != nil {
		return nil, err
	}
	return tree, nil
}

// A kept spawn tree, refused when the path lies outside the spawn trees of the profiles
func (a *Agent) LoadTree(path string) (*agentv1.SpawnTree, error) {
	resolved, err := filepath.EvalSymlinks(path)
	if err != nil {
		return nil, connect.NewError(connect.CodeNotFound, "The saved subagent tree could not be opened. "+strs.Full(err.Error()))
	}
	profiles, err := filepath.EvalSymlinks(filepath.Dir(a.config.ProfileDir(a.config.Profile())))
	if err != nil {
		return nil, connect.NewError(connect.CodePermissionDenied, "That file is not among the saved subagent trees, so it can't be opened.")
	}
	rel, err := filepath.Rel(profiles, resolved)
	parts := strings.Split(filepath.ToSlash(rel), "/")
	if err != nil || len(parts) < 3 || parts[0] == ".." || parts[1] != "spawn-trees" {
		return nil, connect.NewError(connect.CodePermissionDenied, fmt.Sprintf("%s is not among the saved subagent trees of any profile, so it can't be opened.", resolved))
	}
	tree, err := readTree(resolved)
	if err != nil {
		return nil, connect.NewError(connect.CodeInvalidArgument, "The saved subagent tree could not be read. "+strs.Full(err.Error()))
	}
	tree.Path = resolved
	return tree, nil
}

// Depth of the spawn tree the settings allow, never below one
func maxSpawnDepth(settings *config.Config) int {
	n := settings.GetInt(delegationconfig.MaxSpawnDepth)
	if n < 1 {
		slog.Warn(fmt.Sprintf("subagent depth %d is below the floor 1; using 1", n))
		return 1
	}
	return n
}

// Subagents of one call, and background delegations at once, the settings allow, never below one
func maxChildren(settings *config.Config) int {
	return max(settings.GetInt(delegationconfig.MaxConcurrent), 1)
}

func (s *service) Subagents(ctx context.Context, request *agentv1.SubagentsRequest) (*agentv1.SubagentsResponse, error) {
	return s.agent.Subagents(ctx, request.GetSessionId())
}

func (s *service) Interrupt(ctx context.Context, request *agentv1.InterruptRequest) (*agentv1.InterruptResponse, error) {
	return &agentv1.InterruptResponse{Found: s.agent.Interrupt(ctx, request.GetSessionId(), request.GetSubagentId())}, nil
}

func (s *service) Steer(ctx context.Context, request *agentv1.SteerRequest) (*agentv1.SteerResponse, error) {
	queued, reason := s.agent.Steer(ctx, request.GetSessionId(), request.GetSubagentId(), request.GetText())
	return &agentv1.SteerResponse{Queued: queued, Reason: reason}, nil
}

func (s *service) Pause(_ context.Context, request *agentv1.PauseRequest) (*agentv1.PauseResponse, error) {
	return &agentv1.PauseResponse{Paused: s.agent.tree.setPaused(request.GetPaused())}, nil
}

func (s *service) Tail(ctx context.Context, request *agentv1.TailRequest) (*agentv1.TailResponse, error) {
	text, ok, truncated, err := s.agent.Tail(ctx, request.GetSessionId(), request.GetSubagentId())
	if err != nil {
		return nil, err
	}
	return &agentv1.TailResponse{Available: ok, Text: text, Truncated: truncated}, nil
}

func (s *service) SaveTree(_ context.Context, request *agentv1.SaveTreeRequest) (*agentv1.SaveTreeResponse, error) {
	path, err := s.agent.SaveTree(request.GetTree().GetSessionId(), request.GetTree())
	if err != nil {
		return nil, err
	}
	return &agentv1.SaveTreeResponse{Path: path}, nil
}

func (s *service) Trees(_ context.Context, request *agentv1.TreesRequest) (*agentv1.TreesResponse, error) {
	trees, counts, err := s.agent.Trees(request.GetSessionId(), request.GetEvery(), int(request.GetLimit()))
	if err != nil {
		return nil, err
	}
	return &agentv1.TreesResponse{Trees: trees, Counts: counts}, nil
}

func (s *service) LoadTree(_ context.Context, request *agentv1.LoadTreeRequest) (*agentv1.LoadTreeResponse, error) {
	tree, err := s.agent.LoadTree(request.GetPath())
	if err != nil {
		return nil, err
	}
	return &agentv1.LoadTreeResponse{Tree: tree}, nil
}
