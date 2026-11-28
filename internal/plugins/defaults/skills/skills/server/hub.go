package server

import (
	"cmp"
	"context"
	"encoding/json"
	"strings"

	"connectrpc.com/connect/v2"

	hub "github.com/nickheyer/icebath/internal/plugins/defaults/skills/skills/skills"
	skillsv1 "github.com/nickheyer/icebath/pkg/gen/icebath/plugins/skills/v1"
	"github.com/nickheyer/icebath/pkg/gen/icebath/plugins/skills/v1/skillsv1connect"
	"github.com/nickheyer/icebath/pkg/strs"
)

func line(l hub.Line) *skillsv1.Line {
	return &skillsv1.Line{Text: l.Text, Tone: skillsv1.Tone(l.Tone)}
}

func hubSkill(m hub.Meta, installed map[string]bool) *skillsv1.HubSkill {
	return &skillsv1.HubSkill{Name: m.Name, Description: m.Description, Source: m.Source, Identifier: m.Identifier, Trust: m.Trust, Label: m.Label(),
		Tags: m.Tags, Details: hub.Details(m.Extra), Installed: installed[m.Identifier], Category: m.Category()}
}

// Sends each line of an operation as a message of its stream, the first failure to send kept
func lines[R any](stream interface{ Send(R) error }, wrap func(*skillsv1.Line) R, failed *error) hub.Say {
	return func(l hub.Line) {
		if *failed == nil {
			*failed = stream.Send(wrap(line(l)))
		}
	}
}

func (s *service) Search(ctx context.Context, request *skillsv1.SearchRequest) (*skillsv1.SearchResponse, error) {
	scope, err := s.scope(ctx, request.GetScope())
	if err != nil {
		return nil, err
	}
	limit := int(request.GetLimit())
	if limit <= 0 {
		limit = 25
	}
	found, err := s.hub.Search(ctx, scope, request.GetQuery(), cmp.Or(request.GetSource(), "all"), limit)
	if err != nil {
		return nil, err
	}
	installed := s.hub.InstalledIDs(scope)
	response := &skillsv1.SearchResponse{}
	for _, m := range found {
		response.Skills = append(response.Skills, hubSkill(m, installed))
	}
	return response, nil
}

func (s *service) Browse(ctx context.Context, request *skillsv1.BrowseRequest, stream skillsv1connect.SkillsServiceBrowseServerStream) error {
	scope, err := s.scope(ctx, request.GetScope())
	if err != nil {
		return err
	}
	var sent error
	done := func(source string, n int) {
		if sent == nil {
			sent = stream.Send(&skillsv1.BrowseResponse{Answered: &skillsv1.SourceCount{Source: source, Count: int32(n)}})
		}
	}
	size := int(request.GetSize())
	if size == 0 {
		size = 20
	}
	page, err := s.hub.Browse(ctx, scope, max(1, int(request.GetPage())), size, cmp.Or(request.GetSource(), "all"), request.GetScreen(), done)
	if err != nil {
		return err
	}
	if sent != nil {
		return sent
	}
	installed := s.hub.InstalledIDs(scope)
	out := &skillsv1.BrowsePage{Page: int32(page.Page), Pages: int32(page.Pages), Total: int32(page.Total), Start: int32(page.Start),
		TimedOut: page.TimedOut, Official: int32(page.Official)}
	for _, m := range page.Skills {
		out.Skills = append(out.Skills, hubSkill(m, installed))
	}
	for _, c := range page.Counts {
		out.Counts = append(out.Counts, &skillsv1.SourceCount{Source: c.Source, Count: int32(c.N)})
	}
	return stream.Send(&skillsv1.BrowseResponse{Page: out})
}

func (s *service) Inspect(ctx context.Context, request *skillsv1.InspectRequest, stream skillsv1connect.SkillsServiceInspectServerStream) error {
	scope, err := s.scope(ctx, request.GetScope())
	if err != nil {
		return err
	}
	var sent error
	say := lines(stream, func(l *skillsv1.Line) *skillsv1.InspectResponse { return &skillsv1.InspectResponse{Line: l} }, &sent)
	meta, preview, err := s.hub.Inspect(ctx, scope, request.GetIdentifier(), say)
	if err != nil {
		return err
	}
	if sent != nil {
		return sent
	}
	done := &skillsv1.InspectResponse{Done: true, Preview: preview}
	if meta != nil {
		done.Skill = hubSkill(*meta, s.hub.InstalledIDs(scope))
	}
	return stream.Send(done)
}

func pending(p *hub.Pending) *skillsv1.Pending {
	if p == nil {
		return nil
	}
	return &skillsv1.Pending{Id: p.ID, Name: p.Name, Source: p.Source, Official: p.Official, AskCategory: p.AskCategory, Categories: p.Categories,
		Category: p.Category, SkillsDir: p.SkillsDir, Url: p.URL}
}

func outcome(o hub.Outcome) skillsv1.Outcome {
	return map[hub.Outcome]skillsv1.Outcome{hub.Unchanged: skillsv1.Outcome_OUTCOME_UNSPECIFIED, hub.Installed: skillsv1.Outcome_OUTCOME_INSTALLED,
		hub.Staged: skillsv1.Outcome_OUTCOME_STAGED, hub.NeedsName: skillsv1.Outcome_OUTCOME_NEEDS_NAME, hub.Failed: skillsv1.Outcome_OUTCOME_FAILED}[o]
}

func (s *service) Stage(ctx context.Context, request *skillsv1.StageRequest, stream skillsv1connect.SkillsServiceStageServerStream) error {
	scope, err := s.scope(ctx, request.GetScope())
	if err != nil {
		return err
	}
	if request.GetIdentifier() == "" {
		return connect.NewError(connect.CodeInvalidArgument, "Name the skill to install.")
	}
	var sent error
	say := lines(stream, func(l *skillsv1.Line) *skillsv1.StageResponse { return &skillsv1.StageResponse{Line: l} }, &sent)
	result, waiting, err := s.hub.Stage(ctx, scope, hub.Install{Identifier: request.GetIdentifier(), Category: request.GetCategory(), Name: request.GetName(),
		Force: request.GetForce(), Yes: request.GetYes(), Source: request.GetSource()}, say)
	if err != nil {
		return err
	}
	if sent != nil {
		return sent
	}
	return stream.Send(&skillsv1.StageResponse{Done: true, Outcome: outcome(result), Pending: pending(waiting)})
}

func (s *service) Commit(ctx context.Context, request *skillsv1.CommitRequest, stream skillsv1connect.SkillsServiceCommitServerStream) error {
	scope, err := s.scope(ctx, request.GetScope())
	if err != nil {
		return err
	}
	var sent error
	say := lines(stream, func(l *skillsv1.Line) *skillsv1.CommitResponse { return &skillsv1.CommitResponse{Line: l} }, &sent)
	result, err := s.hub.Commit(ctx, scope, request.GetId(), request.GetCategory(), say)
	if err != nil {
		return err
	}
	if sent != nil {
		return sent
	}
	return stream.Send(&skillsv1.CommitResponse{Done: true, Outcome: outcome(result)})
}

func (s *service) Discard(_ context.Context, request *skillsv1.DiscardRequest) (*skillsv1.DiscardResponse, error) {
	s.hub.Discard(request.GetId())
	return &skillsv1.DiscardResponse{}, nil
}

func (s *service) Check(ctx context.Context, request *skillsv1.CheckRequest) (*skillsv1.CheckResponse, error) {
	scope, err := s.scope(ctx, request.GetScope())
	if err != nil {
		return nil, err
	}
	checks, err := s.hub.Check(ctx, scope, request.GetName())
	if err != nil {
		return nil, err
	}
	response := &skillsv1.CheckResponse{}
	for _, c := range checks {
		response.Skills = append(response.Skills, &skillsv1.UpdateCheck{Name: c.Name, Identifier: c.Identifier, Source: c.Source, Status: c.Status})
	}
	return response, nil
}

func (s *service) Update(ctx context.Context, request *skillsv1.UpdateRequest, stream skillsv1connect.SkillsServiceUpdateServerStream) error {
	scope, err := s.scope(ctx, request.GetScope())
	if err != nil {
		return err
	}
	var sent error
	say := lines(stream, func(l *skillsv1.Line) *skillsv1.UpdateResponse { return &skillsv1.UpdateResponse{Line: l} }, &sent)
	ok, err := s.hub.Update(ctx, scope, request.GetName(), request.GetForce(), say)
	if err != nil {
		return err
	}
	if sent != nil {
		return sent
	}
	return stream.Send(&skillsv1.UpdateResponse{Done: true, Ok: ok})
}

func (s *service) Audit(ctx context.Context, request *skillsv1.AuditRequest, stream skillsv1connect.SkillsServiceAuditServerStream) error {
	scope, err := s.scope(ctx, request.GetScope())
	if err != nil {
		return err
	}
	var sent error
	say := lines(stream, func(l *skillsv1.Line) *skillsv1.AuditResponse { return &skillsv1.AuditResponse{Line: l} }, &sent)
	ok, err := s.hub.Audit(ctx, scope, request.GetName(), request.GetDeep(), say)
	if err != nil {
		return err
	}
	if sent != nil {
		return sent
	}
	return stream.Send(&skillsv1.AuditResponse{Done: true, Ok: ok})
}

func (s *service) Uninstall(ctx context.Context, request *skillsv1.UninstallRequest) (*skillsv1.UninstallResponse, error) {
	scope, err := s.scope(ctx, request.GetScope())
	if err != nil {
		return nil, err
	}
	ok, message, err := s.hub.Uninstall(ctx, scope, request.GetName())
	if err != nil {
		return nil, err
	}
	return &skillsv1.UninstallResponse{Ok: ok, Message: message}, nil
}

func (s *service) Publish(ctx context.Context, request *skillsv1.PublishRequest, stream skillsv1connect.SkillsServicePublishServerStream) error {
	scope, err := s.scope(ctx, request.GetScope())
	if err != nil {
		return err
	}
	var sent error
	say := lines(stream, func(l *skillsv1.Line) *skillsv1.PublishResponse { return &skillsv1.PublishResponse{Line: l} }, &sent)
	ok, err := s.hub.Publish(ctx, scope, request.GetPath(), cmp.Or(request.GetTo(), "github"), request.GetRepo(), say)
	if err != nil {
		return err
	}
	if sent != nil {
		return sent
	}
	return stream.Send(&skillsv1.PublishResponse{Done: true, Ok: ok})
}

func (s *service) Export(ctx context.Context, request *skillsv1.ExportRequest) (*skillsv1.ExportResponse, error) {
	scope, err := s.scope(ctx, request.GetScope())
	if err != nil {
		return nil, err
	}
	snapshot, err := s.hub.Export(scope)
	if err != nil {
		return nil, err
	}
	data, err := json.MarshalIndent(snapshot, "", "  ")
	if err != nil {
		return nil, err
	}
	return &skillsv1.ExportResponse{Snapshot: string(data) + "\n", Skills: int32(len(snapshot.Skills)), Taps: int32(len(snapshot.Taps))}, nil
}

func (s *service) Taps(ctx context.Context, request *skillsv1.TapsRequest) (*skillsv1.TapsResponse, error) {
	scope, err := s.scope(ctx, request.GetScope())
	if err != nil {
		return nil, err
	}
	taps, err := s.hub.Taps(scope)
	if err != nil {
		return nil, err
	}
	response := &skillsv1.TapsResponse{}
	for _, t := range taps {
		response.Taps = append(response.Taps, &skillsv1.SkillTap{Repo: t.Repo, Path: t.Path})
	}
	return response, nil
}

func (s *service) Tap(ctx context.Context, request *skillsv1.TapRequest) (*skillsv1.TapResponse, error) {
	scope, err := s.scope(ctx, request.GetScope())
	if err != nil {
		return nil, err
	}
	if request.GetRepo() == "" {
		return nil, connect.NewError(connect.CodeInvalidArgument, "Name the GitHub repository, as owner/repo.")
	}
	changed, err := s.hub.Tap(ctx, scope, request.GetRepo(), request.GetRemove())
	if err != nil {
		return nil, err
	}
	return &skillsv1.TapResponse{Changed: changed}, nil
}

func (s *service) Trust(ctx context.Context, request *skillsv1.TrustRequest) (*skillsv1.TrustResponse, error) {
	scope, err := s.scope(ctx, request.GetScope())
	if err != nil {
		return nil, err
	}
	response := &skillsv1.TrustResponse{}
	result, ok := s.hub.Trust(scope, request.GetPath(), !request.GetUntrust(), func(l hub.Line) { response.Lines = append(response.Lines, line(l)) })
	response.Ok = ok
	switch {
	case !ok:
		for _, l := range response.GetLines() {
			response.Said = strings.TrimSpace(response.Said + " " + l.GetText())
		}
	case request.GetUntrust() && result.Changed:
		response.Said = result.Root + " is no longer trusted. Its skills no longer load."
	case request.GetUntrust():
		response.Said = "You had not trusted " + result.Root + ", so nothing changed."
	case !result.Changed:
		response.Said = "You trust " + result.Root + " already."
	case result.Skills == 0:
		response.Said = "Trusted " + result.Root + ". It keeps no skills of its own yet."
	case result.Skills == 1:
		response.Said = "Trusted " + result.Root + ". Its skill loads in sessions working in it."
	default:
		response.Said = "Trusted " + result.Root + ". Its " + strs.Count(result.Skills, "skill") + " load in sessions working in it."
	}
	return response, nil
}
