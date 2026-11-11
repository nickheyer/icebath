package kanban

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
	"github.com/nickheyer/icebath/pkg/state"
)

// BlackboardPrefix opens every structured blackboard comment on a swarm root
const BlackboardPrefix = "[swarm:blackboard] "

// SwarmWorkerSpec is one parallel worker card of a swarm
type SwarmWorkerSpec struct {
	Profile           string
	Title             string
	Body              string
	Skills            []string
	Priority          int64
	MaxRuntimeSeconds *int64
}

// SwarmCreated holds the ids a swarm creation produced
type SwarmCreated struct {
	RootID        string
	WorkerIDs     []string
	VerifierID    string
	SynthesizerID string
}

// Ordered mapping of the created ids
func (s *SwarmCreated) AsDict() *pyjson.Object {
	return pyjson.ObjectOf("root_id", s.RootID, "worker_ids", stringsAny(s.WorkerIDs), "verifier_id", s.VerifierID, "synthesizer_id", s.SynthesizerID)
}

// SwarmOptions are the keyword arguments of a swarm creation
type SwarmOptions struct {
	Goal                string
	Workers             []SwarmWorkerSpec
	VerifierAssignee    string
	SynthesizerAssignee string
	RootTitle           *string
	VerifierTitle       string
	SynthesizerTitle    string
	Tenant              *string
	CreatedBy           string
	WorkspaceKind       *string
	WorkspacePath       *string
	Priority            int64
	IdempotencyKey      *string
}

// Swarm options at the create_swarm defaults
func DefaultSwarmOptions() SwarmOptions {
	return SwarmOptions{VerifierTitle: "Verify swarm outputs", SynthesizerTitle: "Synthesize swarm outputs", CreatedBy: "swarm-orchestrator"}
}

// Stripped required text, a ValueError naming the field when blank
func swarmRequireText(value, fieldName string) (string, error) {
	text := pystr.Strip(value)
	if text == "" {
		return "", &ValueError{fieldName + " is required"}
	}
	return text, nil
}

// Swarm protocol suffix appended to every worker, verifier and synthesizer body
func swarmContext(rootID, goal string) string {
	return fmt.Sprintf("\n\n## Swarm protocol\n- Swarm root / shared blackboard: `%s`.\n- Read "+
		"sibling/parent handoffs from Kanban context before working.\n- Put machine-readable "+
		"facts in completion metadata.\n- Put cross-worker notes on the root task using "+
		"structured comments.\n- Goal: %s\n", rootID, pystr.Strip(goal))
}

// Flips the blocked swarm root to done inside the caller's transaction, false when it was not blocked
func activateRootInline(ctx context.Context, conn state.SQLiteConn, rootID, summary string, metadata *pyjson.Object) (bool, error) {
	res, err := conn.ExecContext(ctx, `
        UPDATE tasks
           SET status       = 'done',
               completed_at = ?,
               claim_lock   = NULL,
               claim_expires= NULL,
               worker_pid   = NULL
         WHERE id = ?
           AND status = 'blocked'
        `, time.Now().Unix(), rootID)
	if err != nil {
		return false, err
	}
	if rowsAffected(res) != 1 {
		return false, nil
	}
	runID, err := synthesizeEndedRun(ctx, conn, rootID, "completed", &summary, nil, metadata, runProfile{})
	if err != nil {
		return false, err
	}
	var short any
	if s := pystr.Slice(summary, 0, 400); s != "" {
		short = s
	}
	return true, appendEvent(ctx, conn, rootID, "completed", payload("result_len", 0, "summary", short), &runID)
}

// Atomically creates a dispatchable swarm graph, completing its planning root after commit
func CreateSwarm(ctx context.Context, conn state.SQLiteConn, o SwarmOptions) (*SwarmCreated, error) {
	const activationSummary = "Swarm topology planned; root remains the shared blackboard."
	activated := false
	var created *SwarmCreated
	err := WriteTxn(ctx, conn, false, func() error {
		var err error
		if created, err = createSwarmUncommitted(ctx, conn, o); err != nil {
			return err
		}
		root, err := GetTask(ctx, conn, created.RootID)
		if err != nil {
			return err
		}
		if root != nil && root.Status == "blocked" {
			ok, err := activateRootInline(ctx, conn, created.RootID, activationSummary,
				pyjson.ObjectOf("kind", "kanban_swarm_v1", "goal", pystr.Strip(o.Goal), "worker_count", len(created.WorkerIDs)))
			if err != nil {
				return err
			}
			if !ok {
				return &RuntimeError{"could not activate the completed swarm topology"}
			}
			activated = true
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	if activated {
		if _, err := RecomputeReady(ctx, conn, nil); err != nil {
			return nil, err
		}
		root, err := GetTask(ctx, conn, created.RootID)
		if err != nil {
			return nil, err
		}
		run, err := LatestRun(ctx, conn, created.RootID)
		if err != nil {
			return nil, err
		}
		var assignee, runID any
		if root != nil {
			assignee = optStrValue(root.Assignee)
		}
		if run != nil {
			runID = run.ID
		}
		fireKanbanLifecycleHook(ctx, "kanban_task_completed", created.RootID, map[string]any{
			"board": GetCurrentBoard(ctx), "assignee": assignee, "run_id": runID, "summary": activationSummary,
		})
	}
	return created, nil
}

// Creates the swarm graph inside the caller's transaction, recovering an idempotent root's topology
func createSwarmUncommitted(ctx context.Context, conn state.SQLiteConn, o SwarmOptions) (*SwarmCreated, error) {
	goal, err := swarmRequireText(o.Goal, "goal")
	if err != nil {
		return nil, err
	}
	verifierAssignee, err := swarmRequireText(o.VerifierAssignee, "verifier_assignee")
	if err != nil {
		return nil, err
	}
	synthesizerAssignee, err := swarmRequireText(o.SynthesizerAssignee, "synthesizer_assignee")
	if err != nil {
		return nil, err
	}
	if len(o.Workers) == 0 {
		return nil, &ValueError{"at least one worker is required"}
	}
	for i, spec := range o.Workers {
		if _, err := swarmRequireText(spec.Profile, fmt.Sprintf("workers[%d].profile", i+1)); err != nil {
			return nil, err
		}
		if _, err := swarmRequireText(spec.Title, fmt.Sprintf("workers[%d].title", i+1)); err != nil {
			return nil, err
		}
	}
	common := func(opts CreateTaskOptions) CreateTaskOptions {
		createdBy := o.CreatedBy
		opts.CreatedBy, opts.Tenant, opts.WorkspaceKind, opts.WorkspacePath = &createdBy, o.Tenant, o.WorkspaceKind, o.WorkspacePath
		return opts
	}
	rootTitle := "Swarm: " + pystr.Slice(pystr.Splitlines(goal, false)[0], 0, 80)
	if o.RootTitle != nil && *o.RootTitle != "" {
		rootTitle = *o.RootTitle
	}
	rootBody := "Kanban Swarm v1 planning/root card. This card is completed " +
		"immediately so parallel workers can start while it remains the " +
		"shared blackboard and audit anchor.\n\nGoal:\n" + goal
	rootAssignee := o.CreatedBy
	root, err := CreateTask(ctx, conn, common(CreateTaskOptions{
		Title: rootTitle, Body: &rootBody, Assignee: &rootAssignee, Priority: o.Priority,
		IdempotencyKey: o.IdempotencyKey, InitialStatus: "blocked",
	}))
	if err != nil {
		return nil, err
	}
	board, err := LatestBlackboard(ctx, conn, root)
	if err != nil {
		return nil, err
	}
	if existing, _ := board.Get("topology"); isDict(existing) {
		var workerIDs []string
		if ids, _ := dictGet(existing, "worker_ids"); ids != nil {
			list, ok := ids.([]any)
			if !ok {
				return nil, &TypeError{fmt.Sprintf("'%s' object is not iterable", pyrepr.TypeName(ids))}
			}
			for _, x := range list {
				if pyrepr.Truthy(x) {
					workerIDs = append(workerIDs, pyrepr.Str(x))
				}
			}
		}
		verifierID, _ := dictGet(existing, "verifier_id")
		synthesizerID, _ := dictGet(existing, "synthesizer_id")
		if len(workerIDs) > 0 && pyrepr.Truthy(verifierID) && pyrepr.Truthy(synthesizerID) {
			return &SwarmCreated{root, workerIDs, pyrepr.Str(verifierID), pyrepr.Str(synthesizerID)}, nil
		}
	}
	contextSuffix := swarmContext(root, goal)
	workerIDs := make([]string, 0, len(o.Workers))
	for _, spec := range o.Workers {
		body := spec.Body + contextSuffix
		profile := spec.Profile
		priority := spec.Priority
		if priority == 0 {
			priority = o.Priority
		}
		var skills []string
		if len(spec.Skills) > 0 {
			skills = spec.Skills
		}
		id, err := CreateTask(ctx, conn, common(CreateTaskOptions{
			Title: spec.Title, Body: &body, Assignee: &profile, Parents: []string{root}, Priority: priority,
			Skills: skills, MaxRuntimeSeconds: spec.MaxRuntimeSeconds,
		}))
		if err != nil {
			return nil, err
		}
		workerIDs = append(workerIDs, id)
	}
	verifierBody := "Review every worker handoff and blackboard update. Gate the swarm: " +
		"complete only with metadata {\"gate\": \"pass\"} when evidence is " +
		"sufficient; otherwise block with exact missing work." + contextSuffix
	verifier, err := CreateTask(ctx, conn, common(CreateTaskOptions{
		Title: o.VerifierTitle, Body: &verifierBody, Assignee: &verifierAssignee, Parents: workerIDs,
		Priority: o.Priority, Skills: []string{"requesting-code-review"},
	}))
	if err != nil {
		return nil, err
	}
	synthesizerBody := "Synthesize the verified worker outputs into the final deliverable. " +
		"Do not start until the verifier has passed the gate." + contextSuffix
	synthesizer, err := CreateTask(ctx, conn, common(CreateTaskOptions{
		Title: o.SynthesizerTitle, Body: &synthesizerBody, Assignee: &synthesizerAssignee, Parents: []string{verifier},
		Priority: o.Priority, Skills: []string{"humanizer"},
	}))
	if err != nil {
		return nil, err
	}
	created := &SwarmCreated{root, workerIDs, verifier, synthesizer}
	topology := created.AsDict()
	topology.Set("goal", goal)
	if _, err := PostBlackboardUpdate(ctx, conn, root, o.CreatedBy, "topology", topology); err != nil {
		return nil, err
	}
	return created, nil
}

// Appends one structured key and value update to a swarm root blackboard
func PostBlackboardUpdate(ctx context.Context, conn state.SQLiteConn, rootID, author, key string, value any) (int64, error) {
	if _, err := swarmRequireText(rootID, "root_id"); err != nil {
		return 0, err
	}
	author, err := swarmRequireText(author, "author")
	if err != nil {
		return 0, err
	}
	if key, err = swarmRequireText(key, "key"); err != nil {
		return 0, err
	}
	body, err := pyjson.Dumps(pyjson.ObjectOf("key", key, "value", value), pyjson.EnsureASCII(false), pyjson.SortKeys(true))
	if err != nil {
		return 0, err
	}
	return AddComment(ctx, conn, rootID, author, BlackboardPrefix+body)
}

// Merged structured blackboard comments of a root, later keys winning and _authors naming each winner
func LatestBlackboard(ctx context.Context, conn state.SQLiteConn, rootID string) (*pyjson.Object, error) {
	merged := pyjson.NewObject()
	authors := pyjson.NewObject()
	comments, err := ListComments(ctx, conn, rootID)
	if err != nil {
		return nil, err
	}
	for _, comment := range comments {
		rest, ok := strings.CutPrefix(comment.Body, BlackboardPrefix)
		if !ok {
			continue
		}
		decoded, err := pyjson.Loads(rest)
		if err != nil {
			var de *pyjson.DecodeError
			if errors.As(err, &de) {
				continue
			}
			return nil, err
		}
		entry, ok := decoded.(*pyjson.Object)
		if !ok {
			return nil, &TypeError{fmt.Sprintf("'%s' object has no attribute 'get'", pyrepr.TypeName(decoded))}
		}
		key, ok := entry.GetOr("key", nil).(string)
		if !ok || key == "" {
			continue
		}
		merged.Set(key, entry.GetOr("value", nil))
		authors.Set(key, comment.Author)
	}
	if authors.Len() > 0 {
		merged.Set("_authors", authors)
	}
	return merged, nil
}

// Worker spec of a CLI --worker value naming profile, title and optional skills
func ParseWorkerArg(raw string) (SwarmWorkerSpec, error) {
	split := strings.SplitN(raw, ":", 3)
	parts := make([]string, len(split))
	for i, p := range split {
		parts[i] = pystr.Strip(p)
	}
	if len(parts) < 2 {
		return SwarmWorkerSpec{}, &ValueError{"worker must be profile:title or profile:title:skill,skill"}
	}
	skills := []string{}
	if len(parts) == 3 && parts[2] != "" {
		for _, s := range strings.Split(parts[2], ",") {
			if s = pystr.Strip(s); s != "" {
				skills = append(skills, s)
			}
		}
	}
	return SwarmWorkerSpec{Profile: parts[0], Title: parts[1], Body: parts[1], Skills: skills}, nil
}
