package server

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"connectrpc.com/connect/v2"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/nickheyer/icebath/internal/config"
	"github.com/nickheyer/icebath/internal/git"
	"github.com/nickheyer/icebath/internal/server"
	"github.com/nickheyer/icebath/internal/sessions"
	sessionsconfig "github.com/nickheyer/icebath/internal/sessions/config"
	"github.com/nickheyer/icebath/internal/terminal"
	"github.com/nickheyer/icebath/internal/worktrees"
	worktreesconfig "github.com/nickheyer/icebath/internal/worktrees/config"
	"github.com/nickheyer/icebath/pkg/fsx"
	worktreesv1 "github.com/nickheyer/icebath/pkg/gen/icebath/plugins/worktrees/v1"
	"github.com/nickheyer/icebath/pkg/gen/icebath/plugins/worktrees/v1/worktreesv1connect"
)

type service struct {
	config    *config.Config
	worktrees *worktrees.Worktrees
	sessions  *sessions.Registry
	terminal  *terminal.Terminal
}

func Register(registry *server.Registry, cfg *config.Config, host *worktrees.Worktrees, stores *sessions.Registry, shells *terminal.Terminal) {
	registry.Register(&service{config: cfg, worktrees: host, sessions: stores, terminal: shells})
}

func (s *service) Mount(services *connect.Server) {
	worktreesv1connect.RegisterWorktreeServiceHandler(services, s)
}

// Repository a folder is in, a refusal the user reads outside one
func repository(ctx context.Context, dir string) (string, error) {
	if repo := git.Root(ctx, dir); repo != "" {
		return repo, nil
	}
	return "", connect.NewError(connect.CodeFailedPrecondition, "Not inside a git repository (or pass --repo <path>).")
}

func tree(t worktrees.Tree) *worktreesv1.Tree {
	return &worktreesv1.Tree{Path: t.Path, Branch: t.Branch, Repo: t.Repo}
}

func from(t *worktreesv1.Tree) worktrees.Tree {
	return worktrees.Tree{Path: t.GetPath(), Branch: t.GetBranch(), Repo: t.GetRepo()}
}

func (s *service) Status(ctx context.Context, request *worktreesv1.StatusRequest) (*worktreesv1.StatusResponse, error) {
	return &worktreesv1.StatusResponse{Repo: git.Root(ctx, request.GetDirectory())}, nil
}

func (s *service) List(ctx context.Context, request *worktreesv1.ListRequest) (*worktreesv1.ListResponse, error) {
	response := &worktreesv1.ListResponse{Repo: git.Root(ctx, request.GetDirectory())}
	if response.Repo == "" {
		return response, nil
	}
	if listing, ok := git.Output(ctx, response.Repo, 10*time.Second, "worktree", "list"); ok {
		for line := range strings.Lines(listing) {
			response.Lines = append(response.Lines, strings.TrimRight(line, "\r\n"))
		}
	}
	return response, nil
}

// Days a request holds back young trees by, refused below zero
func olderThan(days *float64) error {
	if days != nil && *days < 0 {
		return connect.NewError(connect.CodeInvalidArgument, "--older-than must be a non-negative number of days.")
	}
	return nil
}

func record(r worktrees.TreeRecord) *worktreesv1.TreeRecord {
	out := &worktreesv1.TreeRecord{Name: r.Name, Path: r.Path, Branch: r.Branch, AgeDays: r.AgeDays, Verdict: r.Verdict, Reason: r.Reason,
		Untracked: r.Untracked}
	if r.SizeMB != nil {
		mb := int32(*r.SizeMB)
		out.SizeMb = &mb
	}
	return out
}

func skipped(list []worktrees.Skipped) []*worktreesv1.Skipped {
	var out []*worktreesv1.Skipped
	for _, s := range list {
		out = append(out, &worktreesv1.Skipped{Path: s.Path, Bytes: s.Bytes})
	}
	return out
}

func (s *service) Audit(ctx context.Context, request *worktreesv1.AuditRequest) (*worktreesv1.AuditResponse, error) {
	repo, err := repository(ctx, request.GetDirectory())
	if err != nil {
		return nil, err
	}
	if err := olderThan(request.OlderThan); err != nil {
		return nil, err
	}
	records, err := s.worktrees.Audit(s.config, repo, true, request.OlderThan)
	if err != nil {
		return nil, err
	}
	external, err := worktrees.External(repo)
	if err != nil {
		return nil, err
	}
	branches, err := worktrees.Branches(repo)
	if err != nil {
		return nil, err
	}
	response := &worktreesv1.AuditResponse{Repo: repo}
	for _, r := range records {
		response.Trees = append(response.Trees, record(r))
	}
	for _, e := range external {
		response.External = append(response.External, &worktreesv1.ExternalTree{Path: e.Path, Branch: e.Branch, Locked: e.Locked, Missing: e.Missing})
	}
	for _, b := range branches {
		response.Branches = append(response.Branches, &worktreesv1.Branch{Name: b.Name, Verdict: b.Verdict, Reason: b.Reason})
	}
	for _, a := range worktrees.Archives(repo) {
		archive := &worktreesv1.Archive{Ref: a.Ref, Name: a.Name, Sha: a.Sha, Branch: a.Meta.Branch, Reason: a.Meta.Reason, Skipped: skipped(a.Meta.Skipped),
			Path: a.Meta.Path}
		if !a.Created.IsZero() {
			archive.Created = timestamppb.New(a.Created)
		}
		response.Archives = append(response.Archives, archive)
	}
	return response, nil
}

func decision(d *worktrees.Decision) *worktreesv1.Decision {
	out := &worktreesv1.Decision{Name: d.Name, Path: d.Path, Action: d.Action, Reason: d.Reason, AgeHours: d.AgeHours, Mtime: d.Mtime, Force: d.Force,
		Verdict: d.Verdict, LockState: d.Lock, TierHours: d.Tier, PlannedAt: d.Planned}
	if d.SizeMB != nil {
		mb := int32(*d.SizeMB)
		out.SizeMb = &mb
	}
	return out
}

// Whether an audit verdict keeps a tree for the work it holds, rather than for being a kanban tree or in use
func preserved(r worktrees.TreeRecord) bool {
	return r.Verdict == "keep" && !strings.Contains(r.Reason, "kanban") && !strings.Contains(r.Reason, "in use")
}

func (s *service) Prune(ctx context.Context, request *worktreesv1.PruneRequest) (*worktreesv1.PruneResponse, error) {
	repo, err := repository(ctx, request.GetDirectory())
	if err != nil {
		return nil, err
	}
	if err := olderThan(request.OlderThan); err != nil {
		return nil, err
	}
	dry, cfg := request.GetDryRun(), s.config
	response := &worktreesv1.PruneResponse{Repo: repo}
	if !request.GetBranchesOnly() {
		var plan []*worktrees.Decision
		if request.GetSweep() {
			settings := s.worktrees.Settings(cfg)
			plan = s.worktrees.Plan(cfg, repo, 24, request.OlderThan)
			for _, d := range plan {
				response.Sweep = append(response.Sweep, decision(d))
			}
			if !dry && !settings.Classic() {
				_, _, archived := s.worktrees.Execute(cfg, repo, plan)
				expired, err := worktrees.Expire(repo, settings.ArchiveRetentionDays, false)
				if err != nil {
					return nil, err
				}
				for _, a := range archived {
					if a.Removed {
						response.Actions = append(response.Actions, "archived + removed → "+a.Ref)
					} else {
						response.Actions = append(response.Actions, fmt.Sprintf("archived, kept (%s) → %s", cmp.Or(a.WhyKept, "?"), a.Ref))
					}
				}
				for _, ref := range expired {
					response.Actions = append(response.Actions, "deleted expired archive "+ref)
				}
			}
			missing, err := worktrees.PruneMissing(repo, dry)
			if err != nil {
				return nil, err
			}
			response.Actions = append(response.Actions, missing...)
		}
		audited, err := s.worktrees.Audit(cfg, repo, false, request.OlderThan)
		if err != nil {
			return nil, err
		}
		records := s.attended(cfg, audited, plan, request.GetKeep(), request.GetSweep())
		response.Actions = append(response.Actions, s.worktrees.Reclaim(cfg, repo, dry, records)...)
		for _, r := range records {
			if preserved(r) {
				response.Preserved = append(response.Preserved, record(r))
			}
		}
	}
	if !request.GetTreesOnly() {
		branches, err := worktrees.Branches(repo)
		if err != nil {
			return nil, err
		}
		actions, err := worktrees.ReclaimBranches(repo, dry, branches)
		if err != nil {
			return nil, err
		}
		response.Actions = append(response.Actions, actions...)
	}
	return response, nil
}

// Trees of an audit the attended reclaim may take: none the user keeps; after a sweep, none it decided on and none a process works in
func (s *service) attended(cfg *config.Config, records []worktrees.TreeRecord, plan []*worktrees.Decision, keep string, sweep bool) []worktrees.TreeRecord {
	var free []worktrees.TreeRecord
	owned := map[string]bool{}
	if sweep && !s.worktrees.Settings(cfg).Classic() {
		for _, d := range plan {
			if d.Action != "skip-young" {
				owned[d.Path] = true
			}
		}
	}
	var paths []string
	for _, r := range records {
		if !owned[r.Path] && (keep == "" || r.Path != keep) {
			free = append(free, r)
			paths = append(paths, r.Path)
		}
	}
	if !sweep || s.worktrees.Settings(cfg).Classic() {
		return free
	}
	live := worktrees.Live(paths)
	var out []worktrees.TreeRecord
	for _, r := range free {
		if len(live[r.Path]) == 0 {
			out = append(out, r)
		}
	}
	return out
}

func (s *service) Restore(ctx context.Context, request *worktreesv1.RestoreRequest) (*worktreesv1.RestoreResponse, error) {
	repo, err := repository(ctx, request.GetDirectory())
	if err != nil {
		return nil, err
	}
	if request.GetName() == "" {
		return nil, connect.NewError(connect.CodeInvalidArgument, "Usage: icebath worktree restore <name> [--path DIR]")
	}
	restored, err := s.worktrees.Restore(ctx, repo, request.GetName(), request.GetPath())
	if failure, ok := errors.AsType[*worktrees.ArchiveError](err); ok {
		return &worktreesv1.RestoreResponse{Error: failure.Error()}, nil
	}
	if err != nil {
		return nil, err
	}
	return &worktreesv1.RestoreResponse{Path: restored.Path, Branch: restored.Branch, Detached: restored.Detached, Head: restored.Head, Ref: restored.Ref,
		Skipped: skipped(restored.Skipped)}, nil
}

func (s *service) Create(ctx context.Context, request *worktreesv1.CreateRequest) (*worktreesv1.CreateResponse, error) {
	response := &worktreesv1.CreateResponse{Repo: git.Root(ctx, request.GetDirectory())}
	say := func(line string) { response.Lines = append(response.Lines, line) }
	if response.Repo == "" && !request.GetLaunch() {
		return response, nil
	}
	cfg := s.config
	if request.GetLaunch() && response.Repo != "" {
		if err := s.worktrees.PruneStale(ctx, cfg, response.Repo, 24); err != nil {
			say("✗ Failed to create the worktree: " + err.Error())
			return response, nil
		}
		go git.Maintain(context.WithoutCancel(ctx), response.Repo)
	}
	made := s.worktrees.Create(ctx, cfg, response.Repo, worktrees.Request{Name: request.GetName(), Owner: int(request.GetPid()),
		Launch: request.GetLaunch()}, cfg.GetBool(worktreesconfig.Sync), say)
	if made != nil {
		response.Tree = tree(*made)
	}
	return response, nil
}

// Settings and record of the session, the profile in use for none
func (s *service) session(ctx context.Context, id string) (*config.Config, sessions.Store, string, error) {
	store, err := s.sessions.Store(s.config.GetString(sessionsconfig.Store))
	if err != nil {
		return nil, nil, "", err
	}
	session, err := store.Get(ctx, id)
	if err != nil {
		return nil, nil, "", err
	}
	return s.config.In(session.GetProfile()), store, session.GetDirectory(), nil
}

func (s *service) Enter(ctx context.Context, request *worktreesv1.EnterRequest) (*worktreesv1.EnterResponse, error) {
	made := from(request.GetTree())
	if !fsx.IsDir(made.Path) {
		return nil, connect.NewError(connect.CodeFailedPrecondition, "The worktree "+made.Path+" is gone.")
	}
	response := &worktreesv1.EnterResponse{Lines: []string{"✅ Worktree ready: " + made.Path, "Branch: " + made.Branch}}
	if request.GetSessionId() == "" {
		s.worktrees.Record(s.config, made)
		response.Lines = append(response.Lines, "New sessions of this window start in the worktree.")
		return response, nil
	}
	cfg, store, _, err := s.session(ctx, request.GetSessionId())
	if err != nil {
		return nil, err
	}
	if err := store.Update(ctx, request.GetSessionId(), sessions.Change{Directory: &made.Path, GitBranch: &made.Branch, GitRoot: &made.Repo}); err != nil {
		return nil, err
	}
	s.worktrees.Record(cfg, made)
	backend, err := s.terminal.Configured(cfg)
	if err != nil {
		return nil, err
	}
	if backend.Container || backend.Remote {
		response.Lines = append(response.Lines, "File tools now work in the worktree; terminal.backend="+backend.Name+" keeps running commands in the folder its "+
			"environment works in, away from this machine.")
		return response, nil
	}
	if s.terminal.Folder(cfg, request.GetSessionId()) != "" {
		lease, err := s.terminal.Acquire(ctx, cfg, request.GetSessionId(), made.Path)
		if err != nil {
			return nil, err
		}
		lease.Record(made.Path)
		lease.Release()
	}
	response.Lines = append(response.Lines, "Terminal and file tools now operate in the worktree.")
	return response, nil
}

func (s *service) State(_ context.Context, request *worktreesv1.StateRequest) (*worktreesv1.StateResponse, error) {
	made := from(request.GetTree())
	state := worktrees.State(made)
	return &worktreesv1.StateResponse{State: state, InUse: state != "" && worktrees.StillInUse(made.Path, 5*time.Second, int(request.GetPid()))}, nil
}

func (s *service) Settle(_ context.Context, request *worktreesv1.SettleRequest) (*worktreesv1.SettleResponse, error) {
	made := from(request.GetTree())
	response := &worktreesv1.SettleResponse{}
	say := func(line string) { response.Lines = append(response.Lines, line) }
	if request.GetLaunch() {
		response.Gone = s.worktrees.Leave(s.config, made, request.GetChoice(), say, int(request.GetPid()))
		return response, nil
	}
	s.worktrees.Settle(s.config, made, request.GetChoice(), say, int(request.GetPid()))
	response.Gone = !fsx.IsDir(made.Path)
	return response, nil
}

func (s *service) Recreate(ctx context.Context, request *worktreesv1.RecreateRequest) (*worktreesv1.RecreateResponse, error) {
	cfg, _, dir, err := s.session(ctx, request.GetSessionId())
	if err != nil {
		return nil, err
	}
	return &worktreesv1.RecreateResponse{Path: s.worktrees.Recreate(cfg, dir, request.GetSessionId())}, nil
}
