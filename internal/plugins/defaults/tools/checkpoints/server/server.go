package server

import (
	"context"
	"path/filepath"
	"strings"
	"time"

	"connectrpc.com/connect/v2"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/nickheyer/icebath/internal/checkpoints"
	"github.com/nickheyer/icebath/internal/config"
	"github.com/nickheyer/icebath/internal/server"
	"github.com/nickheyer/icebath/internal/sessions"
	sessionsconfig "github.com/nickheyer/icebath/internal/sessions/config"
	"github.com/nickheyer/icebath/internal/terminal"
	checkpointsv1 "github.com/nickheyer/icebath/pkg/gen/icebath/plugins/checkpoints/v1"
	"github.com/nickheyer/icebath/pkg/gen/icebath/plugins/checkpoints/v1/checkpointsv1connect"
	"github.com/nickheyer/icebath/pkg/strs"
)

type service struct {
	config   *config.Config
	sessions *sessions.Registry
	terminal *terminal.Terminal
}

func Register(registry *server.Registry, cfg *config.Config, stores *sessions.Registry, host *terminal.Terminal) {
	registry.Register(&service{config: cfg, sessions: stores, terminal: host})
}

func (s *service) Mount(services *connect.Server) {
	checkpointsv1connect.RegisterCheckpointServiceHandler(services, s)
}

// Checkpoints as the session sees them
func (s *service) scope(ctx context.Context, id string) (*checkpoints.Scope, error) {
	return Scope(ctx, s.config, s.sessions, s.terminal, id)
}

// Checkpoints as a session sees them: its profile's, of the project its folder lies in
func Scope(ctx context.Context, cfg *config.Config, stores *sessions.Registry, host *terminal.Terminal, id string) (*checkpoints.Scope, error) {
	if id == "" {
		return nil, connect.NewError(connect.CodeFailedPrecondition, "No session is open, so there are no checkpoints to restore.")
	}
	store, err := stores.Store(cfg.GetString(sessionsconfig.Store))
	if err != nil {
		return nil, err
	}
	session, err := store.Get(ctx, id)
	if err != nil {
		return nil, err
	}
	return checkpoints.For(sessions.Settings(cfg, session), session.GetDirectory(), host), nil
}

func stamp(when time.Time) *timestamppb.Timestamp {
	if when.IsZero() {
		return nil
	}
	return timestamppb.New(when)
}

func (s *service) List(ctx context.Context, request *checkpointsv1.ListRequest) (*checkpointsv1.ListResponse, error) {
	scope, err := s.scope(ctx, request.GetSessionId())
	if err != nil {
		return nil, err
	}
	response := &checkpointsv1.ListResponse{Enabled: scope.Enabled(), Refusal: scope.Refusal, Directory: scope.Directory}
	if scope.Profile != s.config.Profile() {
		response.Profile = scope.Profile
	}
	if !scope.Enabled() {
		return response, nil
	}
	for _, entry := range scope.List(scope.Directory) {
		response.Checkpoints = append(response.Checkpoints, &checkpointsv1.Checkpoint{Hash: entry.Hash, Short: entry.Short, When: stamp(entry.When),
			Reason: entry.Reason, Files: int32(entry.Files), Insertions: int32(entry.Insertions), Deletions: int32(entry.Deletions)})
	}
	return response, nil
}

// Why snapshots can't serve the session: they are off, or its backend is one they don't serve; nil when they can
func refuse(scope *checkpoints.Scope) error {
	if scope.Refusal != "" {
		return connect.NewError(connect.CodeFailedPrecondition, scope.Refusal)
	}
	if !scope.Enabled() {
		return connect.NewError(connect.CodeFailedPrecondition, checkpoints.NotEnabled)
	}
	return nil
}

// Snapshot of the session's project the reference names, refused when snapshots can't serve the session
func target(scope *checkpoints.Scope, reference string) (string, error) {
	if err := refuse(scope); err != nil {
		return "", err
	}
	sha, err := scope.Resolve(scope.Directory, reference)
	if err != nil {
		return "", connect.NewError(connect.CodeInvalidArgument, err.Error())
	}
	return sha, nil
}

func (s *service) Diff(ctx context.Context, request *checkpointsv1.DiffRequest) (*checkpointsv1.DiffResponse, error) {
	scope, err := s.scope(ctx, request.GetSessionId())
	if err != nil {
		return nil, err
	}
	diff, sha, err := Diff(scope, request.GetCheckpoint())
	if err != nil {
		return nil, err
	}
	return &checkpointsv1.DiffResponse{Hash: sha, Stat: diff.Stat, Diff: diff.Patch}, nil
}

// What changed in the session's project since the checkpoint the reference names, or since its oldest kept one, the session's baseline,
// for none; and the snapshot compared with
func Diff(scope *checkpoints.Scope, reference string) (*checkpoints.Diff, string, error) {
	if reference == "" {
		if err := refuse(scope); err != nil {
			return nil, "", err
		}
		diff, err := scope.SessionDiff(scope.Directory)
		if err != nil {
			return nil, "", connect.NewError(connect.CodeFailedPrecondition, err.Error())
		}
		return diff, diff.Baseline, nil
	}
	sha, err := target(scope, reference)
	if err != nil {
		return nil, "", err
	}
	diff, err := scope.Diff(scope.Directory, sha)
	if err != nil {
		return nil, "", connect.NewError(connect.CodeFailedPrecondition, err.Error())
	}
	return diff, sha, nil
}

func (s *service) Restore(ctx context.Context, request *checkpointsv1.RestoreRequest) (*checkpointsv1.RestoreResponse, error) {
	scope, err := s.scope(ctx, request.GetSessionId())
	if err != nil {
		return nil, err
	}
	sha, err := target(scope, request.GetCheckpoint())
	if err != nil {
		return nil, err
	}
	restored, err := scope.Restore(scope.Directory, sha, request.GetFile(), request.GetFile() == "" && !request.GetAll())
	if err != nil {
		return nil, connect.NewError(connect.CodeFailedPrecondition, err.Error())
	}
	return Restored(restored), nil
}

// A restore as the service answers it
func Restored(restored *checkpoints.Restored) *checkpointsv1.RestoreResponse {
	return &checkpointsv1.RestoreResponse{RestoredTo: restored.To, Reason: restored.Reason, Directory: restored.Directory, File: restored.File,
		Safe: restored.Safe, Restored: restored.Files, KeptEdits: restored.Edits, KeptOversize: restored.Oversize, FailedDeletes: restored.FailedDeletes}
}

func (s *service) Status(_ context.Context, request *checkpointsv1.StatusRequest) (*checkpointsv1.StatusResponse, error) {
	cfg, err := server.Profile(s.config, request.GetProfile())
	if err != nil {
		return nil, err
	}
	status := checkpoints.Open(cfg).Status()
	response := &checkpointsv1.StatusResponse{Base: status.Base, Exists: status.Exists, StoreBytes: status.StoreBytes,
		LegacyBytes: status.LegacyBytes, TotalBytes: status.TotalBytes, Footprint: checkpoints.Footprint(cfg), Profile: cfg.Profile()}
	for _, project := range status.Projects {
		response.Projects = append(response.Projects, &checkpointsv1.Project{Hash: project.Hash, Workdir: project.Workdir, Exists: project.Exists,
			Created: stamp(project.Created), Touched: stamp(project.Touched), Commits: int32(project.Commits)})
	}
	for _, archive := range status.Archives {
		response.Archives = append(response.Archives, &checkpointsv1.Archive{Name: archive.Name, Bytes: archive.Bytes, Modified: stamp(archive.Modified)})
	}
	return response, nil
}

func (s *service) Prune(_ context.Context, request *checkpointsv1.PruneRequest) (*checkpointsv1.PruneResponse, error) {
	cfg, err := server.Profile(s.config, request.GetProfile())
	if err != nil {
		return nil, err
	}
	options := checkpoints.PruneOptions{Retention: time.Duration(request.GetRetentionDays()) * 24 * time.Hour, Orphans: request.GetOrphans(),
		MaxTotalMB: int(request.GetMaxSizeMb())}
	if request.GetAllowListed() {
		options.Allow = map[string]bool{}
		for _, hash := range request.GetAllow() {
			options.Allow[hash] = true
		}
	}
	pruned, err := checkpoints.Open(cfg).Prune(options)
	if err != nil {
		return nil, err
	}
	return &checkpointsv1.PruneResponse{Scanned: int32(pruned.Scanned), Orphans: int32(pruned.Orphans), Stale: int32(pruned.Stale),
		Errors: int32(pruned.Errors), Freed: pruned.Freed}, nil
}

func (s *service) Clear(_ context.Context, request *checkpointsv1.ClearRequest) (*checkpointsv1.ClearResponse, error) {
	cfg, err := server.Profile(s.config, request.GetProfile())
	if err != nil {
		return nil, err
	}
	freed, err := checkpoints.Open(cfg).Clear()
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, "Could not delete the checkpoints: "+err.Error()+".")
	}
	return &checkpointsv1.ClearResponse{Freed: freed}, nil
}

func (s *service) ClearLegacy(_ context.Context, request *checkpointsv1.ClearLegacyRequest) (*checkpointsv1.ClearLegacyResponse, error) {
	cfg, err := server.Profile(s.config, request.GetProfile())
	if err != nil {
		return nil, err
	}
	cleared := checkpoints.Open(cfg).ClearLegacy()
	return &checkpointsv1.ClearLegacyResponse{Deleted: int32(cleared.Deleted), Freed: cleared.Freed, Errors: int32(cleared.Errors)}, nil
}

// What a restore did as the user reads it, line by line
func Report(restored *checkpointsv1.RestoreResponse) []string {
	if restored.GetSafe() && len(restored.GetRestored()) == 0 && len(restored.GetKeptEdits()) > 0 {
		return []string{"Nothing was restored, because you changed every one of those files yourself since the checkpoint: " +
			strs.AndMore(restored.GetKeptEdits(), 6) + ".", "Run /rollback <N> --all to restore them anyway."}
	}
	target := "your files"
	if restored.GetFile() != "" {
		target = filepath.ToSlash(restored.GetFile())
	}
	head := "✓ Restored " + target + " to checkpoint " + restored.GetRestoredTo()
	if reason := strings.TrimSpace(restored.GetReason()); reason != "" {
		head += ", “" + reason + "”"
	}
	lines := []string{head + ".", "A checkpoint of how they were just before was saved first, so you can undo this."}
	if edits := restored.GetKeptEdits(); len(edits) > 0 {
		lines = append(lines, "Kept the changes you made yourself to "+strs.AndMore(edits, 6)+".", "Run /rollback <N> --all to restore those files too.")
	}
	if oversize := restored.GetKeptOversize(); len(oversize) > 0 {
		lines = append(lines, "Kept "+strs.AndMore(oversize, 6)+" as they are, because they are too large for checkpoints and no copy of them was saved.")
	}
	if failed := restored.GetFailedDeletes(); len(failed) > 0 {
		lines = append(lines, "⚠ Could not delete "+strs.AndMore(failed, 6)+", so they are still there.")
	}
	return lines
}
