package server

import (
	"cmp"
	"context"
	"errors"
	"path/filepath"
	"slices"
	"strings"
	"unicode"
	"unicode/utf8"

	"connectrpc.com/connect/v2"

	"github.com/nickheyer/icebath/internal/auxiliary"
	"github.com/nickheyer/icebath/internal/config"
	"github.com/nickheyer/icebath/internal/credentials"
	"github.com/nickheyer/icebath/internal/prompt"
	"github.com/nickheyer/icebath/internal/provider"
	"github.com/nickheyer/icebath/internal/server"
	"github.com/nickheyer/icebath/internal/sessions"
	sessionsconfig "github.com/nickheyer/icebath/internal/sessions/config"
	"github.com/nickheyer/icebath/internal/transport"
	sessionsv1 "github.com/nickheyer/icebath/pkg/gen/icebath/sessions/v1"
	"github.com/nickheyer/icebath/pkg/gen/icebath/sessions/v1/sessionsv1connect"
)

type service struct {
	config   *config.Config
	sessions *sessions.Sessions
	prompts  *prompt.Prompt
	// What helper model calls are made with, for titling sessions again
	tasks       *auxiliary.Registry
	providers   *provider.Registry
	transports  *transport.Registry
	credentials *credentials.Registry
	learned     *auxiliary.State
}

func Register(host *server.Server, cfg *config.Config, s *sessions.Sessions, prompts *prompt.Prompt, tasks *auxiliary.Registry, providers *provider.Registry,
	transports *transport.Registry, creds *credentials.Registry) {
	host.Register(&service{config: cfg, sessions: s, prompts: prompts, tasks: tasks, providers: providers, transports: transports, credentials: creds,
		learned: auxiliary.NewState()})
}

func (s *service) Mount(services *connect.Server) {
	sessionsv1connect.RegisterSessionServiceHandler(services, s)
}

func (s *service) store() (sessions.Store, error) {
	return s.sessions.Current()
}

// Error a caller reads: a refusal of the store as a failed precondition rather than an internal error
func failure(err error) error {
	if err == nil {
		return nil
	}
	if _, ok := errors.AsType[*connect.Error](err); ok {
		return err
	}
	return connect.NewError(connect.CodeFailedPrecondition, err.Error())
}

func (s *service) Create(ctx context.Context, request *sessionsv1.CreateRequest) (*sessionsv1.CreateResponse, error) {
	if !filepath.IsAbs(request.GetDirectory()) {
		return nil, connect.NewError(connect.CodeInvalidArgument, "The folder of a new session must be a full path, such as /home/you/project.")
	}
	store, err := s.store()
	if err != nil {
		return nil, err
	}
	profile := cmp.Or(request.GetProfile(), s.config.Profile())
	if !s.config.Exists(profile) {
		return nil, connect.NewError(connect.CodeInvalidArgument, "There is no profile named "+profile+". Run icebath profile list to see your profiles.")
	}
	session, err := store.Create(ctx, &sessionsv1.Session{Directory: request.GetDirectory(), Profile: profile, Source: request.GetSource(),
		Title: request.GetTitle(), ParentId: request.GetParentId()})
	if err != nil {
		return nil, failure(err)
	}
	return &sessionsv1.CreateResponse{Session: session}, nil
}

func (s *service) List(ctx context.Context, request *sessionsv1.ListRequest) (*sessionsv1.ListResponse, error) {
	store, err := s.store()
	if err != nil {
		return nil, err
	}
	list, more, err := store.List(ctx, request.GetFilter())
	if err != nil {
		return nil, failure(err)
	}
	return &sessionsv1.ListResponse{Sessions: list, More: more}, nil
}

func (s *service) Get(ctx context.Context, request *sessionsv1.GetRequest) (*sessionsv1.GetResponse, error) {
	store, err := s.store()
	if err != nil {
		return nil, err
	}
	session, err := store.Get(ctx, request.GetId())
	if err != nil {
		return nil, err
	}
	messages, err := store.Messages(ctx, request.GetId(), sessions.Display)
	if err != nil {
		return nil, failure(err)
	}
	return &sessionsv1.GetResponse{Session: session, Messages: messages}, nil
}

func (s *service) Resolve(ctx context.Context, request *sessionsv1.ResolveRequest) (*sessionsv1.ResolveResponse, error) {
	store, err := s.store()
	if err != nil {
		return nil, err
	}
	session, err := store.Resolve(ctx, cmp.Or(request.GetProfile(), s.config.Profile()), request.GetRef())
	if err != nil {
		return nil, err
	}
	if request.GetTip() {
		tip, err := store.Tip(ctx, session.GetId())
		if err != nil {
			return nil, failure(err)
		}
		if tip != session.GetId() {
			if session, err = store.Get(ctx, tip); err != nil {
				return nil, failure(err)
			}
		}
	}
	return &sessionsv1.ResolveResponse{Session: session}, nil
}

func (s *service) Rename(ctx context.Context, request *sessionsv1.RenameRequest) (*sessionsv1.RenameResponse, error) {
	store, err := s.store()
	if err != nil {
		return nil, err
	}
	title, err := store.SetTitle(ctx, request.GetId(), request.GetTitle(), sessions.TitleUser)
	if err != nil {
		return nil, failure(err)
	}
	return &sessionsv1.RenameResponse{Title: title}, nil
}

func (s *service) Delete(ctx context.Context, request *sessionsv1.DeleteRequest) (*sessionsv1.DeleteResponse, error) {
	store, err := s.store()
	if err != nil {
		return nil, err
	}
	ids, err := store.Delete(ctx, request.GetId())
	if err != nil {
		return nil, failure(err)
	}
	for _, id := range ids {
		s.sessions.Release(ctx, id)
	}
	return &sessionsv1.DeleteResponse{Ids: ids}, nil
}

func (s *service) Mark(ctx context.Context, request *sessionsv1.MarkRequest) (*sessionsv1.MarkResponse, error) {
	store, err := s.store()
	if err != nil {
		return nil, err
	}
	session, err := store.Mark(ctx, request)
	if err != nil {
		return nil, failure(err)
	}
	return &sessionsv1.MarkResponse{Session: session}, nil
}

func (s *service) Branch(ctx context.Context, request *sessionsv1.BranchRequest) (*sessionsv1.BranchResponse, error) {
	store, err := s.store()
	if err != nil {
		return nil, err
	}
	session, err := store.Branch(ctx, request)
	if err != nil {
		return nil, failure(err)
	}
	return &sessionsv1.BranchResponse{Session: session}, nil
}

func (s *service) Search(ctx context.Context, request *sessionsv1.SearchRequest) (*sessionsv1.SearchResponse, error) {
	store, err := s.store()
	if err != nil {
		return nil, err
	}
	hits, err := store.Search(ctx, request)
	if err != nil {
		return nil, failure(err)
	}
	return &sessionsv1.SearchResponse{Hits: hits}, nil
}

func (s *service) Prompts(ctx context.Context, request *sessionsv1.PromptsRequest) (*sessionsv1.PromptsResponse, error) {
	store, err := s.store()
	if err != nil {
		return nil, err
	}
	prompts, err := store.Prompts(ctx, request.GetId(), cmp.Or(int(request.GetLimit()), 50))
	if err != nil {
		return nil, failure(err)
	}
	return &sessionsv1.PromptsResponse{Prompts: prompts}, nil
}

func (s *service) Export(ctx context.Context, request *sessionsv1.ExportRequest) (*sessionsv1.ExportResponse, error) {
	store, err := s.store()
	if err != nil {
		return nil, err
	}
	limit := int64(s.config.GetInt(sessionsconfig.MaxExportMessages))
	capped := request.GetCapped() && limit > 0
	// A session too long to build in memory is refused from its count, before any of it is read
	if capped {
		listed, _, err := store.List(ctx, request.GetFilter())
		if err != nil {
			return nil, failure(err)
		}
		for _, session := range listed {
			if session.GetMessages() > limit {
				return nil, failure(sessions.TooLarge(session.GetId(), limit))
			}
		}
	}
	transcripts, err := store.Export(ctx, request)
	if err != nil {
		return nil, failure(err)
	}
	if capped {
		for _, transcript := range transcripts {
			if int64(len(transcript.GetMessages())) > limit {
				return nil, failure(sessions.TooLarge(transcript.GetSession().GetId(), limit))
			}
		}
	}
	return &sessionsv1.ExportResponse{Transcripts: transcripts}, nil
}

func (s *service) Import(ctx context.Context, request *sessionsv1.ImportRequest) (*sessionsv1.ImportResponse, error) {
	store, err := s.store()
	if err != nil {
		return nil, err
	}
	for _, transcript := range request.GetTranscripts() {
		if transcript.GetSession().GetProfile() == "" {
			transcript.Session.Profile = s.config.Profile()
		}
	}
	response, err := store.Import(ctx, request.GetTranscripts())
	return response, failure(err)
}

func (s *service) Prune(ctx context.Context, request *sessionsv1.PruneRequest) (*sessionsv1.PruneResponse, error) {
	store, err := s.store()
	if err != nil {
		return nil, err
	}
	response, err := store.Prune(ctx, request)
	if err == nil && !request.GetDryRun() {
		for _, session := range response.GetSessions() {
			s.sessions.Release(ctx, session.GetId())
		}
	}
	return response, failure(err)
}

func (s *service) Archive(ctx context.Context, request *sessionsv1.ArchiveRequest) (*sessionsv1.ArchiveResponse, error) {
	store, err := s.store()
	if err != nil {
		return nil, err
	}
	response, err := store.Archive(ctx, request.GetFilter(), request.GetDryRun())
	return response, failure(err)
}

func (s *service) Stats(ctx context.Context, _ *sessionsv1.StatsRequest) (*sessionsv1.StatsResponse, error) {
	store, err := s.store()
	if err != nil {
		return nil, err
	}
	response, err := store.Stats(ctx)
	return response, failure(err)
}

func (s *service) Maintain(ctx context.Context, request *sessionsv1.MaintainRequest, stream sessionsv1connect.SessionServiceMaintainServerStream) error {
	store, err := s.store()
	if err != nil {
		return err
	}
	var sendErr error
	err = store.Maintain(ctx, request, func(line string) {
		if sendErr == nil {
			sendErr = stream.Send(&sessionsv1.MaintainResponse{Line: line})
		}
	})
	if sendErr != nil {
		return sendErr
	}
	if err != nil {
		return stream.Send(&sessionsv1.MaintainResponse{Line: err.Error(), Done: true, Failed: true})
	}
	return stream.Send(&sessionsv1.MaintainResponse{Done: true})
}

func (s *service) RefreshPrompts(ctx context.Context, request *sessionsv1.RefreshPromptsRequest) (*sessionsv1.RefreshPromptsResponse, error) {
	store, err := s.store()
	if err != nil {
		return nil, err
	}
	id := request.GetSessionId()
	if id != "" {
		session, err := store.Resolve(ctx, s.config.Profile(), id)
		if err != nil {
			return nil, connect.NewError(connect.CodeNotFound, "There is no session "+id+". Run icebath sessions list to find its id.")
		}
		id = session.GetId()
	}
	held, err := store.RefreshPrompts(ctx, id)
	if err != nil {
		return nil, failure(err)
	}
	if _, _, err := s.prompts.Refresh(id); err != nil && !errors.Is(err, prompt.ErrNoKeeper) {
		return nil, failure(err)
	}
	return &sessionsv1.RefreshPromptsResponse{SessionId: id, Sessions: held}, nil
}

func (s *service) RepairPrompts(ctx context.Context, request *sessionsv1.RepairPromptsRequest) (*sessionsv1.RepairPromptsResponse, error) {
	store, err := s.store()
	if err != nil {
		return nil, err
	}
	if id := request.GetSessionId(); id != "" {
		session, err := store.Resolve(ctx, s.config.Profile(), id)
		if err != nil {
			return nil, connect.NewError(connect.CodeNotFound, "There is no session "+id+". Run icebath sessions list to find its id.")
		}
		request.SessionId = session.GetId()
	}
	response, err := store.RepairPrompts(ctx, request, func(tools []string) []string { return s.guidance(ctx, tools) })
	return response, failure(err)
}

// Sections of the system prompt the tools bring: those a prompt built with them has and one built without them lacks
func (s *service) guidance(ctx context.Context, tools []string) []string {
	without, err := s.prompts.Sections(ctx, &prompt.Env{Config: s.config})
	if err != nil {
		return nil
	}
	with, err := s.prompts.Sections(ctx, &prompt.Env{Config: s.config, Tools: tools})
	if err != nil {
		return nil
	}
	var out []string
	for _, section := range with {
		text := strings.TrimSpace(section.Text)
		if text != "" && !slices.ContainsFunc(without, func(other prompt.Section) bool { return strings.TrimSpace(other.Text) == text }) {
			out = append(out, text)
		}
	}
	return out
}

func (s *service) RepairRouting(ctx context.Context, request *sessionsv1.RepairRoutingRequest) (*sessionsv1.RepairRoutingResponse, error) {
	store, err := s.store()
	if err != nil {
		return nil, err
	}
	response, err := store.RepairRouting(ctx, request)
	return response, failure(err)
}

func (s *service) RepairProfiles(ctx context.Context, request *sessionsv1.RepairProfilesRequest) (*sessionsv1.RepairProfilesResponse, error) {
	store, err := s.store()
	if err != nil {
		return nil, err
	}
	response, err := store.RepairProfiles(ctx, request, s.config.Exists)
	return response, failure(err)
}

func (s *service) Lost(ctx context.Context, request *sessionsv1.LostRequest) (*sessionsv1.LostResponse, error) {
	store, err := s.store()
	if err != nil {
		return nil, err
	}
	session, err := store.Resolve(ctx, s.config.Profile(), request.GetSession())
	if err != nil {
		return nil, connect.NewError(connect.CodeNotFound, "No session matches “"+request.GetSession()+"”. Run icebath sessions list to see your sessions.")
	}
	pending, err := store.Pending(ctx, session.GetId())
	if err != nil {
		return nil, failure(err)
	}
	response := &sessionsv1.LostResponse{SessionId: session.GetId()}
	for _, admission := range pending {
		if admission.GetStatus() == "unknown" {
			response.Admissions = append(response.Admissions, admission)
		}
	}
	return response, nil
}

func (s *service) Discard(ctx context.Context, request *sessionsv1.DiscardRequest) (*sessionsv1.DiscardResponse, error) {
	store, err := s.store()
	if err != nil {
		return nil, err
	}
	session, err := store.Resolve(ctx, s.config.Profile(), request.GetSession())
	if err != nil {
		return nil, connect.NewError(connect.CodeNotFound, "No session matches “"+request.GetSession()+"”. Run icebath sessions list to see your sessions.")
	}
	discarded, err := store.Discard(ctx, session.GetId(), request.GetAdmissions())
	if err != nil {
		return nil, failure(err)
	}
	return &sessionsv1.DiscardResponse{SessionId: session.GetId(), Discarded: discarded}, nil
}

// Sessions whose first message invoked a skill, titled again from what the user typed alongside it
func (s *service) RetitleSkills(ctx context.Context, request *sessionsv1.RetitleSkillsRequest) (*sessionsv1.RetitleSkillsResponse, error) {
	store, err := s.store()
	if err != nil {
		return nil, err
	}
	limit := max(int(cmp.Or(request.GetLimit(), 200)), 1)
	list, _, err := store.List(ctx, &sessionsv1.Filter{IncludeArchived: true, IncludeHidden: true, IncludeChildren: true, ByStart: true})
	if err != nil {
		return nil, failure(err)
	}
	response := &sessionsv1.RetitleSkillsResponse{}
	for _, session := range list {
		if int(response.GetCandidates()) >= limit {
			break
		}
		if session.GetTitle() == "" {
			continue
		}
		messages, err := store.Messages(ctx, session.GetId(), sessions.Display)
		if err != nil {
			return nil, failure(err)
		}
		opener := ""
		for _, m := range messages {
			if m.GetRole() == sessionsv1.Role_ROLE_USER {
				opener = m.GetText()
				break
			}
		}
		if !strings.HasPrefix(opener, sessions.SkillPrefix) && !strings.HasPrefix(opener, sessions.AutoSkillPrefix) {
			continue
		}
		response.Candidates++
		typed, _ := sessions.DescribeSkill(opener, " — ")
		aux := &auxiliary.Client{Tasks: s.tasks, Config: s.config.In(session.GetProfile()), Providers: s.providers, Transports: s.transports,
			Credentials: s.credentials, State: s.learned}
		title, _, err := sessions.GenerateTitle(ctx, aux, sessions.TitleRequest{Message: typed})
		if err != nil || title == "" || title == session.GetTitle() {
			continue
		}
		retitle := &sessionsv1.Retitle{Id: session.GetId(), OldTitle: session.GetTitle(), NewTitle: title}
		response.Retitles = append(response.Retitles, retitle)
		if first, _ := utf8.DecodeRuneInString(title); !unicode.IsLetter(first) && !unicode.IsDigit(first) {
			retitle.Kept = true
			continue
		}
		if !request.GetApply() {
			continue
		}
		if _, err := store.SetTitle(ctx, session.GetId(), title, sessions.TitleUser); err != nil {
			next, nextErr := store.NextTitle(ctx, session.GetProfile(), title)
			if nextErr == nil && next != title {
				if _, err = store.SetTitle(ctx, session.GetId(), next, sessions.TitleUser); err == nil {
					retitle.Renamed = next
				}
			}
			if err != nil {
				retitle.Error = err.Error()
			}
		}
	}
	return response, nil
}

func (s *service) Notices(context.Context, *sessionsv1.NoticesRequest) (*sessionsv1.NoticesResponse, error) {
	return &sessionsv1.NoticesResponse{Notices: s.sessions.Notices()}, nil
}

func (s *service) Watch(ctx context.Context, _ *sessionsv1.WatchRequest, stream sessionsv1connect.SessionServiceWatchServerStream) error {
	store, err := s.store()
	if err != nil {
		return err
	}
	changes, stop := store.Subscribe()
	defer stop()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case ids := <-changes:
			if err := stream.Send(&sessionsv1.WatchResponse{Ids: ids}); err != nil {
				return err
			}
		}
	}
}
