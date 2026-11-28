package sessions

import (
	"context"
	"errors"
	"fmt"
	"io"
	"maps"
	"slices"
	"sync"
	"time"

	"connectrpc.com/connect/v2"

	"github.com/nickheyer/icebath/internal/config"
	"github.com/nickheyer/icebath/internal/hooks"
	sessionsconfig "github.com/nickheyer/icebath/internal/sessions/config"
	sessionsv1 "github.com/nickheyer/icebath/pkg/gen/icebath/sessions/v1"
	"github.com/nickheyer/icebath/pkg/strs"
)

var ErrNotFound = connect.NewError(connect.CodeNotFound, "There is no such session. Run icebath sessions list to see your sessions.")

// Who set a title, least authority first: a title is only replaced by one of the same authority or more, a person's always
const (
	TitleDerived = "derived"
	TitleLLM     = "llm"
	TitleUser    = "user"
)

// Why a session ended
const (
	// The person started a new session in its window
	EndNewSession = "new_session"
	// It went on in a child session holding its summary
	EndCompression = "compression"
	// A process that died left it open
	EndOrphan = "startup_orphan_reap"
	// Its last window went away and it sat idle
	EndDisconnected = "ws_orphan_reap"
	// The server closed it to stay under its live session limit, or as it stopped
	EndClosed = "agent_close"
	// The person closed it from the sessions screen
	EndUserClosed = "tui_close"
	// A messaging chat was reset, switched to another session, or its session expired after sitting idle, a day, or suspended
	EndReset         = "session_reset"
	EndSwitch        = "session_switch"
	EndIdle          = "idle"
	EndDaily         = "daily"
	EndSuspended     = "suspended"
	EndResumeExpired = "resume_pending_expired"
)

// Whether a session that ended for the reason was left on purpose, its conversation over: a new session or a reset chat
func Boundary(reason string) bool {
	switch reason {
	case EndNewSession, EndReset, EndSwitch, EndIdle, EndDaily, EndSuspended, EndResumeExpired:
		return true
	}
	return false
}

// Title of the hidden session a bot's chat lives in, whose name is its identity: it is never renamed and keeps its title from others
const BotChat = "Bot Chat"

// Messages of a session a read returns
type View int

const (
	// What the model is sent: the active messages
	Model View = iota
	// What the person sees: the active messages and those compaction folded away
	Display
	// Every message, rewound ones too
	All
)

// Change of a session's record; nil fields are left as they are
type Change struct {
	Model, Provider, BaseURL *string
	// Working directory and its git facts
	Directory, GitBranch, GitRoot *string
	// What the session is doing now, which also moves its last activity; empty clears it
	Activity *string
	Yolo     *bool
	// Keys merged into the model settings of the session, a nil value removing its key
	ModelConfig map[string]any
	Peer        *sessionsv1.Peer
	Source      *string
}

// Who may write a session's transcript: the holder of its turn lease, a compressor holding its lock
type Guard struct {
	Turn        string
	Compression string
}

// Messages around one of a session's messages
type Window struct {
	Messages      []*sessionsv1.Message
	Before, After int
	// Messages of the session before and after the window, nearest last and first, when asked for
	Start, End []*sessionsv1.Message
}

// Process serving sessions out of the store, whose beat says it is alive
type Backend struct {
	ID      string
	PID     int
	Started time.Time
	Profile string
	Host    string
}

// Store of sessions and their messages, shared by every process that opens it
type Store interface {
	Records
	Transcripts
	Prompts
	Upkeep
	Coordination
	Admissions
	Compression
	Gateway
	// Tells the channel the ids of sessions as they are made, changed or deleted, none for when anything may have changed, until stop
	Subscribe() (changes <-chan []string, stop func())
}

// Session records
type Records interface {
	Create(ctx context.Context, session *sessionsv1.Session) (*sessionsv1.Session, error)
	Get(ctx context.Context, id string) (*sessionsv1.Session, error)
	// Session an id, the unique start of one, or a title of the profile's names; a title is followed to its newest numbered continuation.
	// Titles are each profile's own, so an empty profile takes the newest session of any that holds it
	Resolve(ctx context.Context, profile, ref string) (*sessionsv1.Session, error)
	// Session of the profile a title alone names, followed to its newest numbered continuation; any profile's when none is given
	Titled(ctx context.Context, profile, title string) (*sessionsv1.Session, error)
	// Sessions the filter matches, and whether more do than its limit let through
	List(ctx context.Context, filter *sessionsv1.Filter) ([]*sessionsv1.Session, bool, error)
	// Sets the title at the authority of its source, returning the title stored: empty when one of more authority holds the session
	SetTitle(ctx context.Context, id, title, source string) (string, error)
	// Title after the base not yet taken among the profile's sessions: the base itself, else the base numbered one past its highest
	NextTitle(ctx context.Context, profile, base string) (string, error)
	// Deletes the session, the sessions it continued and those it delegated to, returning them all
	Delete(ctx context.Context, id string) ([]string, error)
	Mark(ctx context.Context, mark *sessionsv1.MarkRequest) (*sessionsv1.Session, error)
	Update(ctx context.Context, id string, change Change) error
	// Runs the session on another profile from now on
	Move(ctx context.Context, id, profile string) error
	// Deletes every session run on the profile, and what the gateway keeps of its chats
	Clear(ctx context.Context, profile string) error
	// Moves every session of a renamed profile, the keys of its chats and its routes to its new name
	Rekey(ctx context.Context, old, renamed string) error
	// Marks the session ended, keeping the first reason it was ended for
	End(ctx context.Context, id, reason string) error
	Reopen(ctx context.Context, id string) error
	Branch(ctx context.Context, branch *sessionsv1.BranchRequest) (*sessionsv1.Session, error)
	RecordUsage(ctx context.Context, id, model, task string, usage *sessionsv1.Usage) error
}

// Messages of sessions
type Transcripts interface {
	Messages(ctx context.Context, id string, view View) ([]*sessionsv1.Message, error)
	// Stores the messages at the end of the session, giving each its id, time and uid
	Append(ctx context.Context, id string, guard Guard, messages ...*sessionsv1.Message) error
	// Takes back the user's message and everything after it, returning how many messages went
	Rewind(ctx context.Context, id string, message int64) (int, error)
	Around(ctx context.Context, id string, message int64, window, bookend int, roles []sessionsv1.Role) (Window, error)
	// Messages the user sent, newest first
	Prompts(ctx context.Context, id string, limit int) ([]*sessionsv1.Prompt, error)
	Search(ctx context.Context, search *sessionsv1.SearchRequest) ([]*sessionsv1.Hit, error)
	// Rebuild of the search index under way, nil when it is complete: searches meanwhile scan the messages it has yet to reach
	Indexing(ctx context.Context) (*sessionsv1.Indexing, error)
	// Session a message belongs to, and the message, rewound or folded away by compaction as well; ErrNotFound when there is none
	Locate(ctx context.Context, message int64) (string, *sessionsv1.Message, error)
	Export(ctx context.Context, export *sessionsv1.ExportRequest) ([]*sessionsv1.Transcript, error)
	Import(ctx context.Context, transcripts []*sessionsv1.Transcript) (*sessionsv1.ImportResponse, error)
}

// System prompts sessions keep, so a provider's prompt cache holds them from run to run
type Prompts interface {
	SystemPrompt(ctx context.Context, id string) (string, error)
	SetSystemPrompt(ctx context.Context, id, prompt string, tools []string) error
	// Count that goes up each time the session's prompt, or every session's, is asked to be built again
	PromptEpoch(ctx context.Context, id string) (int64, error)
	// Asks the session, every session for no id, to build its prompt again, returning how many hold one
	RefreshPrompts(ctx context.Context, id string) (int64, error)
	// Finds stored prompts missing the guidance the tools they were built with call for, the texts expected of a prompt with those tools,
	// and clears them on apply
	RepairPrompts(ctx context.Context, request *sessionsv1.RepairPromptsRequest, expected func(tools []string) []string) (*sessionsv1.RepairPromptsResponse, error)
}

// Cleanup and care of the store
type Upkeep interface {
	Prune(ctx context.Context, request *sessionsv1.PruneRequest) (*sessionsv1.PruneResponse, error)
	Archive(ctx context.Context, filter *sessionsv1.Filter, dryRun bool) (*sessionsv1.ArchiveResponse, error)
	Stats(ctx context.Context) (*sessionsv1.StatsResponse, error)
	// Runs a task on the store, telling progress line by line
	Maintain(ctx context.Context, request *sessionsv1.MaintainRequest, progress func(line string)) error
	RepairRouting(ctx context.Context, request *sessionsv1.RepairRoutingRequest) (*sessionsv1.RepairRoutingResponse, error)
	RepairProfiles(ctx context.Context, request *sessionsv1.RepairProfilesRequest, live func(profile string) bool) (*sessionsv1.RepairProfilesResponse, error)
}

// What lets several processes share the store: one turn at a time per session, and beats of the processes serving it
type Coordination interface {
	// Takes the session for one turn of the holder, false while another holder has it
	AcquireTurn(ctx context.Context, id, holder string, ttl time.Duration) (bool, error)
	RefreshTurn(ctx context.Context, id, holder string, ttl time.Duration) (bool, error)
	ReleaseTurn(ctx context.Context, id, holder string) error
	Beat(ctx context.Context, backend Backend) error
	EndBeat(ctx context.Context, id string) error
	Backends(ctx context.Context) ([]Backend, error)
	// Ends the sessions of the sources a process that died left open and idle past the window, returning them
	SweepOrphans(ctx context.Context, sources []string, idle time.Duration) ([]string, error)
}

// Messages sent to sessions, kept until their turn settles so a turn cut short is known when the server starts again
type Admissions interface {
	// Records a message sent to the session by the backend, queued for its turn
	Admit(ctx context.Context, session, text, backend string) (*sessionsv1.Admission, error)
	// Marks the admission's turn started by the backend, a lost one's again
	Begin(ctx context.Context, id, backend string) error
	Settle(ctx context.Context, id, outcome string) error
	// Admissions not settled: queued, started or lost
	Pending(ctx context.Context, session string) ([]*sessionsv1.Admission, error)
	// Turns started by backends no longer alive become lost; returns every lost one and the messages those backends left queued
	Recover(ctx context.Context, alive []string) ([]*sessionsv1.Admission, error)
	Discard(ctx context.Context, session string, ids []string) ([]string, error)
}

// What context compression keeps: its lock, its failures, the compacted transcript and the chain of sessions it continues into
type Compression interface {
	AcquireCompression(ctx context.Context, id, holder string, ttl time.Duration) (bool, error)
	RefreshCompression(ctx context.Context, id, holder string, ttl time.Duration) (bool, error)
	ReleaseCompression(ctx context.Context, id, holder string) error
	CompressionHolder(ctx context.Context, id string) (string, error)
	// Failure that holds compression off until the time, and how many fallbacks and ineffective passes came in a row
	CompressionCooldown(ctx context.Context, id string) (Cooldown, error)
	SetCompressionCooldown(ctx context.Context, id string, cooldown Cooldown) error
	// Folds the active messages up to and with the one given away, still shown but no longer sent to the model, and stores the messages
	// given after them in their place for the model; the first summary among those takes the uids of the folded ones as absorbed. Messages
	// at the start of those given that carry the id of one stored stay as they are
	Compact(ctx context.Context, id string, through int64, messages []*sessionsv1.Message, guard Guard) error
	// Ends the session for compression and starts its continuation, a child holding the messages given
	Continue(ctx context.Context, id string, child *sessionsv1.Session, messages []*sessionsv1.Message) (*sessionsv1.Session, error)
	// Newest continuation of the session, itself when it has none
	Tip(ctx context.Context, id string) (string, error)
	// Sessions of the compression chain the session is in, oldest first
	Lineage(ctx context.Context, id string) ([]string, error)
	// Session at the start of the chain of parents the session hangs from
	Root(ctx context.Context, id string) (string, error)
}

// Failures of compression in a row and how long they hold it off
type Cooldown struct {
	Until            time.Time
	Error            string
	FallbackStreak   int
	IneffectiveCount int
	RecoveryDeadline time.Time
	OverloadStreak   int
}

// What messaging gateways keep with sessions: where each chat routes, handoffs between surfaces and failures of their housekeeping
type Gateway interface {
	// Routing entries of a scope by session key, each the JSON the gateway keeps
	Routes(ctx context.Context, scope string) (map[string]string, error)
	SetRoute(ctx context.Context, scope, key, entry string) error
	DeleteRoute(ctx context.Context, scope, key string) error
	// Newest session of a chat, empty when none
	PeerSession(ctx context.Context, source string, peer *sessionsv1.Peer) (string, error)
	// Next conversation generation of a chat, which never goes back
	NextGeneration(ctx context.Context, source, key string) (int64, error)
	RequestHandoff(ctx context.Context, id, platform string) error
	// Sessions waiting to be handed off to the platform, claimed by the caller
	ClaimHandoffs(ctx context.Context, platform string) ([]string, error)
	FinishHandoff(ctx context.Context, id, failure string) error
	HygieneFailures(ctx context.Context, key string, failed bool) (int, error)
	// Keyed sessions opened and never used, older than the age
	NeverActive(ctx context.Context, older time.Duration) ([]*sessionsv1.Session, error)
}

type Registrant interface {
	Stores() map[string]Store
}

// A store that names itself where a person picks one: its title and what it keeps sessions in
type Described interface {
	Title() string
	About() string
}

type Registry struct {
	registrants []Registrant
	starts      []func(ctx context.Context)
	releases    []func(ctx context.Context, session string)
	// Registry this one is nested in, nil for the host's own
	parent *Registry
	// Hook points of the host's own registry, which hear of the sessions taken out of the store
	hooks *hooks.Hooks
	// What the host's own registry has to tell the person, oldest first, until the app or the command line shows it
	told    sync.Mutex
	notices []string
}

// Tells the person something once, such as a session database set aside: the app shows it in the chat in front, the command line prints it
// when its command ends
func (r *Registry) Tell(text string) {
	if r.parent != nil {
		r.parent.Tell(text)
		return
	}
	r.told.Lock()
	defer r.told.Unlock()
	if !slices.Contains(r.notices, text) {
		r.notices = append(r.notices, text)
	}
}

// What there is to tell the person, oldest first, each handed out once
func (r *Registry) Notices() []string {
	if r.parent != nil {
		return r.parent.Notices()
	}
	r.told.Lock()
	defer r.told.Unlock()
	notices := r.notices
	r.notices = nil
	return notices
}

// Tells the session hooks of sessions taken out of the store by a delete, a prune, an archive or a sweep: the profile of the settings they
// were of, every one taken, why, and the one asked for when there was one
func (r *Registry) Deleted(ctx context.Context, cfg *config.Config, ids []string, reason, primary string) {
	if r.parent != nil {
		r.parent.Deleted(ctx, cfg, ids, reason, primary)
		return
	}
	unique := slices.Sorted(slices.Values(slices.DeleteFunc(slices.Clone(ids), func(id string) bool { return id == "" })))
	unique = slices.Compact(unique)
	if len(unique) == 0 || !r.hooks.Has(cfg, hooks.SessionDelete) {
		return
	}
	event := hooks.Deleted{Session: unique[0], Reason: reason, Profile: hooks.Optional(cfg.Profile())}
	if slices.Contains(unique, primary) {
		event.Session = primary
	}
	if len(unique) > 1 {
		event.IDs = unique
	}
	r.hooks.Fire(context.WithoutCancel(ctx), cfg, event)
}

func (r *Registry) Register(registrant Registrant) {
	if nested, ok := registrant.(*Registry); ok {
		nested.parent = r
	}
	r.registrants = append(r.registrants, registrant)
}

// Runs the function once a long-lived server starts, such as the agent picking up turns cut short
func (r *Registry) OnStart(start func(ctx context.Context)) {
	r.starts = append(r.starts, start)
}

// Runs the function as a session is closed or deleted, to let go of what it holds outside the store, such as the shell its commands ran in
func (r *Registry) OnRelease(release func(ctx context.Context, session string)) {
	r.releases = append(r.releases, release)
}

// Lets go of what the session holds outside the store, it being closed or deleted: every function given to OnRelease, of this registry,
// the one it is nested in and those nested in that
func (r *Registry) Release(ctx context.Context, session string) {
	if r.parent != nil {
		r.parent.Release(ctx, session)
		return
	}
	r.release(ctx, session)
}

func (r *Registry) release(ctx context.Context, session string) {
	for _, release := range r.releases {
		release(ctx, session)
	}
	for _, registrant := range r.registrants {
		if nested, ok := registrant.(*Registry); ok {
			nested.release(ctx, session)
		}
	}
}

func (r *Registry) Stores() map[string]Store {
	stores := map[string]Store{}
	for _, registrant := range r.registrants {
		maps.Copy(stores, registrant.Stores())
	}
	return stores
}

func (r *Registry) Store(name string) (Store, error) {
	store, ok := r.Stores()[name]
	if !ok {
		return nil, fmt.Errorf("There is no session storage named %s. Storage, an option of Session storage, picks one.", name)
	}
	return store, nil
}

// Settings of the profile the session runs on, found in the store the settings name
func (r *Registry) Profile(ctx context.Context, cfg *config.Config, session string) (*config.Config, error) {
	store, err := r.Store(cfg.GetString(sessionsconfig.Store))
	if err != nil {
		return nil, err
	}
	found, err := store.Get(ctx, session)
	if err != nil {
		return nil, err
	}
	return cfg.In(found.GetProfile()), nil
}

// What runs when a long-lived server starts: the start of every registrant that has one, then the functions given to OnStart
func (r *Registry) Start(ctx context.Context) {
	for _, registrant := range r.registrants {
		if starter, ok := registrant.(interface{ Start(ctx context.Context) }); ok {
			starter.Start(ctx)
		}
	}
	for _, start := range r.starts {
		go start(ctx)
	}
}

func (r *Registry) Close() error {
	var errs []error
	for _, registrant := range slices.Backward(r.registrants) {
		if closer, ok := registrant.(io.Closer); ok {
			errs = append(errs, closer.Close())
		}
	}
	return errors.Join(errs...)
}

type Sessions struct {
	Registry
	config *config.Config
	once   sync.Once
}

func New(cfg *config.Config, points *hooks.Hooks) *Sessions {
	s := &Sessions{config: cfg}
	s.Registry.hooks = points
	sessionsconfig.Register(cfg, func() []config.Option {
		var options []config.Option
		stores := s.Stores()
		for _, name := range slices.Sorted(maps.Keys(stores)) {
			option := config.Option{Value: name, Title: name}
			if described, ok := stores[name].(Described); ok {
				option.Title, option.About = described.Title(), described.About()
			}
			options = append(options, option)
		}
		return options
	})
	cfg.Guard(sessionsconfig.AutoPrune, config.Guard{Write: func(cfg *config.Config, _ string, value any) (bool, error) {
		return true, SetAutoPrune(cfg, value)
	}})
	return s
}

// Store sessions are kept in
func (s *Sessions) Current() (Store, error) {
	return s.Store(s.config.GetString(sessionsconfig.Store))
}

// Starts the work of a long-lived server once
func (s *Sessions) Start(ctx context.Context) {
	s.once.Do(func() { s.Registry.Start(ctx) })
}

// Deletes the sessions of a deleted profile and what the gateway keeps of its chats
func (s *Sessions) Delete(profile string) error {
	store, err := s.Current()
	if err != nil {
		return err
	}
	return store.Clear(context.Background(), profile)
}

// Moves the sessions of a renamed profile and what the gateway keeps of its chats to its new name
func (s *Sessions) Rename(old, renamed string) error {
	store, err := s.Current()
	if err != nil {
		return err
	}
	return store.Rekey(context.Background(), old, renamed)
}

// Refusal to load a session back that holds more messages than the resume limit
func TooLong(count, limit int64) error {
	return fmt.Errorf("This session has %s messages, more than the %s that can be reopened safely. Start a new session and keep a copy with "+
		"icebath sessions export --format md --session-id <id>, or raise Reopen limit, an option of Session storage.", strs.Number(float64(count)),
		strs.Number(float64(limit)))
}

// Refusal to export a session in memory that holds more messages than the export limit
func TooLarge(id string, limit int64) error {
	return fmt.Errorf("The session %s has more than %s messages, the most a JSON export holds. Export it as Markdown with "+
		"icebath sessions export --format md --session-id %s, or raise Export limit, an option of Session storage.", id, strs.Number(float64(limit)), id)
}

type resumeKey struct{}

// Context of an app run that opens the session in place of a new chat
func WithResume(ctx context.Context, id string) context.Context {
	return context.WithValue(ctx, resumeKey{}, id)
}

// Session the app run was asked to open, empty for none
func Resumed(ctx context.Context) string {
	id, _ := ctx.Value(resumeKey{}).(string)
	return id
}

type shownKey struct{}

// Session an app run showed last, which the launcher names on the way out so it can be resumed
type Shown struct {
	mu sync.Mutex
	id string
}

func (s *Shown) Set(id string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.id = id
}

func (s *Shown) ID() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.id
}

// Context of an app run that records the session it shows last
func WithShown(ctx context.Context) (context.Context, *Shown) {
	shown := &Shown{}
	return context.WithValue(ctx, shownKey{}, shown), shown
}

// Records the session the app run shows, where the run records one
func Show(ctx context.Context, id string) {
	if shown, ok := ctx.Value(shownKey{}).(*Shown); ok && id != "" {
		shown.Set(id)
	}
}
