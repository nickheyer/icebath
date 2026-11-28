package server

import (
	"cmp"
	"context"
	"fmt"
	"maps"
	"regexp"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"connectrpc.com/connect/v2"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/nickheyer/icebath/internal/approvals"
	"github.com/nickheyer/icebath/internal/auxiliary"
	"github.com/nickheyer/icebath/internal/config"
	"github.com/nickheyer/icebath/internal/contexts"
	"github.com/nickheyer/icebath/internal/credentials"
	"github.com/nickheyer/icebath/internal/delegation"
	"github.com/nickheyer/icebath/internal/hooks"
	"github.com/nickheyer/icebath/internal/models"
	agentconfig "github.com/nickheyer/icebath/internal/plugins/defaults/agents/agent/config"
	agentlaunch "github.com/nickheyer/icebath/internal/plugins/defaults/agents/agent/launch"
	"github.com/nickheyer/icebath/internal/prompt"
	"github.com/nickheyer/icebath/internal/provider"
	providerconfig "github.com/nickheyer/icebath/internal/provider/config"
	"github.com/nickheyer/icebath/internal/sessions"
	sessionsconfig "github.com/nickheyer/icebath/internal/sessions/config"
	"github.com/nickheyer/icebath/internal/skills"
	"github.com/nickheyer/icebath/internal/slash"
	"github.com/nickheyer/icebath/internal/terminal"
	"github.com/nickheyer/icebath/internal/tools"
	"github.com/nickheyer/icebath/internal/transport"
	"github.com/nickheyer/icebath/internal/web"
	"github.com/nickheyer/icebath/internal/worktrees"
	agentv1 "github.com/nickheyer/icebath/pkg/gen/icebath/plugins/agent/v1"
	sessionsv1 "github.com/nickheyer/icebath/pkg/gen/icebath/sessions/v1"
)

type Agent struct {
	ctx    context.Context
	cancel context.CancelFunc

	config      *config.Config
	providers   *provider.Registry
	transports  *transport.Registry
	credentials *credentials.Registry
	models      *models.Registry
	tools       *tools.Registry
	sessions    *sessions.Registry
	slash       *slash.Registry
	auxiliary   *auxiliary.Registry
	prompts     *prompt.Registry
	// What decides whether risky commands and actions go ahead, and the prompts waiting for the user
	approvals *approvals.Approvals
	// Search backends, whose active one a provider's own search can stand in for
	web *web.Registry
	// Skills of the profiles, which the prompts list and whose changes the turns are told of
	skills *skills.Registry
	// Compressions running now over every session
	slots chan struct{}
	// Shells of the sessions, whose sandboxes see the files of the profile's caches at their own paths
	terminal *terminal.Terminal
	// Profiles whose old saved tool results were swept in this process
	swept map[string]bool
	// Context engines that keep each session within its model's window
	contexts *contexts.Contexts
	// What helper model calls learned, shared by every session
	learned *auxiliary.State
	// Process this server serves sessions as, and the turns running now
	life *agentLife
	// Git worktrees subagents work in apart from their parent's checkout
	worktrees *worktrees.Worktrees
	// Backends that route each task of delegate_task to where its subagent runs
	delegation *delegation.Registry
	// Subagents running in this process, and the background delegations told to their sessions as they end
	tree  *spawnTree
	async *background
	// Retries sessions armed for the login they are held to to reset
	waits loginWaits
	// Lifecycle hook points the turns fire, which plugins and the user's shell hooks set callbacks on
	hooks *hooks.Hooks
	// Sessions whose on_session_finalize hooks ran, which their end tells no more; and the pre_command hooks of each session waiting their
	// turn
	ended    map[string]bool
	commands map[string]*observing

	mu      sync.Mutex
	runs    map[string]*run
	working sync.WaitGroup
}

type run struct {
	// Session the run serves, which a compression that continued the conversation in a new session moves on
	session atomic.Pointer[string]

	mu        sync.Mutex
	watchers  map[chan *agentv1.WatchResponse]struct{}
	queue     []queued
	again     bool
	busy      bool
	stop      context.CancelFunc
	text      strings.Builder
	reasoning strings.Builder
	// Questions the clarify tool waits on the user for, and where their outcome goes once settled
	questions *agentv1.Questions
	settled   chan string
	// Profile the session runs on, whose providers, accounts and settings it uses
	profile  string
	provider string
	model    string
	effort   string
	// Notices of the session, kept so a watcher that comes later, the first one of a new session included, still sees them
	notices []notice
	// System prompt of the session, built on its first turn and kept until what it was made for changes
	system *system
	// What the sections of its prompt keep for the life of the session, such as the workspace snapshot
	kept map[string]string
	// The user sent a message while the reply ran, for a long running tool to hand its work to the background
	yield atomic.Bool
	// Windows watching the session, admissions of its queued messages and when it is closed
	life runLife
	// The session answers one query of a one-shot run, so it waits for its background work before it ends
	oneshot bool
	// The client of the last message waits for its turn alone, as a query run of the command line does: nobody answers the turn's prompts and
	// nothing is left to tell the session after it
	finite atomic.Bool
	// Shell of the session's client, as the session froze it from its first message; nil when it was sent none
	client *tools.Client
	// Settings the session runs with over its profile's, such as what a launch of the command line asked of it, and the skills its prompt
	// loads; nil for none
	overrides *sessions.Overrides
	skills    []string
	// Files the writes of the turn running landed in, which verify on stop asks about before it ends
	edited []string
	// Model of the fallback chain the session runs on while its main one fails, nil while it runs on its main one
	failover *failover
	// What its model calls took since it was opened here
	spent spent
	// How its context is kept within the model's window
	cx compaction
	// Guidance the user or the parent agent steered the running turn with, delivered right after its next tool results
	steering []string
	// What the session runs as a subagent, nil for a session of its own
	child *child
	// How long the subagent the session runs went without progress before it was given up as stale, 0 while it is not
	stale time.Duration
	// Subagents the session's one-shot run started so far
	spawned int
	// Provider, login and reset the session's windows were last told ran out, so a login is told of once until it resets
	exhausted string
	// What the session's model request did last and when, for its liveness; the line its windows show in place while the request waits
	doing   string
	touched time.Time
	wait    string
	// Newest line on the profile's Bot Desktop since the last message, told with the next one
	screen string
	// Route and effort of the turn the person spoke, nil outside one; whether the session was told the voice chat model could not be used
	voice       *voiceTurn
	warnedVoice bool
	// The person cut a spoken reply off, which the next message queued tells the model
	cut bool
	// What its route turned down of the reasoning its requests asked for
	refused refusals
}

// Records what the session's model request is doing now, which a subagent's liveness counts as progress
func (r *run) touch(doing string) {
	r.mu.Lock()
	r.doing, r.touched = doing, time.Now()
	c := r.child
	r.mu.Unlock()
	c.touch(doing)
}

// Shows the line in place while the request waits on the provider, replaced by the next and cleared by an empty one
func (r *run) waiting(text string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if text == r.wait {
		return
	}
	r.wait = text
	r.publish(&agentv1.WatchResponse{Event: &agentv1.WatchResponse_Activity{Activity: text}})
}

// What the session runs as a subagent, nil for a session of its own
func (r *run) subagent() *child {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.child
}

// Session the run serves now
func (r *run) sessionID() string {
	if id := r.session.Load(); id != nil {
		return *id
	}
	return ""
}

// Notice and the count of messages the session held when it was given, which places it among them on a replay
type notice struct {
	after int
	text  string
}

const keptNotices = 200

var named = regexp.MustCompile(`^/[A-Za-z][A-Za-z0-9_-]*$`)

func echo(line string) string {
	return "`" + strings.ReplaceAll(line, "`", "'") + "`"
}

// Told a person whose message waits for the reply running
const queuedNote = "Your message is queued. It is sent when the current reply finishes."

// Why a command was refused, marked once as a refusal
func refused(err error) string {
	return "✗ " + strings.TrimPrefix(err.Error(), "✗ ")
}

// Refusal of a provider the profile does not have
func noProvider(profile, name string) error {
	return fmt.Errorf("The profile %s has no provider named %s. /providers adds it, and /model picks another.", profile, name)
}

func (a *Agent) Close() error {
	a.mu.Lock()
	a.cancel()
	a.mu.Unlock()
	// Retries armed for logins stay in their file, armed again when icebath next starts
	a.waits.mu.Lock()
	for id, timer := range a.waits.timers {
		timer.Stop()
		delete(a.waits.timers, id)
	}
	a.waits.mu.Unlock()
	a.working.Wait()
	a.shutdown()
	return nil
}

func (a *Agent) Send(ctx context.Context, id, text string) error {
	return a.Message(ctx, id, text, false, false)
}

// A message the person sent: a command run, else queued, steering or stopping the reply as the settings say. Spoken in voice mode its turn
// runs on the voice chat model; sent after the person cut a spoken reply off, the next message queued tells the model so
func (a *Agent) Message(ctx context.Context, id, text string, voice, interrupted bool) error {
	r, err := a.run(id)
	if err != nil {
		return err
	}
	if text = strings.TrimSpace(text); text == "" {
		return connect.NewError(connect.CodeInvalidArgument, "Your message is empty. Type something to send.")
	}
	line, _, _ := strings.Cut(text, "\n")
	word, _, _ := strings.Cut(line, " ")
	if command, argument, ok := a.slash.Command(text); ok {
		// The pre_command hooks hear of it before it runs
		if done := a.precommand(ctx, id, command.Name, strings.ToLower(strings.TrimPrefix(word, "/")), argument); done != nil {
			select {
			case <-done:
			case <-ctx.Done():
				return ctx.Err()
			}
		}
		note, err := command.Run(ctx, &slash.Call{SessionID: id, Argument: argument})
		if err != nil {
			note = refused(err)
		}
		if note != "" {
			r.mu.Lock()
			a.notify(r, echo(line)+"\n\n"+note)
			r.mu.Unlock()
		}
		return nil
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	// A cut spoken reply waits for the next message queued, past a steer, a refusal or a mistyped command
	r.cut = r.cut || interrupted
	if named.MatchString(word) {
		a.notify(r, echo(line)+"\n\n✗ There is no command "+word+". Type /help to list the commands.")
		return nil
	}
	if err := a.blocked(ctx, r); err != nil {
		a.notify(r, echo(line)+"\n\n"+refused(err))
		return nil
	}
	// A message sent while the reply runs is queued, steers the turn or stops it to go now, as the session's settings say
	mode := agentconfig.BusyQueue
	if r.busy {
		mode = a.config.In(r.profile).With(r.overrides.Layer()).GetString(agentconfig.BusyInputMode)
	}
	if mode == agentconfig.BusySteer {
		r.steering = append(r.steering, text)
		a.notify(r, "The agent gets your message after its next tool call: “"+steerPreview(text)+"”.")
		return nil
	}
	a.admit(r, text)
	r.queue = append(r.queue, r.said(text, voice))
	if !r.busy {
		return a.start(r)
	}
	r.yield.Store(true)
	if mode == agentconfig.BusyInterrupt && r.stop != nil {
		r.stop()
		a.notify(r, "Stopping the current reply. Your message is sent next.")
		return nil
	}
	a.notify(r, queuedNote)
	return nil
}

func (a *Agent) Stop(id string) error {
	r, err := a.run(id)
	if err != nil {
		return err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.stop != nil {
		r.stop()
	}
	return nil
}

func (a *Agent) Watch(ctx context.Context, id, client string, send func(*agentv1.WatchResponse) error) error {
	r, err := a.run(id)
	if err != nil {
		return err
	}
	if err := a.attach(r, client); err != nil {
		return connect.NewError(connect.CodeResourceExhausted, err.Error())
	}
	defer a.detach(r, client)
	store, err := a.store()
	if err != nil {
		return err
	}
	events := make(chan *agentv1.WatchResponse, 1024)
	state := a.state(r)
	r.mu.Lock()
	messages, err := store.Messages(ctx, id, sessions.Display)
	if err != nil {
		r.mu.Unlock()
		return err
	}
	pending := []*agentv1.WatchResponse{state}
	noted := 0
	for i, message := range messages {
		for ; noted < len(r.notices) && r.notices[noted].after <= i; noted++ {
			pending = append(pending, noticeEvent(r.notices[noted].text))
		}
		pending = append(pending, messageEvent(message))
	}
	for ; noted < len(r.notices); noted++ {
		pending = append(pending, noticeEvent(r.notices[noted].text))
	}
	if r.busy {
		pending = append(pending, busyEvent(true))
	}
	if r.text.Len()+r.reasoning.Len() > 0 {
		pending = append(pending, deltaEvent(r.text.String(), r.reasoning.String()))
	}
	if prompts := a.approvals.Pending(r.sessionID()); len(prompts) > 0 {
		pending = append(pending, approvalsEvent(prompts))
	}
	if r.questions != nil {
		pending = append(pending, questionsEvent(r.questions))
	}
	r.watchers[events] = struct{}{}
	r.mu.Unlock()
	defer func() {
		r.mu.Lock()
		delete(r.watchers, events)
		r.mu.Unlock()
	}()
	for _, event := range pending {
		if err := send(event); err != nil {
			return err
		}
	}
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-a.ctx.Done():
			return connect.NewError(connect.CodeUnavailable, "icebath is shutting down.")
		case event, ok := <-events:
			if !ok {
				return connect.NewError(connect.CodeAborted, "This window fell behind the session. Open the session again to see new messages.")
			}
			if err := send(event); err != nil {
				return err
			}
		}
	}
}

func (a *Agent) store() (sessions.Store, error) {
	return a.sessions.Store(a.config.GetString(sessionsconfig.Store))
}

func (a *Agent) run(id string) (*run, error) {
	store, err := a.store()
	if err != nil {
		return nil, err
	}
	session, err := store.Get(a.ctx, id)
	if err != nil {
		return nil, err
	}
	a.mu.Lock()
	r := a.runs[id]
	a.mu.Unlock()
	if r != nil {
		return r, nil
	}
	if err := a.revive(a.ctx, store, session); err != nil {
		return nil, err
	}
	a.mu.Lock()
	r = a.runs[id]
	made := r == nil
	if made {
		// A session held again ends again, as its hooks know it
		delete(a.ended, id)
		r = &run{profile: session.GetProfile(), watchers: map[chan *agentv1.WatchResponse]struct{}{}, life: runLife{active: time.Now()},
			oneshot: session.GetSource() == oneshotSource, client: storedClient(session), overrides: sessions.OverridesOf(session),
			skills: storedSkills(session)}
		r.session.Store(&id)
		a.runs[id] = r
	}
	a.mu.Unlock()
	if made {
		a.approvals.Watch(id, func() {
			event := approvalsEvent(a.approvals.Pending(id))
			r.mu.Lock()
			defer r.mu.Unlock()
			r.publish(event)
		})
	}
	if made && a.approvals.Restore(id, session.GetYolo()) {
		r.mu.Lock()
		a.notify(r, "⚠ "+approvals.SkipKept())
		r.mu.Unlock()
	}
	a.evict()
	return r, nil
}

// Profile the session runs on
func (r *run) profileName() string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.profile
}

// Settings the session runs with: those of its profile with its own over them
func (a *Agent) settings(r *run) *config.Config {
	r.mu.Lock()
	defer r.mu.Unlock()
	return a.config.In(r.profile).With(r.overrides.Layer())
}

func (a *Agent) start(r *run) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.ctx.Err() != nil {
		return connect.NewError(connect.CodeUnavailable, "icebath is shutting down.")
	}
	r.busy = true
	r.publish(busyEvent(true))
	a.working.Add(1)
	go a.work(r)
	return nil
}

// Runs work the sessions leave behind, such as a title or a lease given back, in the background where Close waits for it; false, the
// work not started, once the agent is closing
func (a *Agent) aside(work func()) bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.ctx.Err() != nil {
		return false
	}
	a.working.Add(1)
	go func() {
		defer a.working.Done()
		work()
	}()
	return true
}

// Sessions this process holds now
func (a *Agent) held() []*run {
	a.mu.Lock()
	defer a.mu.Unlock()
	return slices.Collect(maps.Values(a.runs))
}

// Whether the run is of a session this process still holds, neither closed nor dropped, and icebath is not closing
func (a *Agent) holds(r *run) bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.ctx.Err() == nil && a.runs[r.sessionID()] == r
}

func (a *Agent) work(r *run) {
	defer a.working.Done()
	var wait lingering
	for {
		r.mu.Lock()
		if len(r.queue) == 0 && !r.again && a.ctx.Err() == nil && wait.due(r) {
			r.mu.Unlock()
			if a.linger(r, &wait) {
				continue
			}
			r.mu.Lock()
		}
		if len(r.queue) == 0 && !r.again || a.ctx.Err() != nil {
			r.busy, r.wait = false, ""
			r.publish(busyEvent(false))
			r.mu.Unlock()
			return
		}
		text, admission := queued{}, ""
		if r.again {
			r.again, admission, r.life.resumed = false, r.life.resumed, ""
		} else {
			text, r.queue = r.queue[0], r.queue[1:]
			if len(r.life.admissions) > 0 {
				admission, r.life.admissions = r.life.admissions[0], r.life.admissions[1:]
			}
		}
		ctx, stop := context.WithCancel(a.ctx)
		r.stop = stop
		r.mu.Unlock()
		finish, err := a.enter(ctx, r, admission)
		if err == nil {
			if text.compact != nil {
				err = a.compactNow(ctx, r, text.compact)
			} else {
				err = a.turn(ctx, r, text)
			}
			finish(outcome(ctx, err))
		}
		// Only a one-shot query answered waits for its background work
		wait.done = wait.done || wait.rounds == 0 && err != nil
		r.mu.Lock()
		r.text.Reset()
		r.reasoning.Reset()
		failed := false
		switch {
		case ctx.Err() != nil:
			a.notify(r, agentlaunch.Stopped)
		case err != nil:
			a.notify(r, agentlaunch.Failed+strings.TrimPrefix(err.Error(), agentlaunch.Failed))
			failed = true
		}
		if r.child == nil && len(r.steering) > 0 {
			// Guidance that came after the turn's last tool results is the next message; a turn stopped drops it with the turn
			leftover := strings.Join(r.steering, "\n")
			r.steering = nil
			if ctx.Err() == nil {
				a.admit(r, leftover)
				r.queue = append(r.queue, queued{text: leftover})
				a.notify(r, "Your /steer message came after the agent's last tool call, so it is sent as your next message.")
			}
		}
		r.mu.Unlock()
		if failed {
			// A turn that failed on the login the session is held to says once that it ran out, and what can be done
			a.exhaustedLogin(r)
		}
		stop()
	}
}

func (a *Agent) route(r *run) (provider.Instance, provider.Route, error) {
	settings := a.settings(r)
	r.mu.Lock()
	name, model, c := r.provider, r.model, r.child
	r.mu.Unlock()
	if c != nil && c.place.fixed != nil {
		route := *c.place.fixed
		route.Model = cmp.Or(model, route.Model)
		return provider.Instance{ID: route.Provider, Spec: provider.Spec{Name: route.Spec, BaseURL: route.BaseURL, Mode: route.Mode}, Known: true}, route, nil
	}
	// The client's ICEBATH_INFERENCE_PROVIDER, or this process's for a session with none, picks the provider when nothing else does
	requested, _ := r.shell().Lookup("ICEBATH_INFERENCE_PROVIDER")
	if requested = strings.ToLower(strings.TrimSpace(requested)); requested == "auto" {
		requested = ""
	}
	return a.routeModel(settings, name, cmp.Or(settings.GetString(agentconfig.Provider), requested), cmp.Or(model, settings.GetString(agentconfig.Model)))
}

// Instance of the provider the name picks, the first connected for none, and the route of the model on it with its account's credential
func (a *Agent) routeFor(settings *config.Config, name, model string) (provider.Instance, provider.Route, error) {
	instance, err := a.instance(settings, name)
	if err != nil {
		return instance, provider.Route{}, err
	}
	route, err := a.credentials.Route(instance, model)
	if err == nil && route.Model == "" {
		// A kind with no default model lands on the one its catalog names, as one never picked
		if fallback := a.models.Host().DefaultModel(settings, route); fallback != "" {
			route, err = a.credentials.Route(instance, fallback)
		}
	}
	return instance, route, err
}

// Provider of the profile with the name, or the first of the kind it names; one of the kind as its plugin sets it up when the profile has
// none of it, such as for a session that leaves the profile's config files out, its credentials coming from the environment
func (a *Agent) provider(settings *config.Config, name string) (provider.Instance, bool) {
	if instance, ok := a.providers.Instance(settings, name); ok {
		return instance, true
	}
	spec, ok := a.providers.Spec(name)
	if !ok {
		return provider.Instance{}, false
	}
	view := settings.View(providerconfig.Key).View(spec.Name)
	if spec.Resolve != nil {
		spec = spec.Resolve(spec, view)
	}
	return provider.Instance{ID: spec.Name, Spec: spec, Known: true, Settings: view}, true
}

// Provider of the profile with the name, or for no name the one icebath detects in its order: a kind taken first, a key set in the
// environment over a signed in account, an account signed in, any with a credential, a kind taken last
func (a *Agent) instance(settings *config.Config, name string) (provider.Instance, error) {
	if name != "" {
		instance, ok := a.provider(settings, name)
		if !ok {
			return instance, noProvider(settings.Profile(), name)
		}
		return instance, nil
	}
	return a.credentials.Auto(settings, a.providers, a.transports.Factories())
}

func (a *Agent) state(r *run) *agentv1.WatchResponse {
	route, effort := a.own(r)
	used, window, estimated, compressions := a.dial(r)
	cost := a.cost(r)
	settings := a.settings(r)
	busy := settings.GetString(agentconfig.BusyInputMode)
	title := a.providerTitle(settings, route.Provider)
	inUse := a.config.Profile()
	r.mu.Lock()
	defer r.mu.Unlock()
	return &agentv1.WatchResponse{Event: &agentv1.WatchResponse_State{State: &agentv1.State{
		Profile:          r.profile,
		ProfileInUse:     r.profile == inUse,
		Provider:         route.Provider,
		ProviderTitle:    title,
		Model:            route.Model,
		ReasoningEffort:  effort,
		Yolo:             a.approvals.Skips(r.sessionID()),
		ContextUsed:      int64(used),
		ContextMax:       int64(window),
		ContextEstimated: estimated,
		Compressions:     int32(compressions),
		Cost:             cost,
		BusyInputMode:    busy,
	}}}
}

// Dollars the session's model calls cost so far as the store counts them, the provider's figure over the estimate; 0 when not known
func (a *Agent) cost(r *run) float64 {
	store, err := a.store()
	if err != nil {
		return 0
	}
	session, err := store.Get(a.ctx, r.sessionID())
	if err != nil {
		return 0
	}
	if actual := session.GetUsage().GetActualCost(); actual > 0 {
		return actual
	}
	return session.GetUsage().GetEstimatedCost()
}

// How full the session's context is for the dial of its status line: the size, the window, whether the size is estimated, and the
// compressions so far; zeros while nothing is known
func (a *Agent) dial(r *run) (int, int, bool, int) {
	if a.contexts == nil {
		return 0, 0, false, 0
	}
	engine := a.contexts.Engine(r.sessionID())
	if engine == nil {
		return 0, 0, false, 0
	}
	status := engine.Status()
	store, err := a.store()
	if err != nil || status.Window <= 0 {
		return 0, 0, false, status.Compressions
	}
	history, err := store.Messages(a.ctx, r.sessionID(), sessions.Model)
	if err != nil || len(history) == 0 {
		return 0, status.Window, false, status.Compressions
	}
	used, estimated := a.occupancy(a.ctx, r, history)
	return min(used, status.Window), status.Window, estimated, status.Compressions
}

func (a *Agent) changed(r *run) {
	state := a.state(r)
	r.mu.Lock()
	defer r.mu.Unlock()
	r.publish(state)
}

// Effort the session's model calls ask for: the voice chat effort through a turn the person spoke, else the session's own; what the route
// turned down of reasoning left out
func (a *Agent) effort(r *run) string {
	r.mu.Lock()
	v, refused := r.voice, r.refused
	r.mu.Unlock()
	effort := a.ownEffort(r)
	if v != nil && v.effort != "" {
		effort = v.effort
	}
	return refused.apply(effort)
}

// Effort the session is set to: a subagent's own, else the session's own setting over the profile's
func (a *Agent) ownEffort(r *run) string {
	r.mu.Lock()
	effort := r.effort
	r.mu.Unlock()
	return cmp.Or(effort, a.settings(r).GetString(agentconfig.ReasoningEffort))
}

func (a *Agent) append(store sessions.Store, r *run, message *sessionsv1.Message) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if err := store.Append(a.ctx, r.sessionID(), sessions.Guard{Turn: a.holder(r)}, message); err != nil {
		if err := a.divert(r, err, message); err != nil {
			return err
		}
	}
	r.text.Reset()
	r.reasoning.Reset()
	r.publish(messageEvent(message))
	return nil
}

func (r *run) delta(text, reasoning string) {
	r.mu.Lock()
	r.text.WriteString(text)
	r.reasoning.WriteString(reasoning)
	r.publish(deltaEvent(text, reasoning))
	c := r.child
	r.mu.Unlock()
	c.streamed(text, reasoning)
}

// Drops the reply streamed so far, which the next one replaces
func (r *run) restart() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.text.Reset()
	r.reasoning.Reset()
	r.publish(&agentv1.WatchResponse{Event: &agentv1.WatchResponse_Delta{Delta: &agentv1.Delta{Restart: true}}})
}

func (r *run) publish(event *agentv1.WatchResponse) {
	for events := range r.watchers {
		select {
		case events <- event:
		default:
			delete(r.watchers, events)
			close(events)
		}
	}
}

// Keeps the notice for later watchers and shows it to those watching now, r.mu held
func (a *Agent) notify(r *run, text string) {
	after := 0
	if store, err := a.store(); err == nil {
		if messages, err := store.Messages(a.ctx, r.sessionID(), sessions.Display); err == nil {
			after = len(messages)
		}
	}
	r.notices = append(r.notices, notice{after: after, text: text})
	if len(r.notices) > keptNotices {
		r.notices = r.notices[len(r.notices)-keptNotices:]
	}
	r.publish(noticeEvent(text))
}

// Drops the notices given after the first keep messages, for a session cut back to them, r.mu held
func (r *run) forget(keep int) {
	for i := range r.notices {
		r.notices[i].after = min(r.notices[i].after, keep)
	}
}

func messageEvent(message *sessionsv1.Message) *agentv1.WatchResponse {
	return &agentv1.WatchResponse{Event: &agentv1.WatchResponse_Message{Message: message}}
}

func deltaEvent(text, reasoning string) *agentv1.WatchResponse {
	return &agentv1.WatchResponse{Event: &agentv1.WatchResponse_Delta{Delta: &agentv1.Delta{Text: text, Reasoning: reasoning}}}
}

// Event of the approval prompts waiting, none when nothing waits
func approvalsEvent(prompts []approvals.Prompt) *agentv1.WatchResponse {
	pending := make([]*agentv1.Approval, len(prompts))
	for i, p := range prompts {
		approval := &agentv1.Approval{Id: p.ID, Command: p.Command, Reason: p.Description, Keys: p.Keys, SmartDenied: p.SmartDenied, Title: p.Title,
			Deadline: timestamppb.New(p.Deadline)}
		for _, choice := range p.Choices {
			approval.Choices = append(approval.Choices, &agentv1.Choice{Value: choice.Value, Label: choice.Label})
		}
		pending[i] = approval
	}
	return &agentv1.WatchResponse{Event: &agentv1.WatchResponse_Approvals{Approvals: &agentv1.Approvals{Pending: pending}}}
}

func noticeEvent(text string) *agentv1.WatchResponse {
	return &agentv1.WatchResponse{Event: &agentv1.WatchResponse_Notice{Notice: text}}
}

func busyEvent(busy bool) *agentv1.WatchResponse {
	return &agentv1.WatchResponse{Event: &agentv1.WatchResponse_Busy{Busy: busy}}
}

// Adds a message the session gets from outside a turn, such as one a background process sends as it ends: shown to the user by its title
// and answered in a turn of its own, after the one running
func (a *Agent) deliver(r *run, notice tools.Notice) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if notice.Title != "" {
		a.notify(r, notice.Title)
	}
	if notice.Text == "" {
		return nil
	}
	r.queue = append(r.queue, queued{text: notice.Text, notice: true})
	if r.busy {
		return nil
	}
	return a.start(r)
}
