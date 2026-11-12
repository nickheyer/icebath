package acp

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
	"time"

	"github.com/google/uuid"

	"github.com/nickheyer/icebath/pkg/gateway"
	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/i18n"
	"github.com/nickheyer/icebath/pkg/internal/pyacp"
	"github.com/nickheyer/icebath/pkg/internal/pydict"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
	"github.com/nickheyer/icebath/pkg/logging"
	"github.com/nickheyer/icebath/pkg/pm"
	"github.com/nickheyer/icebath/pkg/provider"
	"github.com/nickheyer/icebath/pkg/tools"
)

var gatewayServerLog = logging.GetLogger("acp_adapter.gateway_server")

// Owner connection the gateway agent projects, requests answered as ordered objects
type gatewayLink interface {
	RPC(ctx context.Context, method string, params map[string]any, timeout time.Duration) (*pyjson.Object, error)
	Events() *gateway.EventQueue
	ReaderDone() bool
	Close() error
}

type clientLink struct{ client *gateway.GatewayClient }

func (c *clientLink) RPC(ctx context.Context, method string, params map[string]any, timeout time.Duration) (*pyjson.Object, error) {
	result, err := c.client.RPCTimeout(ctx, method, params, timeout)
	if err != nil {
		return nil, err
	}
	obj, ok := result.(*pyjson.Object)
	if !ok {
		return nil, fmt.Errorf("gateway %s answered %T, not an object", method, result)
	}
	return obj, nil
}

func (c *clientLink) Events() *gateway.EventQueue { return c.client.Events }
func (c *clientLink) ReaderDone() bool            { return c.client.ReaderDone() }
func (c *clientLink) Close() error                { return c.client.Close() }

// Attaches to this home's gateway owner
var connectGateway = func(ctx context.Context) (gatewayLink, error) {
	client, err := gateway.ConnectGateway(ctx)
	if err != nil {
		return nil, err
	}
	return &clientLink{client: client}, nil
}

// Logical session id of an id an editor holds
var logicalSessionID = LogicalSessionID

// Provider and model of a picker id or typed model input
var parseModelInput = provider.ParseModelInput

// Editor side calls of the gateway agent, session updates, permission requests and raw requests
type gatewayEditor interface {
	Client
	SendRequest(ctx context.Context, method string, params any) (any, error)
}

type editorConn struct{ *pyacp.AgentSideConnection }

func (e editorConn) SendRequest(ctx context.Context, method string, params any) (any, error) {
	return e.Conn().SendRequest(ctx, method, params)
}

// Post ack recovery request queued behind every frame read so far
type recoveryRequest struct{ sessionID, admissionID string }

// Reply cursor a snapshot's pending rows were read at
type pendingWatermark struct {
	epoch    any
	sequence float64
}

// Staged submit whose ack never arrived, kept for the editor's retry of the same prompt
type retainedSubmit struct {
	key         submitKey
	inputID     string
	text        string
	attachments []StagedAttachment
}

type permissionKey struct{ session, promptID, generation string }

type inflightKey struct{ session, admission string }

// Mode, model, provider and effort the editor's config options last showed
type shownConfig struct {
	fields *pyjson.Object
	mode   string
}

func (c *shownConfig) equal(o *shownConfig) bool {
	if c == nil || o == nil {
		return c == o
	}
	return c.mode == o.mode && pyrepr.Equal(c.fields, o.fields)
}

// Canonical command name a text invokes, false for ordinary text
func slashCommandOf(text string) (string, bool) {
	stripped := pystr.Lstrip(text)
	if !strings.HasPrefix(stripped, "/") {
		return "", false
	}
	parts := pystr.SplitWhitespace(stripped, 1)
	name := parts[0][1:]
	if name == "" || strings.Contains(name, "/") {
		return "", false
	}
	if canonical, ok := ResolveCommand(name); ok {
		return canonical, true
	}
	if _, ok := gatewayRoutes[name]; ok {
		return name, true
	}
	return "", false
}

// Explicit route a safe mode or ignore user config launch freezes into the session
func bypassRoute() (map[string]any, error) {
	model := strings.TrimSpace(os.Getenv("ICEBATH_MODEL"))
	if model == "" {
		model = strings.TrimSpace(os.Getenv("ICEBATH_INFERENCE_MODEL"))
	}
	if model == "" {
		return nil, &gateway.GatewayClientError{Msg: "--safe-mode / --ignore-user-config read no profile default model: launch " +
			"with one, e.g. icebath -m <model> --provider <provider> --safe-mode acp"}
	}
	providerName := strings.TrimSpace(os.Getenv("ICEBATH_INFERENCE_PROVIDER"))
	if providerName == "" {
		return nil, &gateway.GatewayClientError{Msg: "--safe-mode / --ignore-user-config read no profile provider: launch with " +
			"--provider too, e.g. icebath -m <model> --provider <provider> --safe-mode acp"}
	}
	return map[string]any{"model": model, "provider": providerName}, nil
}

// Notice that the owner re-froze the session's editor MCP tools
func toolsChangedNotice(payload *pyjson.Object) string {
	var names []string
	if servers, ok := dget(payload, "mcp_servers").([]any); ok {
		for _, s := range servers {
			names = append(names, pyStr(s))
		}
	}
	servers := strings.Join(names, ", ")
	if servers == "" {
		servers = "none"
	}
	return fmt.Sprintf("Editor MCP tools changed (servers: %s); the new tool list applies from the next turn.\n", servers)
}

var (
	_ pyacp.Connector          = (*GatewayACPAgent)(nil)
	_ pyacp.Initializer        = (*GatewayACPAgent)(nil)
	_ pyacp.Authenticator      = (*GatewayACPAgent)(nil)
	_ pyacp.SessionCreator     = (*GatewayACPAgent)(nil)
	_ pyacp.SessionLoader      = (*GatewayACPAgent)(nil)
	_ pyacp.SessionResumer     = (*GatewayACPAgent)(nil)
	_ pyacp.Canceller          = (*GatewayACPAgent)(nil)
	_ pyacp.SessionForker      = (*GatewayACPAgent)(nil)
	_ pyacp.SessionLister      = (*GatewayACPAgent)(nil)
	_ pyacp.Prompter           = (*GatewayACPAgent)(nil)
	_ pyacp.SessionModelSetter = (*GatewayACPAgent)(nil)
	_ pyacp.SessionModeSetter  = (*GatewayACPAgent)(nil)
	_ pyacp.ConfigOptionSetter = (*GatewayACPAgent)(nil)
)

// ACP projection of the profile gateway, it never constructs an execution owner
type GatewayACPAgent struct {
	loop            *coLoop
	ctx             context.Context
	conn            gatewayEditor
	gateway         gatewayLink
	connection      gatewayLink
	connectLock     *coLock
	home            string
	snapshots       *pydict.Dict[string, *pyjson.Object]
	eventTask       *loopTask
	terminals       *pydict.Dict[string, *pyjson.Object]
	streamed        map[string]string
	segments        map[string][]string
	failure         error
	link            int
	lostFailure     error
	editorMCP       map[string]*pyjson.Object
	permissions     map[permissionKey]*loopTask
	admissions      map[string]string
	submitting      map[string]bool
	unacked         *pydict.Dict[string, retainedSubmit]
	settling        map[string]bool
	receipted       *pydict.Dict[string, struct{}]
	cut             map[string]string
	inflight        *pydict.Dict[inflightKey, struct{}]
	lost            map[string]error
	resumes         map[string]int
	adopted         map[string]int
	pendingCancels  map[string]bool
	toolArgs        *pydict.Dict[toolKey, heldTool]
	messageIDs      map[string]*AssistantMessageIDAllocator
	aliases         *pydict.Dict[string, string]
	formElicitation bool
	mutations       PreparedMutations
	titles          map[string]string
	modes           map[string]string
	configs         map[string]*shownConfig
	usage           map[string]*pyjson.Object
	background      map[*loopTask]bool
	answerApproval  func(ctx context.Context, sessionID string, prompt *pyjson.Object) bool
	answerQuestion  func(ctx context.Context, sessionID string, prompt *pyjson.Object) bool
	confirmer       func(sessionID string) MutationConfirm
}

// Gateway projection attached to the profile the launch home's gateway serves
func NewGatewayACPAgent() *GatewayACPAgent {
	ctx := context.Background()
	profileHome, err := gateway.GatewayProfileHome(home.Dir(ctx))
	if err != nil {
		panic(err)
	}
	a := &GatewayACPAgent{
		loop: newCoLoop(), ctx: ctx, connectLock: newCoLock(), home: profileHome,
		snapshots: pydict.New[string, *pyjson.Object](), terminals: pydict.New[string, *pyjson.Object](),
		streamed: map[string]string{}, segments: map[string][]string{}, editorMCP: map[string]*pyjson.Object{},
		permissions: map[permissionKey]*loopTask{}, admissions: map[string]string{}, submitting: map[string]bool{},
		unacked: pydict.New[string, retainedSubmit](), settling: map[string]bool{}, receipted: pydict.New[string, struct{}](),
		cut: map[string]string{}, inflight: pydict.New[inflightKey, struct{}](), lost: map[string]error{}, resumes: map[string]int{},
		adopted: map[string]int{}, pendingCancels: map[string]bool{}, toolArgs: pydict.New[toolKey, heldTool](),
		messageIDs: map[string]*AssistantMessageIDAllocator{}, aliases: pydict.New[string, string](), mutations: NewPreparedMutations(),
		titles: map[string]string{}, modes: map[string]string{}, configs: map[string]*shownConfig{}, usage: map[string]*pyjson.Object{},
		background: map[*loopTask]bool{},
	}
	a.answerApproval, a.answerQuestion, a.confirmer = a.answerPermission, a.answerClarify, a.modelConfirmer
	return a
}

// Agent serving ACP, the gateway projection when no session manager is given
func NewAgent(sessionManager Sessions) any {
	if sessionManager == nil {
		return NewGatewayACPAgent()
	}
	return NewHermesACPAgent(sessionManager)
}

// Stores the editor connection and observes its raw initialize message
func (a *GatewayACPAgent) OnConnect(conn *pyacp.AgentSideConnection) {
	a.loop.enter()
	a.conn = editorConn{conn}
	a.loop.leave()
	conn.Conn().AddObserver(a.observeClientMessage)
}

// Reads the raw initialize capabilities the typed model drops
func (a *GatewayACPAgent) observeClientMessage(event pyacp.StreamEvent) {
	message, ok := event.Message.(*pyjson.Object)
	if event.Direction != pyacp.StreamIncoming || !ok || dget(message, "method") != "initialize" {
		return
	}
	supported := ClientSupportsFormElicitation(dget(message, "params"))
	a.loop.enter()
	a.formElicitation = supported
	a.loop.leave()
}

func isCancelled(err error) bool { return errors.Is(err, context.Canceled) }

// Whether err is an owner refusal carrying exactly this reason
func refusedWith(err error, reason string) bool {
	var refused *gateway.GatewayClientError
	return errors.As(err, &refused) && refused.Msg == reason
}

// Whether err is a client failure, an operating system error or a timeout
func isClientOSOrTimeout(err error) bool {
	var refused *gateway.GatewayClientError
	var opErr *net.OpError
	var pathErr *os.PathError
	var errno syscall.Errno
	return errors.As(err, &refused) || errors.Is(err, os.ErrDeadlineExceeded) || errors.Is(err, context.DeadlineExceeded) ||
		errors.As(err, &opErr) || errors.As(err, &pathErr) || errors.As(err, &errno)
}

// Sends one session update to the editor, the coroutine suspended while it is written
func (a *GatewayACPAgent) sessionUpdate(ctx context.Context, sessionID string, update pyacp.SessionUpdate) error {
	conn := a.conn
	var err error
	if cancelled := a.loop.awaitCtx(ctx, func() { err = conn.SessionUpdate(ctx, sessionID, update, nil) }); cancelled != nil {
		return cancelled
	}
	return err
}

func (a *GatewayACPAgent) say(ctx context.Context, sessionID, text string) error {
	return a.sessionUpdate(ctx, sessionID, pyacp.UpdateAgentMessageText(text))
}

// One owner request, the coroutine suspended until its answer
func (a *GatewayACPAgent) rpc(ctx context.Context, link gatewayLink, method string, params map[string]any) (*pyjson.Object, error) {
	return a.rpcTimeout(ctx, link, method, params, 0)
}

func (a *GatewayACPAgent) rpcTimeout(ctx context.Context, link gatewayLink, method string, params map[string]any, timeout time.Duration) (*pyjson.Object, error) {
	var result *pyjson.Object
	var err error
	if cancelled := a.loop.awaitCtx(ctx, func() { result, err = link.RPC(ctx, method, params, timeout) }); cancelled != nil {
		return nil, cancelled
	}
	return result, err
}

// Owner requests of a prepared mutation, answered as plain maps
func (a *GatewayACPAgent) mutationRPC(link gatewayLink) MutationRPC {
	return func(ctx context.Context, method string, params map[string]any, timeout time.Duration) (map[string]any, error) {
		result, err := a.rpcTimeout(ctx, link, method, params, timeout)
		if err != nil {
			return nil, err
		}
		plain, _ := pyjson.Plain(result).(map[string]any)
		return plain, nil
	}
}

// Answers initialize with the protocol version, agent info, capabilities and auth methods
func (a *GatewayACPAgent) Initialize(ctx context.Context, req *pyacp.InitializeRequest) (*pyacp.InitializeResponse, error) {
	return &pyacp.InitializeResponse{
		ProtocolVersion: pyacp.ProtocolVersion,
		AgentInfo:       &pyacp.Implementation{Name: "icebath-agent", Version: pm.DunderVersion()},
		AgentCapabilities: &pyacp.AgentCapabilities{
			LoadSession:        pyacp.Ptr(true),
			PromptCapabilities: &pyacp.PromptCapabilities{Image: pyacp.Ptr(true)},
			SessionCapabilities: &pyacp.SessionCapabilities{
				Fork: &pyacp.SessionForkCapabilities{}, List: &pyacp.SessionListCapabilities{}, Resume: &pyacp.SessionResumeCapabilities{},
			},
		},
		AuthMethods: BuildAuthMethods(ctx),
	}, nil
}

// Answers authenticate for the detected provider or the terminal setup method
func (a *GatewayACPAgent) Authenticate(ctx context.Context, req *pyacp.AuthenticateRequest) (*pyacp.AuthenticateResponse, error) {
	return authenticate(ctx, req.MethodID), nil
}

// Verified owner connection with its event pump, a dead transport replaced by a fresh one
func (a *GatewayACPAgent) client(ctx context.Context) (gatewayLink, error) {
	if err := a.connectLock.acquire(ctx, a.loop); err != nil {
		return nil, err
	}
	defer a.connectLock.release()
	if a.failure != nil || a.transportClosed() {
		a.teardown()
	}
	if a.gateway == nil {
		var link gatewayLink
		var err error
		if cancelled := a.loop.awaitCtx(ctx, func() { link, err = connectGateway(ctx) }); cancelled != nil {
			return nil, cancelled
		}
		if err != nil {
			return nil, err
		}
		if err := func() error {
			descriptor, err := a.rpc(ctx, link, "runtime.describe", nil)
			if err != nil {
				return err
			}
			if dget(descriptor, "profile_id") != a.home {
				return &gateway.GatewayClientError{Msg: "profile_mismatch"}
			}
			return a.reattach(ctx, link)
		}(); err != nil {
			a.loop.await(func() { link.Close() })
			return nil, err
		}
		a.connection, a.gateway = link, link
		a.eventTask = a.loop.createTask(a.ctx, func(ctx context.Context) (any, error) { return nil, a.events(ctx) }, nil)
	}
	if a.failure != nil {
		return nil, a.failure
	}
	return a.gateway, nil
}

// Subscribes a replacement connection to every session this editor holds
func (a *GatewayACPAgent) reattach(ctx context.Context, link gatewayLink) error {
	for _, sessionID := range a.snapshots.Keys() {
		params := map[string]any{"session_id": sessionID}
		if editor, ok := a.editorMCP[sessionID]; ok {
			params["editor"] = editor
		}
		snapshot, err := a.rpc(ctx, link, "session.resume", params)
		if err != nil {
			var refused *gateway.GatewayRPCError
			if !errors.As(err, &refused) {
				return err
			}
			gatewayServerLog.Info(fmt.Sprintf("ACP session %s not re-attached after reconnect\n%s", sessionID, err))
			a.snapshots.Delete(sessionID)
			continue
		}
		if err := a.adopt(ctx, sessionID, snapshot, nil); err != nil {
			return err
		}
		if err := a.requestLiveReasoning(ctx, link, sessionID); err != nil {
			return err
		}
		prompts, _ := dget(snapshot, "prompts").([]any)
		for _, pending := range prompts {
			if err := a.permission(sessionID, pending); err != nil {
				return err
			}
		}
		if err := a.notifyUnknown(ctx, sessionID); err != nil {
			return err
		}
	}
	return nil
}

// Installs a fresh session.resume snapshot as the session's projection and projects its mode
func (a *GatewayACPAgent) adopt(ctx context.Context, sessionID string, snapshot *pyjson.Object, resumes *int) error {
	a.snapshots.Set(sessionID, snapshot)
	if resumes != nil {
		a.adopted[sessionID] = *resumes
	}
	a.loop.notifyAll()
	info, _ := dget(snapshot, "info").(*pyjson.Object)
	if !truthy(info) {
		info = pyjson.NewObject()
	}
	return a.projectMode(ctx, sessionID, a.editorID(sessionID), info)
}

// Asks the owner to stream this session's reasoning, a refusal or silence only logged
func (a *GatewayACPAgent) requestLiveReasoning(ctx context.Context, link gatewayLink, sessionID string) error {
	_, err := a.rpcTimeout(ctx, link, "config.set", map[string]any{"session_id": sessionID, "key": "show_reasoning", "value": "1"}, 10*time.Second)
	if err != nil {
		if isCancelled(err) {
			return err
		}
		gatewayServerLog.Debug(fmt.Sprintf("ACP session %s keeps its reasoning hidden\n%s", sessionID, err))
	}
	return nil
}

// Tells the editor why the next prompt will be refused and how to recover a lost turn
func (a *GatewayACPAgent) notifyUnknown(ctx context.Context, sessionID string) error {
	if a.hasUnknown(sessionID) && a.conn != nil {
		text := strings.TrimPrefix(a.unknownRecovery(sessionID, "", false), "unknown_execution: ") + "\n"
		return a.say(ctx, a.editorID(sessionID), text)
	}
	return nil
}

func (a *GatewayACPAgent) transportClosed() bool {
	return a.gateway != nil && a.gateway.ReaderDone()
}

// Drops the transport and its pump, every waiter on it waking to the ambiguous failure
func (a *GatewayACPAgent) teardown() {
	task, connection := a.eventTask, a.connection
	a.eventTask, a.connection, a.gateway = nil, nil, nil
	if task != nil {
		task.Cancel()
		a.loop.join(task)
	}
	if connection != nil {
		a.loop.await(func() { connection.Close() })
	}
	clear(a.streamed)
	clear(a.segments)
	clear(a.messageIDs)
	clear(a.cut)
	a.link++
	a.lostFailure = a.failure
	if a.lostFailure == nil {
		a.lostFailure = &gateway.GatewayClientError{Msg: "Gateway disconnected; turn outcome is unknown. Resume the session to check whether " +
			"it completed or was interrupted; do not resend the work."}
	}
	a.failure = nil
	a.loop.notifyAll()
}

// Editor MCP servers as the owner reads them, each server dumped by alias
func mcpServersPayload(servers []pyacp.McpServer) []any {
	out := make([]any, 0, len(servers))
	for _, server := range servers {
		out = append(out, pyacp.Dump(server, pyacp.DumpOptions{ByAlias: true}))
	}
	return out
}

// Creates an owner session for the editor with its MCP servers and launch flags frozen into its policy
func (a *GatewayACPAgent) NewSession(ctx context.Context, req *pyacp.NewSessionRequest) (*pyacp.NewSessionResponse, error) {
	a.loop.enter()
	defer a.loop.leave()
	link, err := a.client(ctx)
	if err != nil {
		return nil, err
	}
	descriptor, err := a.rpc(ctx, link, "runtime.describe", nil)
	if err != nil {
		return nil, err
	}
	sources, _ := dget(asObject(dget(descriptor, "session_create")), "sources").([]any)
	capabilities, _ := dget(descriptor, "capabilities").([]any)
	if !containsValue(sources, "acp") || !containsValue(capabilities, "acp-editor-policy-v1") {
		return nil, &gateway.GatewayClientError{Msg: "acp_policy_unavailable"}
	}
	flags := EnvLaunchFlags()
	params := map[string]any{"request_id": uuidHex(), "source": "acp", "cwd": translateACPCwd(req.Cwd)}
	editor := pyjson.NewObject()
	editor.Set("mcp_servers", mcpServersPayload(req.MCPServers))
	editor.Set("edit_approval_policy", "ask")
	params["editor"] = editor
	for _, key := range flags.Keys {
		params[key] = flags.Vals[key]
	}
	if truthy(dget(flags, "safe_mode")) || truthy(dget(flags, "ignore_user_config")) {
		route, err := bypassRoute()
		if err != nil {
			return nil, err
		}
		for key, value := range route {
			params[key] = value
		}
	}
	snapshot, err := a.rpc(ctx, link, "session.create", params)
	if err != nil {
		return nil, err
	}
	if !dhas(snapshot, "session_id") {
		return nil, errors.New("KeyError: 'session_id'")
	}
	sessionID := pyStr(dget(snapshot, "session_id"))
	if err := a.adopt(ctx, sessionID, snapshot, nil); err != nil {
		return nil, err
	}
	if len(req.MCPServers) > 0 {
		held := pyjson.NewObject()
		held.Set("mcp_servers", mcpServersPayload(req.MCPServers))
		a.editorMCP[sessionID] = held
	}
	info, _ := dget(snapshot, "info").(*pyjson.Object)
	if !truthy(info) {
		if info, err = a.rpc(ctx, link, "session.info", map[string]any{"session_id": sessionID}); err != nil {
			return nil, err
		}
	}
	if err := a.requestLiveReasoning(ctx, link, sessionID); err != nil {
		return nil, err
	}
	fields, err := a.sessionFields(ctx, sessionID, info, sessionID)
	if err != nil {
		return nil, err
	}
	return &pyacp.NewSessionResponse{SessionID: sessionID, Models: fields.models, Modes: fields.modes, Meta: fields.meta, ConfigOptions: fields.configOptions}, nil
}

// Object form of a decoded JSON value, nil for anything else
func asObject(v any) *pyjson.Object {
	obj, _ := v.(*pyjson.Object)
	return obj
}

func containsValue(items []any, want string) bool {
	for _, item := range items {
		if item == want {
			return true
		}
	}
	return false
}

func uuidHex() string { return strings.ReplaceAll(uuid.New().String(), "-", "") }

// Id the editor addresses a session by, its held alias when one names it
func (a *GatewayACPAgent) editorID(sessionID string) string {
	for held, sid := range a.aliases.All() {
		if sid == sessionID {
			return held
		}
	}
	return sessionID
}

// Reply fields shared by new, load, resume and fork
type sessionFields struct {
	models        *pyacp.SessionModelState
	modes         *pyacp.SessionModeState
	meta          *pyjson.Object
	configOptions []pyacp.SessionConfigOption
}

// Model picker, modes, provenance and config options of a session reply, the post attach notices scheduled behind it
func (a *GatewayACPAgent) sessionFields(ctx context.Context, sessionID string, info *pyjson.Object, editorID string) (*sessionFields, error) {
	if title, ok := dget(info, "title").(string); ok && pystr.Strip(title) != "" {
		a.titles[sessionID] = title
	}
	var models *pyacp.SessionModelState
	if cancelled := a.loop.awaitCtx(ctx, func() { models = gatewayModelState(ctx, info) }); cancelled != nil {
		return nil, cancelled
	}
	var meta *pyjson.Object
	var err error
	if cancelled := a.loop.awaitCtx(ctx, func() { meta, err = gatewayProvenanceMeta(ctx, filepath.Join(a.home, "state.db"), editorID, sessionID) }); cancelled != nil {
		return nil, cancelled
	}
	if err != nil {
		return nil, err
	}
	if a.conn != nil {
		a.spawnBackground(ctx, func(ctx context.Context) (any, error) { return nil, a.afterAttach(ctx, sessionID, editorID) })
	}
	shown, ok := a.modes[sessionID]
	if !ok {
		shown = gatewaySessionModes(info).CurrentModeID
		a.modes[sessionID] = shown
	}
	policy := pyjson.NewObject()
	policy.Set("edit_approval_policy", ModeToEditApprovalPolicy[shown])
	modes := gatewaySessionModes(policy)
	return &sessionFields{models: models, modes: modes, meta: meta, configOptions: gatewayConfigOptions(models, info, modes.CurrentModeID)}, nil
}

// Schedules a background coroutine once the current response is written
func (a *GatewayACPAgent) spawnBackground(ctx context.Context, fn func(ctx context.Context) (any, error)) {
	var task *loopTask
	started := make(chan struct{})
	gate := func(taskCtx context.Context) (any, error) {
		var err error
		a.loop.await(func() {
			select {
			case <-started:
			case <-taskCtx.Done():
				err = taskCtx.Err()
			}
		})
		if err != nil {
			return nil, err
		}
		return fn(taskCtx)
	}
	task = a.loop.createTask(a.ctx, gate, func(t *loopTask) { delete(a.background, t) })
	a.background[task] = true
	pyacp.AfterResponse(ctx, func() { close(started) })
}

// Advertises the commands and the context indicator after the session reply
func (a *GatewayACPAgent) afterAttach(ctx context.Context, sessionID, editorID string) error {
	err := func() error {
		if err := a.sessionUpdate(ctx, editorID, gatewayAvailableCommandsUpdate()); err != nil {
			return err
		}
		link, err := a.client(ctx)
		if err != nil {
			return err
		}
		stats, err := a.rpc(ctx, link, "session.stats", map[string]any{"session_id": sessionID})
		if err != nil {
			return err
		}
		return a.projectUsage(ctx, sessionID, editorID, stats)
	}()
	if err != nil {
		if isCancelled(err) {
			return err
		}
		gatewayServerLog.Debug(fmt.Sprintf("ACP post-attach notices for %s not delivered\n%s", sessionID, err))
	}
	return nil
}

// Session info of a session to attach, a thread the in process server wrote adopted through session.resume first
func (a *GatewayACPAgent) attachInfo(ctx context.Context, link gatewayLink, sessionID string) (*pyjson.Object, error) {
	info, err := a.rpc(ctx, link, "session.info", map[string]any{"session_id": sessionID})
	if err == nil {
		return info, nil
	}
	var refused *gateway.GatewayRPCError
	if !errors.As(err, &refused) || refused.Msg != "not_found" {
		return nil, err
	}
	adopted, err := a.rpc(ctx, link, "session.resume", map[string]any{"session_id": sessionID})
	if err != nil {
		return nil, err
	}
	if !dhas(adopted, "subscription_id") {
		return nil, errors.New("KeyError: 'subscription_id'")
	}
	if _, err := a.rpc(ctx, link, "session.detach", map[string]any{"session_id": sessionID, "subscription_id": dget(adopted, "subscription_id")}); err != nil {
		return nil, err
	}
	return a.rpc(ctx, link, "session.info", map[string]any{"session_id": sessionID})
}

// Attaches the editor to an existing session, replaying its live transcript before the reply
func (a *GatewayACPAgent) resume(ctx context.Context, cwd, heldID string, servers []pyacp.McpServer) (*sessionFields, error) {
	link, err := a.client(ctx)
	if err != nil {
		return nil, err
	}
	var sessionID string
	if cancelled := a.loop.awaitCtx(ctx, func() { sessionID, err = logicalSessionID(ctx, filepath.Join(a.home, "state.db"), heldID) }); cancelled != nil {
		return nil, cancelled
	}
	if err != nil {
		return nil, err
	}
	kept := pydict.New[string, string]()
	for held, sid := range a.aliases.All() {
		if sid != sessionID {
			kept.Set(held, sid)
		}
	}
	a.aliases = kept
	if sessionID != heldID {
		a.aliases.Set(heldID, sessionID)
	}
	info, err := a.attachInfo(ctx, link, sessionID)
	if err != nil {
		return nil, err
	}
	if a.moved(info, cwd) {
		if info, err = a.moveWorkspace(ctx, link, sessionID, cwd); err != nil {
			return nil, err
		}
	}
	if !dhas(info, "cwd") && a.conn != nil {
		if err := a.say(ctx, heldID, "Attached to the gateway's existing session policy; editor cwd is not applied.\n"); err != nil {
			return nil, err
		}
	}
	params := map[string]any{"session_id": sessionID}
	var editor *pyjson.Object
	if len(servers) > 0 {
		descriptor, err := a.rpc(ctx, link, "runtime.describe", nil)
		if err != nil {
			return nil, err
		}
		capabilities, _ := dget(descriptor, "capabilities").([]any)
		if !containsValue(capabilities, "acp-session-mcp-v1") {
			return nil, &gateway.GatewayClientError{Msg: "acp_mcp_policy_unavailable"}
		}
		editor = pyjson.NewObject()
		editor.Set("mcp_servers", mcpServersPayload(servers))
		params["editor"] = editor
	}
	snapshot, err := a.rpc(ctx, link, "session.resume", params)
	if err != nil {
		var refused *gateway.GatewayRPCError
		if !errors.As(err, &refused) || refused.Msg != "acp_mcp_policy_conflict" {
			return nil, err
		}
		if err := a.rebindEditorMCP(ctx, link, sessionID, editor); err != nil {
			return nil, err
		}
		if snapshot, err = a.rpc(ctx, link, "session.resume", params); err != nil {
			return nil, err
		}
	}
	if err := a.adopt(ctx, sessionID, snapshot, nil); err != nil {
		return nil, err
	}
	if editor != nil {
		a.editorMCP[sessionID] = editor
	}
	if a.conn != nil {
		if !dhas(snapshot, "messages") {
			return nil, errors.New("KeyError: 'messages'")
		}
		for _, update := range historyReplayUpdates(liveMessages(dget(snapshot, "messages"))) {
			if err := a.sessionUpdate(ctx, heldID, update); err != nil {
				return nil, err
			}
		}
	}
	prompts, _ := dget(snapshot, "prompts").([]any)
	for _, pending := range prompts {
		if err := a.permission(sessionID, pending); err != nil {
			return nil, err
		}
	}
	if err := a.notifyUnknown(ctx, sessionID); err != nil {
		return nil, err
	}
	if err := a.requestLiveReasoning(ctx, link, sessionID); err != nil {
		return nil, err
	}
	merged := pyjson.NewObject()
	for _, key := range info.Keys {
		merged.Set(key, info.Vals[key])
	}
	if snapInfo, ok := dget(snapshot, "info").(*pyjson.Object); ok && truthy(snapInfo) {
		for _, key := range snapInfo.Keys {
			merged.Set(key, snapInfo.Vals[key])
		}
	}
	return a.sessionFields(ctx, sessionID, merged, heldID)
}

// Live messages of a snapshot in stored row order, archived rows left out
func liveMessages(raw any) []map[string]any {
	items, _ := raw.([]any)
	type row struct {
		message map[string]any
		id      float64
	}
	var rows []row
	for _, item := range items {
		obj, ok := item.(*pyjson.Object)
		if !ok || truthy(dget(obj, "archived")) {
			continue
		}
		id := 0.0
		if v := dget(obj, "row_id"); truthy(v) {
			id, _ = pyFloat(v)
		}
		message := make(map[string]any, len(obj.Keys))
		for _, key := range obj.Keys {
			message[key] = obj.Vals[key]
		}
		rows = append(rows, row{message, id})
	}
	sort.SliceStable(rows, func(i, j int) bool { return rows[i].id < rows[j].id })
	out := make([]map[string]any, len(rows))
	for i, r := range rows {
		out[i] = r.message
	}
	return out
}

// Whether the session's cwd differs from the editor's
func (a *GatewayACPAgent) moved(info *pyjson.Object, cwd string) bool {
	return dhas(info, "cwd") && normalizeCwdForCompare(pyStr(dget(info, "cwd"))) != normalizeCwdForCompare(translateACPCwd(cwd))
}

// Moves the session's execution cwd through the owner's workspace mutation and returns the fresh info
func (a *GatewayACPAgent) moveWorkspace(ctx context.Context, link gatewayLink, sessionID, cwd string) (*pyjson.Object, error) {
	payload := pyjson.ObjectOf("cwd", translateACPCwd(cwd))
	a.unacked.Delete(sessionID)
	if _, err := a.mutations.Apply(ctx, a.mutationRPC(link), sessionID, "workspace", payload, nil); err != nil {
		return nil, err
	}
	a.mutations.Acknowledge(sessionID, "workspace", payload)
	return a.rpc(ctx, link, "session.info", map[string]any{"session_id": sessionID})
}

// Rebinds the session's editor MCP servers through the owner's editor_mcp mutation
func (a *GatewayACPAgent) rebindEditorMCP(ctx context.Context, link gatewayLink, sessionID string, editor *pyjson.Object) error {
	a.unacked.Delete(sessionID)
	if _, err := a.mutations.Apply(ctx, a.mutationRPC(link), sessionID, "editor_mcp", editor, nil); err != nil {
		return err
	}
	a.mutations.Acknowledge(sessionID, "editor_mcp", editor)
	return nil
}

// Loads a session, replaying its transcript
func (a *GatewayACPAgent) LoadSession(ctx context.Context, req *pyacp.LoadSessionRequest) (*pyacp.LoadSessionResponse, error) {
	a.loop.enter()
	defer a.loop.leave()
	fields, err := a.resume(ctx, req.Cwd, req.SessionID, req.MCPServers)
	if err != nil {
		return nil, err
	}
	return &pyacp.LoadSessionResponse{Models: fields.models, Modes: fields.modes, Meta: fields.meta, ConfigOptions: fields.configOptions}, nil
}

// Resumes a session, replaying its transcript
func (a *GatewayACPAgent) ResumeSession(ctx context.Context, req *pyacp.ResumeSessionRequest) (*pyacp.ResumeSessionResponse, error) {
	a.loop.enter()
	defer a.loop.leave()
	fields, err := a.resume(ctx, req.Cwd, req.SessionID, req.MCPServers)
	if err != nil {
		return nil, err
	}
	return &pyacp.ResumeSessionResponse{Models: fields.models, Modes: fields.modes, Meta: fields.meta, ConfigOptions: fields.configOptions}, nil
}

// Cancels this editor's own admission, interrupting the turn only when that admission is the one executing
func (a *GatewayACPAgent) cancelAdmission(ctx context.Context, sessionID, admissionID string) error {
	link, err := a.client(ctx)
	if err != nil {
		return err
	}
	_, err = a.rpc(ctx, link, "prompt.cancel", map[string]any{"session_id": sessionID, "admission_id": admissionID})
	if err == nil {
		return nil
	}
	if !refusedWith(err, "stale_generation") {
		return err
	}
	receipt, err := a.rpc(ctx, link, "prompt.receipt", map[string]any{"session_id": sessionID, "admission_id": admissionID})
	if err != nil {
		return err
	}
	if dget(receipt, "status") != "started" {
		return nil
	}
	_, interruptErr := a.rpc(ctx, link, "session.interrupt", map[string]any{"session_id": sessionID, "execution_generation": dget(receipt, "execution_generation")})
	if interruptErr == nil {
		return nil
	}
	if !refusedWith(interruptErr, "stale_generation") {
		return interruptErr
	}
	settled, err := a.rpc(ctx, link, "prompt.receipt", map[string]any{"session_id": sessionID, "admission_id": admissionID})
	if err != nil {
		return err
	}
	if dget(settled, "status") != "terminal" {
		return interruptErr
	}
	return nil
}

// Cancels the editor's prompt on a session, a submit still in flight cancelled once it is acked
func (a *GatewayACPAgent) Cancel(ctx context.Context, req *pyacp.CancelNotification) error {
	a.loop.enter()
	defer a.loop.leave()
	sessionID := a.resolveAlias(req.SessionID)
	if !a.snapshots.Has(sessionID) {
		return &gateway.GatewayClientError{Msg: "not_found"}
	}
	admissionID, ok := a.admissions[sessionID]
	if !ok {
		if a.submitting[sessionID] {
			a.pendingCancels[sessionID] = true
		}
		return nil
	}
	return a.cancelAdmission(ctx, sessionID, admissionID)
}

func (a *GatewayACPAgent) resolveAlias(sessionID string) string {
	if sid, ok := a.aliases.Get(sessionID); ok {
		return sid
	}
	return sessionID
}

// Forks a session through the owner's branch mutation and attaches the child
func (a *GatewayACPAgent) ForkSession(ctx context.Context, req *pyacp.ForkSessionRequest) (*pyacp.ForkSessionResponse, error) {
	a.loop.enter()
	defer a.loop.leave()
	sessionID := a.resolveAlias(req.SessionID)
	link, err := a.client(ctx)
	if err != nil {
		return nil, err
	}
	info, err := a.rpc(ctx, link, "session.info", map[string]any{"session_id": sessionID})
	if err != nil {
		return nil, err
	}
	payload := pyjson.NewObject()
	if len(req.MCPServers) > 0 {
		payload.Set("mcp_servers", mcpServersPayload(req.MCPServers))
	}
	a.unacked.Delete(sessionID)
	result, err := a.mutations.Apply(ctx, a.mutationRPC(link), sessionID, "branch", payload, nil)
	if err != nil {
		return nil, err
	}
	if _, ok := result["branched_session_id"]; !ok {
		return nil, errors.New("KeyError: 'branched_session_id'")
	}
	child := pyStr(result["branched_session_id"])
	if a.moved(info, req.Cwd) {
		if _, err := a.moveWorkspace(ctx, link, child, req.Cwd); err != nil {
			return nil, err
		}
	}
	snapshot, err := a.rpc(ctx, link, "session.resume", map[string]any{"session_id": child})
	if err != nil {
		return nil, err
	}
	if err := a.adopt(ctx, child, snapshot, nil); err != nil {
		return nil, err
	}
	if payload.Len() > 0 {
		a.editorMCP[child] = payload
	} else if parent, ok := a.editorMCP[sessionID]; ok {
		a.editorMCP[child] = parent
	}
	a.mutations.Acknowledge(sessionID, "branch", payload)
	childSnapshot, _ := a.snapshots.Get(child)
	info, _ = dget(childSnapshot, "info").(*pyjson.Object)
	if !truthy(info) {
		if info, err = a.rpc(ctx, link, "session.info", map[string]any{"session_id": child}); err != nil {
			return nil, err
		}
	}
	if err := a.requestLiveReasoning(ctx, link, child); err != nil {
		return nil, err
	}
	fields, err := a.sessionFields(ctx, child, info, child)
	if err != nil {
		return nil, err
	}
	return &pyacp.ForkSessionResponse{SessionID: child, Models: fields.models, Modes: fields.modes, Meta: fields.meta, ConfigOptions: fields.configOptions}, nil
}

// Picks the session's model through the owner's model mutation
func (a *GatewayACPAgent) SetSessionModel(ctx context.Context, req *pyacp.SetSessionModelRequest) (*pyacp.SetSessionModelResponse, error) {
	a.loop.enter()
	defer a.loop.leave()
	return a.setSessionModel(ctx, req.ModelID, req.SessionID)
}

func (a *GatewayACPAgent) setSessionModel(ctx context.Context, modelID, heldID string) (*pyacp.SetSessionModelResponse, error) {
	sessionID := a.resolveAlias(heldID)
	link, err := a.client(ctx)
	if err != nil {
		return nil, err
	}
	var providerName, model string
	if cancelled := a.loop.awaitCtx(ctx, func() { providerName, model = parseModelInput(ctx, modelID, "", nil) }); cancelled != nil {
		return nil, cancelled
	}
	payload := pyjson.ObjectOf("model", model)
	if providerName != "" {
		payload.Set("provider", providerName)
	}
	a.unacked.Delete(sessionID)
	result, err := a.mutations.Apply(ctx, a.mutationRPC(link), sessionID, "model", payload, a.confirmer(sessionID))
	if err != nil {
		return nil, err
	}
	if result["status"] == "cancelled" {
		return nil, &gateway.GatewayClientError{Msg: "model_switch_cancelled"}
	}
	snapshot, err := a.rpc(ctx, link, "session.resume", map[string]any{"session_id": sessionID})
	if err != nil {
		return nil, err
	}
	if err := a.adopt(ctx, sessionID, snapshot, nil); err != nil {
		return nil, err
	}
	a.mutations.Acknowledge(sessionID, "model", payload)
	if result["status"] == "deferred" && a.conn != nil {
		if err := a.say(ctx, a.editorID(sessionID), deferredPickText(result)+"\n"); err != nil {
			return nil, err
		}
	}
	return &pyacp.SetSessionModelResponse{}, nil
}

// Asks the editor to confirm a guarded deferred pick at the turn boundary and tells it the outcome
func (a *GatewayACPAgent) confirmDeferredPick(ctx context.Context, sessionID, editorID string, ask *pyjson.Object) error {
	say := func(text string) error {
		if a.conn != nil {
			return a.say(ctx, editorID, text+"\n")
		}
		return nil
	}
	askPayload, _ := dget(ask, "payload").(*pyjson.Object)
	target := dget(ask, "target_model")
	if !truthy(target) {
		target = dget(askPayload, "model")
	}
	committed := func(reason string) map[string]any {
		return map[string]any{"model": dget(ask, "committed_model"), "provider": dget(ask, "committed_provider"), "target_model": target, "reason": reason}
	}
	plainAsk, _ := pyjson.Plain(ask).(map[string]any)
	err := func() error {
		confirm := a.confirmer(sessionID)
		if confirm == nil {
			return say(refusedPickText(committed("not confirmed")))
		}
		accepted, err := confirm(ctx, plainAsk)
		if err != nil {
			return err
		}
		if !accepted {
			return say(refusedPickText(committed("not confirmed")))
		}
		link, err := a.client(ctx)
		if err != nil {
			return err
		}
		snapshot, err := a.rpc(ctx, link, "session.resume", map[string]any{"session_id": sessionID})
		if err != nil {
			return err
		}
		payload := pyjson.NewObject()
		if askPayload != nil {
			for _, key := range askPayload.Keys {
				payload.Set(key, askPayload.Vals[key])
			}
		}
		payload.Set("confirm", dget(ask, "confirm"))
		result, err := a.rpc(ctx, link, "session.mutate", map[string]any{"session_id": sessionID, "operation": "model", "request_id": uuidHex(),
			"expected_revision": dget(snapshot, "revision"), "expected_generation": dget(snapshot, "execution_generation"), "payload": payload})
		if err != nil {
			return err
		}
		plain, _ := pyjson.Plain(result).(map[string]any)
		if plain["status"] == "confirmation_required" {
			return say(ModelRefusalText(plain, true))
		}
		if a.snapshots.Has(sessionID) {
			fresh, err := a.rpc(ctx, link, "session.resume", map[string]any{"session_id": sessionID})
			if err != nil {
				return err
			}
			if err := a.adopt(ctx, sessionID, fresh, nil); err != nil {
				return err
			}
		}
		if plain["status"] == "deferred" {
			return say(deferredPickText(plain))
		}
		return say(modelText("Model switched to", plain))
	}()
	var refused *gateway.GatewayClientError
	if err != nil && errors.As(err, &refused) {
		gatewayServerLog.Info(fmt.Sprintf("Deferred model pick on %s not confirmed: %s", sessionID, err))
		return say(refusedPickText(committed(refused.Msg)))
	}
	return err
}

// Owner's guarded model confirmation as an ACP permission request, nil without an editor connection
func (a *GatewayACPAgent) modelConfirmer(sessionID string) MutationConfirm {
	if a.conn == nil {
		return nil
	}
	return func(ctx context.Context, refusal map[string]any) (bool, error) {
		title := ConfirmationTitle(refusal)
		target := ""
		if v, ok := refusal["target_model"]; ok {
			target = pyStr(v)
		}
		if _, ok := refusal["confirm_message"]; !ok {
			return false, errors.New("KeyError: 'confirm_message'")
		}
		guards := []any{}
		if warnings, ok := refusal["warnings"].([]any); ok {
			for _, w := range warnings {
				warning, _ := w.(map[string]any)
				guards = append(guards, warning["kind"])
			}
		}
		rawInput := pyjson.NewObject()
		rawInput.Set("model", refusal["target_model"])
		rawInput.Set("provider", refusal["target_provider"])
		rawInput.Set("guards", guards)
		kind, status := "other", "pending"
		toolCall := pyacp.UpdateToolCall("model-confirm-"+uuidHex()[:12], pyacp.ToolCallFields{
			Title: pyacp.Ptr(title + ": " + target), Kind: &kind, Status: &status,
			Content:  []pyacp.ToolCallContent{pyacp.ToolContent(pyacp.TextBlock(pyStr(refusal["confirm_message"])))},
			RawInput: rawInput})
		options := []pyacp.PermissionOption{
			{OptionID: "allow_once", Kind: "allow_once", Name: i18n.T(ctx, "cli.model.choice_switch_anyway", "", nil)},
			{OptionID: "deny", Kind: "reject_once", Name: i18n.T(ctx, "cli.model.choice_cancel", "", nil)},
		}
		response, err := a.requestPermission(ctx, a.editorID(sessionID), toolCall, options)
		if err != nil {
			return false, err
		}
		allowed, ok := response.Outcome.(*pyacp.AllowedOutcome)
		return ok && allowed.OptionID == "allow_once", nil
	}
}

// Asks the editor to pick one permission option for a tool call
func (a *GatewayACPAgent) requestPermission(ctx context.Context, editorID string, toolCall *pyacp.ToolCallProgress, options []pyacp.PermissionOption) (*pyacp.RequestPermissionResponse, error) {
	conn := a.conn
	var response *pyacp.RequestPermissionResponse
	var err error
	if cancelled := a.loop.awaitCtx(ctx, func() { response, err = requestPermissionVia(conn)(ctx, editorID, toolCall, options) }); cancelled != nil {
		return nil, cancelled
	}
	return response, err
}

// Logical id of a session this editor holds, not_found otherwise
func (a *GatewayACPAgent) heldSession(sessionID string) (string, error) {
	sessionID = a.resolveAlias(sessionID)
	if !a.snapshots.Has(sessionID) {
		return "", &gateway.GatewayClientError{Msg: "not_found"}
	}
	return sessionID, nil
}

// Sets the session's edit approval policy on the owner from the editor's mode
func (a *GatewayACPAgent) SetSessionMode(ctx context.Context, req *pyacp.SetSessionModeRequest) (*pyacp.SetSessionModeResponse, error) {
	a.loop.enter()
	defer a.loop.leave()
	policy, ok := ModeToEditApprovalPolicy[req.ModeID]
	if !ok {
		return nil, &gateway.GatewayClientError{Msg: "invalid_params"}
	}
	sessionID, err := a.heldSession(req.SessionID)
	if err != nil {
		return nil, err
	}
	if err := a.setEditPolicy(ctx, sessionID, policy); err != nil {
		return nil, err
	}
	return &pyacp.SetSessionModeResponse{}, nil
}

// Applies one of the advertised pickers and answers the full option list read back from the owner
func (a *GatewayACPAgent) SetConfigOption(ctx context.Context, req *pyacp.SetConfigOptionParams) (*pyacp.SetSessionConfigOptionResponse, error) {
	a.loop.enter()
	defer a.loop.leave()
	editorID := req.SessionID
	sessionID, err := a.heldSession(req.SessionID)
	if err != nil {
		return nil, err
	}
	value, isText := req.Value.(string)
	switch req.ConfigID {
	case EditApprovalPolicyConfigID:
		if _, ok := EditApprovalPolicyToMode[value]; !ok || !isText {
			return nil, &gateway.GatewayClientError{Msg: "invalid_params"}
		}
		if err := a.setEditPolicy(ctx, sessionID, value); err != nil {
			return nil, err
		}
	case "model":
		if !isText {
			return nil, &gateway.GatewayClientError{Msg: "invalid_params"}
		}
		if _, err := a.setSessionModel(ctx, value, editorID); err != nil {
			return nil, err
		}
	case ReasoningConfigID:
		valid := false
		for _, choice := range reasoningChoices() {
			valid = valid || (isText && choice == value)
		}
		if !valid {
			return nil, &gateway.GatewayClientError{Msg: "invalid_params"}
		}
		var effort any
		if value != "default" {
			effort = value
		}
		if err := a.idleControl(ctx, sessionID, "reasoning", pyjson.ObjectOf("effort", effort), "Reasoning effort"); err != nil {
			return nil, err
		}
	}
	link, err := a.client(ctx)
	if err != nil {
		return nil, err
	}
	info, err := a.rpc(ctx, link, "session.info", map[string]any{"session_id": sessionID})
	if err != nil {
		return nil, err
	}
	var models *pyacp.SessionModelState
	if cancelled := a.loop.awaitCtx(ctx, func() { models = gatewayModelState(ctx, info) }); cancelled != nil {
		return nil, cancelled
	}
	mode := a.modes[sessionID]
	if mode == "" {
		mode = gatewayCurrentMode(info)
	}
	return &pyacp.SetSessionConfigOptionResponse{ConfigOptions: gatewayConfigOptions(models, info, mode)}, nil
}

// Commits the session's edit approval policy on the owner
func (a *GatewayACPAgent) setEditPolicy(ctx context.Context, sessionID, policy string) error {
	if err := a.idleControl(ctx, sessionID, "edit_policy", pyjson.ObjectOf("policy", policy), "Mode"); err != nil {
		return err
	}
	a.modes[sessionID] = EditApprovalPolicyToMode[policy]
	return nil
}

// Revision fenced owner mutation that applies to idle sessions only
func (a *GatewayACPAgent) idleControl(ctx context.Context, sessionID, operation string, payload *pyjson.Object, label string) error {
	link, err := a.client(ctx)
	if err != nil {
		return err
	}
	a.unacked.Delete(sessionID)
	if _, err := a.mutations.Apply(ctx, a.mutationRPC(link), sessionID, operation, payload, nil); err != nil {
		var refused *gateway.GatewayClientError
		if !errors.As(err, &refused) || (refused.Msg != "session_busy" && refused.Msg != "unknown_execution") {
			return err
		}
		text := label + " changes apply when the session is idle; try again after this turn."
		if refused.Msg != "session_busy" {
			text = label + " changes wait for the lost turn: " + strings.TrimPrefix(a.unknownRecovery(sessionID, "", false), "unknown_execution: ")
		}
		if a.conn != nil {
			if err := a.say(ctx, a.editorID(sessionID), text+"\n"); err != nil {
				return err
			}
		}
		return pyacp.NewRequestError(-32603, text, nil)
	}
	a.mutations.Acknowledge(sessionID, operation, payload)
	return nil
}

// Lists the profile's ACP sessions after the cursor, fifty per page
func (a *GatewayACPAgent) ListSessions(ctx context.Context, req *pyacp.ListSessionsRequest) (*pyacp.ListSessionsResponse, error) {
	a.loop.enter()
	defer a.loop.leave()
	cwd, cursor := "", ""
	if req.Cwd != nil {
		cwd = *req.Cwd
	}
	if req.Cursor != nil {
		cursor = *req.Cursor
	}
	var rows []*pyjson.Object
	var err error
	if cancelled := a.loop.awaitCtx(ctx, func() { rows, err = CatalogSessions(ctx, filepath.Join(a.home, "state.db"), cwd) }); cancelled != nil {
		return nil, cancelled
	}
	if err != nil {
		return nil, err
	}
	if cursor != "" {
		rest := []*pyjson.Object{}
		for i, row := range rows {
			if dget(row, "session_id") == cursor {
				rest = rows[i+1:]
				break
			}
		}
		rows = rest
	}
	more := len(rows) > 50
	page := []pyacp.SessionInfo{}
	for _, row := range rows[:min(len(rows), 50)] {
		info := pyacp.SessionInfo{SessionID: pyStr(dget(row, "session_id")), Cwd: pyStr(dget(row, "cwd"))}
		if title := dget(row, "title"); title != nil {
			info.Title = pyacp.Ptr(pyStr(title))
		}
		if updated := dget(row, "updated_at"); updated != nil {
			info.UpdatedAt = pyacp.Ptr(pyStr(updated))
		}
		page = append(page, info)
	}
	resp := &pyacp.ListSessionsResponse{Sessions: page}
	if more {
		resp.NextCursor = pyacp.Ptr(page[len(page)-1].SessionID)
	}
	return resp, nil
}

// Submits the editor's prompt to the owner and waits for its turn to settle
func (a *GatewayACPAgent) Prompt(ctx context.Context, req *pyacp.PromptRequest) (*pyacp.PromptResponse, error) {
	a.loop.enter()
	defer a.loop.leave()
	return a.prompt(ctx, req.Prompt, req.SessionID)
}

func (a *GatewayACPAgent) prompt(ctx context.Context, blocks []pyacp.ContentBlock, heldID string) (*pyacp.PromptResponse, error) {
	sessionID := a.resolveAlias(heldID)
	if !a.snapshots.Has(sessionID) {
		return nil, &gateway.GatewayClientError{Msg: "not_found"}
	}
	if a.hasUnknown(sessionID) {
		cancelled, err := a.refreshUnknownForPrompt(ctx, sessionID)
		if err != nil {
			return nil, err
		}
		if cancelled {
			return &pyacp.PromptResponse{StopReason: "cancelled"}, nil
		}
	}
	if a.hasUnknown(sessionID) {
		content, err := ContentBlocksToOpenAIUserContent(blocks)
		if err != nil {
			return nil, err
		}
		text, isText := content.(string)
		if command, ok := slashCommandOf(text); !isText || !ok || command != "discard" {
			return nil, &gateway.GatewayClientError{Msg: a.unknownRecovery(sessionID, "", false)}
		}
	}
	prepared, err := a.prepareSubmit(ctx, sessionID, blocks)
	if err != nil {
		return nil, err
	}
	if prepared == nil {
		return &pyacp.PromptResponse{StopReason: "cancelled"}, nil
	}
	text := prepared.text
	if len(prepared.attachments) == 0 {
		answered, submit, err := a.runSlash(ctx, heldID, sessionID, text)
		if err != nil {
			return nil, err
		}
		if answered {
			return &pyacp.PromptResponse{StopReason: "end_turn"}, nil
		}
		text = submit
	}
	a.mutations.Retire(sessionID)
	submit := map[string]any{"session_id": sessionID, "input_id": prepared.inputID, "text": text}
	if len(prepared.attachments) > 0 {
		attachments := make([]any, 0, len(prepared.attachments))
		for _, staged := range prepared.attachments {
			attachments = append(attachments, pyjson.ObjectOf("path", staged.Path, "mime", staged.Mime))
		}
		submit["attachments"] = attachments
	}
	a.submitting[sessionID] = true
	var link gatewayLink
	var linkID, resumes int
	receipt, err := func() (receipt *pyjson.Object, err error) {
		defer delete(a.submitting, sessionID)
		defer func() {
			if r := recover(); r != nil {
				a.retainUnacked(sessionID, prepared)
				delete(a.pendingCancels, sessionID)
				panic(r)
			}
		}()
		if link, err = a.client(ctx); err == nil {
			linkID, resumes = a.link, a.resumes[sessionID]
			receipt, err = a.rpc(ctx, link, "prompt.submit", submit)
		}
		if err != nil {
			var refused *gateway.GatewayRPCError
			if !errors.As(err, &refused) {
				a.retainUnacked(sessionID, prepared)
			}
			delete(a.pendingCancels, sessionID)
		}
		return receipt, err
	}()
	if err != nil {
		return nil, err
	}
	admissionID, admitted, err := a.admissionFromReceipt(ctx, link, heldID, sessionID, receipt)
	if err != nil {
		return nil, err
	}
	if !admitted {
		return &pyacp.PromptResponse{StopReason: "end_turn"}, nil
	}
	a.inflight.Set(inflightKey{sessionID, admissionID}, struct{}{})
	terminal, err := func() (*pyjson.Object, error) {
		defer func() {
			if a.admissions[sessionID] == admissionID {
				delete(a.admissions, sessionID)
			}
			a.inflight.Delete(inflightKey{sessionID, admissionID})
		}()
		if err := a.afterAck(ctx, sessionID, admissionID, linkID, resumes); err != nil {
			return nil, err
		}
		if a.pendingCancels[sessionID] {
			delete(a.pendingCancels, sessionID)
			if err := a.cancelAdmission(ctx, sessionID, admissionID); err != nil {
				return nil, err
			}
		}
		if err := a.loop.waitFor(ctx, func() bool {
			_, lost := a.lost[admissionID]
			return a.terminals.Has(admissionID) || a.failure != nil || a.link != linkID || lost || a.blockedAdmission(sessionID, admissionID)
		}); err != nil {
			return nil, err
		}
		if !a.terminals.Has(admissionID) {
			if a.link != linkID {
				return nil, a.lostFailure
			}
			if a.failure != nil {
				return nil, a.failure
			}
			if lost, ok := a.lost[admissionID]; ok {
				delete(a.lost, admissionID)
				if lost != nil {
					return nil, lost
				}
			}
			return nil, &gateway.GatewayClientError{Msg: a.unknownRecovery(sessionID, admissionID, true)}
		}
		terminal, _ := a.terminals.Get(admissionID)
		a.terminals.Delete(admissionID)
		return terminal, nil
	}()
	if err != nil {
		return nil, err
	}
	switch dget(terminal, "outcome") {
	case "unknown":
		return nil, &gateway.GatewayClientError{Msg: a.unknownRecovery(sessionID, admissionID, true)}
	case "failed":
		return nil, &gateway.GatewayClientError{Msg: "admitted_turn_failed"}
	case "cancelled":
		return &pyacp.PromptResponse{StopReason: "cancelled", Usage: gatewayTurnUsage(a.usageOf(sessionID))}, nil
	}
	return &pyacp.PromptResponse{StopReason: "end_turn", Usage: gatewayTurnUsage(a.usageOf(sessionID))}, nil
}

func (a *GatewayACPAgent) usageOf(sessionID string) any {
	if stats, ok := a.usage[sessionID]; ok {
		return stats
	}
	return nil
}

// Keeps the identity of a submit whose ack never arrived, the latest sixty four sessions
func (a *GatewayACPAgent) retainUnacked(sessionID string, prepared *retainedSubmit) {
	a.unacked.Delete(sessionID)
	a.unacked.Set(sessionID, *prepared)
	for a.unacked.Len() > 64 {
		a.unacked.Delete(a.unacked.Keys()[0])
	}
}

// Admission a prompt.submit receipt names, false when the owner admitted nothing
func (a *GatewayACPAgent) admissionFromReceipt(ctx context.Context, link gatewayLink, heldID, sessionID string, receipt *pyjson.Object) (string, bool, error) {
	if dget(receipt, "voice_stopped") == true {
		delete(a.pendingCancels, sessionID)
		if a.conn != nil {
			if err := a.say(ctx, heldID, i18n.T(ctx, "cli.voice.stop_phrase_typed", "", nil)+"\n"); err != nil {
				return "", false, err
			}
		}
		return "", false, nil
	}
	if !dhas(receipt, "admission_id") {
		return "", false, errors.New("KeyError: 'admission_id'")
	}
	admissionID := pyStr(dget(receipt, "admission_id"))
	a.admissions[sessionID] = admissionID
	switch dget(receipt, "status") {
	case "terminal":
		if err := a.settledReceipt(ctx, link, heldID, sessionID, receipt, nil); err != nil {
			return "", false, err
		}
	case "unknown":
		delete(a.admissions, sessionID)
		return "", false, &gateway.GatewayClientError{Msg: a.unknownRecovery(sessionID, admissionID, true)}
	}
	return admissionID, true, nil
}

// Runs the local half of a slash command, answered ending the prompt and the text otherwise submitted
func (a *GatewayACPAgent) runSlash(ctx context.Context, heldID, sessionID, text string) (bool, string, error) {
	command, ok := slashCommandOf(text)
	if !ok {
		return false, text, nil
	}
	arg := ""
	if parts := pystr.SplitWhitespace(pystr.Strip(text), 1); len(parts) > 1 {
		arg = pystr.Strip(parts[1])
	}
	reply, submit, err := runGatewayCommand(ctx, a, sessionID, command, arg)
	answered := submit == nil
	if err != nil {
		var refused *CommandRefused
		if !errors.As(err, &refused) {
			return false, "", err
		}
		reply, answered = refused.Msg, true
	}
	if !answered {
		return false, *submit, nil
	}
	if a.conn != nil {
		if err := a.say(ctx, heldID, reply+"\n"); err != nil {
			return false, "", err
		}
	}
	return true, "", nil
}

// Retry key, input id and staged content of one prompt, nil when the editor's Stop arrived while it was staged
func (a *GatewayACPAgent) prepareSubmit(ctx context.Context, sessionID string, blocks []pyacp.ContentBlock) (*retainedSubmit, error) {
	content, err := ContentBlocksToOpenAIUserContent(blocks)
	if err != nil {
		return nil, err
	}
	key := submitFingerprint(sessionID, content)
	retained, had := a.unacked.Get(sessionID)
	a.unacked.Delete(sessionID)
	if had && retained.key == key {
		return &retained, nil
	}
	a.submitting[sessionID] = true
	var text string
	var attachments []StagedAttachment
	cancelled := a.loop.awaitCtx(ctx, func() { text, attachments, err = stageUserContent(ctx, content, a.home) })
	delete(a.submitting, sessionID)
	if cancelled != nil {
		return nil, cancelled
	}
	if err != nil {
		return nil, err
	}
	if a.pendingCancels[sessionID] {
		delete(a.pendingCancels, sessionID)
		return nil, nil
	}
	return &retainedSubmit{key: key, inputID: uuidHex(), text: text, attachments: attachments}, nil
}

// Hands an admission acked during a gap re-attach to the pump, which recovers it against that snapshot
func (a *GatewayACPAgent) afterAck(ctx context.Context, sessionID, admissionID string, linkID, resumes int) error {
	if a.resumes[sessionID] == resumes {
		return nil
	}
	if err := a.loop.waitFor(ctx, func() bool {
		return a.adopted[sessionID] > resumes || a.failure != nil || a.link != linkID
	}); err != nil {
		return err
	}
	if a.adopted[sessionID] > resumes {
		link, err := a.currentGateway()
		if err != nil {
			return err
		}
		gateway.PutBehind(link.Events(), recoveryRequest{sessionID, admissionID})
	}
	return nil
}

// Owner connection in use, an error when none is attached
func (a *GatewayACPAgent) currentGateway() (gatewayLink, error) {
	if a.gateway == nil {
		return nil, errors.New("AttributeError: 'NoneType' object has no attribute 'rpc'")
	}
	return a.gateway, nil
}

// Recovers one admission across a replay gap from the snapshot and, when it ended in the hole, its receipt
func (a *GatewayACPAgent) recover(ctx context.Context, sessionID, admissionID string) error {
	snapshot, _ := a.snapshots.Get(sessionID)
	pending, _ := dget(snapshot, "pending").([]any)
	plainPending, _ := pyjson.Plain(pending).([]any)
	status := ReplayGapPendingStatus(plainPending, admissionID)
	if status == "queued" || status == "unknown" || a.terminals.Has(admissionID) || a.settling[admissionID] || a.receipted.Has(admissionID) {
		return nil
	}
	streamed := a.streamed[admissionID]
	delete(a.streamed, admissionID)
	if _, ok := a.cut[admissionID]; !ok {
		a.cut[admissionID] = streamed
	}
	if truthy(status) {
		return nil
	}
	link, err := a.currentGateway()
	if err != nil {
		return err
	}
	receipt, err := a.rpc(ctx, link, "prompt.receipt", map[string]any{"session_id": sessionID, "admission_id": admissionID, "include_result": true})
	if err != nil {
		if !isClientOSOrTimeout(err) {
			return err
		}
		receipt = pyjson.ObjectOf("status", "terminal")
	}
	plainReceipt, _ := pyjson.Plain(receipt).(map[string]any)
	verdict := ReplayGapSettled(plainReceipt)
	if verdict == nil || *verdict {
		if !a.receipted.Has(admissionID) {
			a.receipted.Set(admissionID, struct{}{})
		}
	}
	if verdict != nil && *verdict {
		return a.settledReceipt(ctx, link, a.editorID(sessionID), sessionID, receipt, receipt)
	}
	if verdict == nil && !a.terminals.Has(admissionID) && !a.settling[admissionID] {
		a.lost[admissionID] = &gateway.GatewayClientError{Msg: "session_replay_gap"}
		a.loop.notifyAll()
	}
	return nil
}

// Reports a turn that already settled from its terminal receipt, once
func (a *GatewayACPAgent) settledReceipt(ctx context.Context, link gatewayLink, heldID, sessionID string, receipt, saved *pyjson.Object) error {
	if !dhas(receipt, "admission_id") {
		return errors.New("KeyError: 'admission_id'")
	}
	admissionID := pyStr(dget(receipt, "admission_id"))
	if !truthy(saved) {
		var err error
		if saved, err = a.rpc(ctx, link, "prompt.receipt", map[string]any{"session_id": sessionID, "admission_id": admissionID, "include_result": true}); err != nil {
			return err
		}
	}
	if a.terminals.Has(admissionID) || a.settling[admissionID] {
		return nil
	}
	a.settling[admissionID] = true
	defer delete(a.settling, admissionID)
	a.receipted.Set(admissionID, struct{}{})
	for a.receipted.Len() > 256 {
		a.receipted.Delete(a.receipted.Keys()[0])
	}
	outcome := dget(receipt, "outcome")
	if outcome == "interrupted" {
		outcome = "cancelled"
	}
	result, _ := dget(saved, "result").(*pyjson.Object)
	final := ""
	if v := dget(result, "final_response"); truthy(v) {
		final = pyStr(v)
	}
	payload := pyjson.ObjectOf("text", final, "outcome", outcome, "response_reused", dget(result, "response_reused"), "receipt", true)
	if err := a.finishReply(ctx, sessionID, heldID, admissionID, payload); err != nil {
		return err
	}
	a.terminals.Set(admissionID, pyjson.ObjectOf("outcome", outcome))
	a.loop.notifyAll()
	return nil
}

// Refusal for a FIFO blocked by a lost turn, naming the recovery control
func (a *GatewayACPAgent) unknownRecovery(sessionID, admissionID string, submitted bool) string {
	var lost []string
	if snapshot, ok := a.snapshots.Get(sessionID); ok {
		pending, _ := dget(snapshot, "pending").([]any)
		for _, raw := range pending {
			if row := asObject(raw); dget(row, "status") == "unknown" {
				lost = append(lost, pyStr(dget(row, "admission_id")))
			}
		}
	}
	if len(lost) == 0 {
		if admissionID == "" {
			lost = []string{"None"}
		} else {
			lost = []string{admissionID}
		}
	}
	command := "icebath"
	if profile, ok := gateway.ProfileNameForHome(a.home); ok && profile != "default" {
		command += " -p " + profile
	}
	state := "nothing was submitted"
	if submitted {
		state = "your input is accepted and kept"
	}
	discards := make([]string, len(lost))
	for i, id := range lost {
		discards[i] = "`/discard " + id + "`"
	}
	return fmt.Sprintf("unknown_execution: do not resend accepted input; a turn in this session was lost when the "+
		"gateway restarted and its outcome is unknown (%s). Send %s here (or `%s sessions discard %s` in a terminal), then continue.",
		state, strings.Join(discards, " and "), command, sessionID)
}

func (a *GatewayACPAgent) hasUnknown(sessionID string) bool {
	snapshot, _ := a.snapshots.Get(sessionID)
	pending, _ := dget(snapshot, "pending").([]any)
	for _, raw := range pending {
		if dget(asObject(raw), "status") == "unknown" {
			return true
		}
	}
	return false
}

// Refreshes a cached lost turn before a prompt, true when the editor's Stop arrived during the read
func (a *GatewayACPAgent) refreshUnknownForPrompt(ctx context.Context, sessionID string) (bool, error) {
	a.submitting[sessionID] = true
	err := a.refreshUnknown(ctx, sessionID)
	delete(a.submitting, sessionID)
	if err != nil {
		delete(a.pendingCancels, sessionID)
		return false, err
	}
	cancelled := a.pendingCancels[sessionID]
	delete(a.pendingCancels, sessionID)
	return cancelled, nil
}

func snapshotWatermark(snapshot *pyjson.Object) pendingWatermark {
	if mark, ok := dget(snapshot, "_pending_watermark").(pendingWatermark); ok {
		return mark
	}
	sequence, _ := pyFloat(dgetOr(snapshot, "last_sequence", int64(0)))
	return pendingWatermark{epoch: dget(snapshot, "replay_epoch"), sequence: sequence}
}

// Asks the owner whether another viewer has acknowledged a cached lost turn
func (a *GatewayACPAgent) refreshUnknown(ctx context.Context, sessionID string) error {
	link, err := a.client(ctx)
	if err != nil {
		return err
	}
	cached, ok := a.snapshots.Get(sessionID)
	if !ok {
		return &gateway.GatewayClientError{Msg: "not_found"}
	}
	if !a.hasUnknown(sessionID) {
		return nil
	}
	if !dhas(cached, "_pending_watermark") {
		cached.Set("_pending_watermark", snapshotWatermark(cached))
	}
	fresh, err := a.rpc(ctx, link, "session.resume", map[string]any{"session_id": sessionID})
	if err != nil {
		return err
	}
	if cached, ok = a.snapshots.Get(sessionID); !ok {
		return &gateway.GatewayClientError{Msg: "not_found"}
	}
	mark := snapshotWatermark(cached)
	freshSequence, _ := pyFloat(dgetOr(fresh, "last_sequence", int64(0)))
	if dhas(fresh, "pending") && (!pyrepr.Equal(dget(fresh, "replay_epoch"), mark.epoch) || freshSequence >= mark.sequence) {
		cached.Set("pending", dget(fresh, "pending"))
		cached.Set("_pending_watermark", pendingWatermark{epoch: dget(fresh, "replay_epoch"), sequence: freshSequence})
	}
	return nil
}

// Whether a pending event is newer than the snapshot's pending watermark
func (a *GatewayACPAgent) pendingEventIsNew(sessionID string, event *pyjson.Object) bool {
	snapshot, _ := a.snapshots.Get(sessionID)
	fence, ok := dget(snapshot, "_pending_watermark").(pendingWatermark)
	sequence := dget(event, "seq")
	if !ok || sequence == nil || !pyrepr.Equal(dget(event, "replay_epoch"), fence.epoch) {
		return true
	}
	seq, _ := pyFloat(sequence)
	return seq > fence.sequence
}

func (a *GatewayACPAgent) blockedAdmission(sessionID, admissionID string) bool {
	if !a.hasUnknown(sessionID) {
		return false
	}
	snapshot, _ := a.snapshots.Get(sessionID)
	pending, _ := dget(snapshot, "pending").([]any)
	for _, raw := range pending {
		row := asObject(raw)
		status := dget(row, "status")
		if dget(row, "admission_id") == admissionID && (status == "queued" || status == "unknown") {
			return true
		}
	}
	return false
}

// Event pump of one owner connection, a projection failure reaching every waiter
func (a *GatewayACPAgent) events(ctx context.Context) error {
	link := a.gateway
	err := func() error {
		for {
			var frame any
			var err error
			a.loop.await(func() { frame, err = link.Events().Get(ctx) })
			if err != nil {
				return err
			}
			switch item := frame.(type) {
			case error:
				return item
			case recoveryRequest:
				if err := a.recover(ctx, item.sessionID, item.admissionID); err != nil {
					return err
				}
				continue
			case *pyjson.Object:
				event, _ := dget(item, "params").(*pyjson.Object)
				if event == nil {
					event = pyjson.NewObject()
				}
				sid, _ := dget(event, "session_id").(string)
				snapshot, ok := a.snapshots.Get(sid)
				if !ok {
					continue
				}
				epoch := dget(event, "replay_epoch")
				seq, _ := pyFloat(dgetOr(event, "seq", int64(0)))
				last, _ := pyFloat(dgetOr(snapshot, "last_sequence", int64(0)))
				if pyrepr.Equal(epoch, dget(snapshot, "replay_epoch")) && seq <= last {
					continue
				}
				snapshot.Set("replay_epoch", epoch)
				snapshot.Set("last_sequence", dgetOr(event, "seq", int64(0)))
				snapshot.Set("execution_generation", dgetOr(event, "execution_generation", dget(snapshot, "execution_generation")))
				if err := a.project(ctx, event); err != nil {
					return err
				}
			}
		}
	}()
	if isCancelled(err) && ctx.Err() != nil {
		return err
	}
	gatewayServerLog.Debug(fmt.Sprintf("ACP gateway event projection stopped\n%s", err))
	a.failure = err
	a.loop.notifyAll()
	return nil
}

// Projects one owner event onto the editor
func (a *GatewayACPAgent) project(ctx context.Context, event *pyjson.Object) error {
	kind, _ := dget(event, "type").(string)
	payload, _ := dget(event, "payload").(*pyjson.Object)
	if payload == nil {
		payload = pyjson.NewObject()
	}
	sid := pyStr(dget(event, "session_id"))
	editorID := a.editorID(sid)
	aid := ""
	if v := dget(event, "admission_id"); v != nil {
		aid = pyStr(v)
	}
	switch kind {
	case "session.info", "session.updated", "session.usage", "session.tools_changed":
		return a.projectSessionState(ctx, kind, sid, editorID, payload, event)
	}
	consumed, err := a.gapGuard(ctx, kind, sid, aid)
	if err != nil || consumed {
		return err
	}
	switch kind {
	case "approval.request", "clarify.request":
		return a.permission(sid, payload)
	case "approval.settled", "clarify.settled":
		if !dhas(payload, "prompt_id") || !dhas(payload, "execution_generation") {
			return errors.New("KeyError: 'prompt_id'")
		}
		key := permissionKey{sid, pyrepr.Repr(dget(payload, "prompt_id")), pyrepr.Repr(dget(payload, "execution_generation"))}
		if task, ok := a.permissions[key]; ok {
			delete(a.permissions, key)
			task.Cancel()
		}
		return nil
	case "reasoning.delta", "todo.updated":
		return a.projectSideUpdate(ctx, kind, sid, editorID, payload)
	case "tool.start", "tool.complete":
		return a.projectTool(ctx, kind, sid, editorID, aid, payload)
	case "message.delta":
		text := replyText(payload)
		a.streamed[aid] += text
		if text != "" && a.conn != nil {
			return a.sessionUpdate(ctx, editorID, a.chunk(sid, text, false))
		}
	case "message.interim":
		a.closeSegment(aid)
		text := replyText(payload)
		a.endMessage(sid)
		if !truthy(dget(payload, "already_streamed")) && pystr.Strip(text) != "" {
			a.segments[aid] = append(a.segments[aid], text)
			if a.conn != nil {
				if err := a.sessionUpdate(ctx, editorID, a.chunk(sid, text+"\n\n", false)); err != nil {
					return err
				}
			}
			a.endMessage(sid)
		}
	case "message.complete":
		if a.settling[aid] || a.receipted.Has(aid) {
			delete(a.streamed, aid)
			delete(a.segments, aid)
			return nil
		}
		a.settling[aid] = true
		defer delete(a.settling, aid)
		if err := a.finishReply(ctx, sid, editorID, aid, payload); err != nil {
			return err
		}
		a.terminals.Set(aid, payload)
		if a.terminals.Len() > 256 {
			a.terminals.Delete(a.terminals.Keys()[0])
		}
		a.loop.notifyAll()
	}
	return nil
}

// Applies what a replay gap changes in the projection, true when the frame is consumed here
func (a *GatewayACPAgent) gapGuard(ctx context.Context, kind, sid, aid string) (bool, error) {
	if kind == "session.replay_gap" {
		params := map[string]any{"session_id": sid}
		if editor, ok := a.editorMCP[sid]; ok {
			params["editor"] = editor
		}
		a.resumes[sid]++
		link, err := a.currentGateway()
		if err != nil {
			return false, err
		}
		snapshot, err := a.rpc(ctx, link, "session.resume", params)
		if err != nil {
			return false, err
		}
		resumes := a.resumes[sid]
		if err := a.adopt(ctx, sid, snapshot, &resumes); err != nil {
			return false, err
		}
		prompts, _ := dget(snapshot, "prompts").([]any)
		for _, prompt := range prompts {
			if err := a.permission(sid, prompt); err != nil {
				return false, err
			}
		}
		var order []string
		seen := map[string]bool{}
		add := func(admission string) {
			if !seen[admission] {
				seen[admission] = true
				order = append(order, admission)
			}
		}
		pending, _ := dget(snapshot, "pending").([]any)
		for _, raw := range pending {
			if row := asObject(raw); dget(row, "status") == "started" {
				if !dhas(row, "admission_id") {
					return false, errors.New("KeyError: 'admission_id'")
				}
				add(pyStr(dget(row, "admission_id")))
			}
		}
		for _, key := range a.inflight.Keys() {
			if key.session == sid {
				add(key.admission)
			}
		}
		for _, admission := range order {
			if err := a.recover(ctx, sid, admission); err != nil {
				return false, err
			}
		}
		return true, nil
	}
	cut, ok := a.cut[aid]
	if !ok {
		return false, nil
	}
	if kind == "tool.start" || kind == "message.interim" {
		if pystr.Strip(cut) != "" {
			a.segments[aid] = append(a.segments[aid], cut)
		}
		a.cut[aid] = ""
		a.endMessage(sid)
	}
	return kind == "message.delta", nil
}

// Projects usage, tool changes, model pick outcomes, pending rows, title and mode of a session state event
func (a *GatewayACPAgent) projectSessionState(ctx context.Context, kind, sid, editorID string, payload, event *pyjson.Object) error {
	if kind == "session.usage" {
		return a.projectUsage(ctx, sid, editorID, payload)
	}
	if kind == "session.tools_changed" {
		if a.conn != nil {
			return a.say(ctx, editorID, toolsChangedNotice(payload))
		}
		return nil
	}
	if kind == "session.updated" && dget(payload, "operation") == "model" && dget(payload, "status") == "refused" {
		if a.conn != nil {
			plain, _ := pyjson.Plain(payload).(map[string]any)
			return a.say(ctx, editorID, refusedPickText(plain)+"\n")
		}
		return nil
	}
	if _, isText := dget(payload, "confirm").(string); kind == "session.updated" && dget(payload, "operation") == "model" &&
		dget(payload, "status") == "confirmation_required" && isText {
		var task *loopTask
		task = a.loop.createTask(a.ctx, func(ctx context.Context) (any, error) { return nil, a.confirmDeferredPick(ctx, sid, editorID, payload) },
			func(t *loopTask) { delete(a.background, t) })
		a.background[task] = true
		return nil
	}
	if kind == "session.info" && a.snapshots.Has(sid) && dhas(payload, "pending") {
		if a.pendingEventIsNew(sid, event) {
			snapshot, _ := a.snapshots.Get(sid)
			snapshot.Set("pending", dget(payload, "pending"))
			sequence, _ := pyFloat(dgetOr(event, "seq", int64(0)))
			snapshot.Set("_pending_watermark", pendingWatermark{epoch: dget(event, "replay_epoch"), sequence: sequence})
			a.loop.notifyAll()
		}
	}
	if err := a.projectTitle(ctx, sid, editorID, dget(payload, "title")); err != nil {
		return err
	}
	return a.projectMode(ctx, sid, editorID, payload)
}

// Projects another surface's edit policy change as a current mode update, the first one seeding silently
func (a *GatewayACPAgent) projectMode(ctx context.Context, sid, editorID string, payload *pyjson.Object) error {
	policy, _ := dget(payload, "edit_approval_policy").(string)
	if mode, ok := EditApprovalPolicyToMode[policy]; ok {
		shown, seeded := a.modes[sid]
		if !seeded {
			a.modes[sid] = mode
		} else if shown != mode {
			a.modes[sid] = mode
			if a.conn != nil {
				if err := a.sessionUpdate(ctx, editorID, &pyacp.CurrentModeUpdate{CurrentModeID: mode}); err != nil {
					return err
				}
			}
		}
	}
	return a.projectConfig(ctx, sid, editorID, payload)
}

// Resends the full config option list when the mode, model or effort the editor shows changed
func (a *GatewayACPAgent) projectConfig(ctx context.Context, sid, editorID string, payload *pyjson.Object) error {
	fields := []string{"model", "provider", "reasoning_effort"}
	relevant := dhas(payload, "edit_approval_policy")
	for _, key := range fields {
		relevant = relevant || dhas(payload, key)
	}
	if !relevant {
		return nil
	}
	seen := a.configs[sid]
	shown := &shownConfig{fields: pyjson.NewObject()}
	if seen != nil {
		for _, key := range seen.fields.Keys {
			shown.fields.Set(key, seen.fields.Vals[key])
		}
	}
	for _, key := range fields {
		if dhas(payload, key) {
			shown.fields.Set(key, dget(payload, key))
		}
	}
	shown.mode = a.modes[sid]
	if shown.mode == "" {
		shown.mode = gatewayCurrentMode(payload)
	}
	if seen.equal(shown) {
		return nil
	}
	a.configs[sid] = shown
	if seen == nil || a.conn == nil {
		return nil
	}
	var models *pyacp.SessionModelState
	if cancelled := a.loop.awaitCtx(ctx, func() { models = gatewayModelState(ctx, shown.fields) }); cancelled != nil {
		return cancelled
	}
	if a.configs[sid] != shown {
		return nil
	}
	return a.sessionUpdate(ctx, editorID, &pyacp.ConfigOptionUpdate{ConfigOptions: gatewayConfigOptions(models, shown.fields, shown.mode)})
}

// Keeps the owner's usage dict for the turn's response and shows it as the context indicator
func (a *GatewayACPAgent) projectUsage(ctx context.Context, sid, editorID string, stats *pyjson.Object) error {
	a.usage[sid] = stats
	if a.conn != nil {
		if update := gatewayUsageUpdate(stats); update != nil {
			return a.sessionUpdate(ctx, editorID, update)
		}
	}
	return nil
}

// Projects a new title as a session info update
func (a *GatewayACPAgent) projectTitle(ctx context.Context, sid, editorID string, title any) error {
	text, ok := title.(string)
	if !ok || pystr.Strip(text) == "" || text == a.titles[sid] {
		return nil
	}
	a.titles[sid] = text
	if a.conn != nil {
		return a.sessionUpdate(ctx, editorID, gatewayTitleUpdate(text))
	}
	return nil
}

// Raises a permission or clarify card for a pending prompt unless one is already open
func (a *GatewayACPAgent) permission(sessionID string, raw any) error {
	prompt := asObject(raw)
	var answer func(ctx context.Context, sessionID string, prompt *pyjson.Object) bool
	switch dget(prompt, "kind") {
	case "approval":
		answer = a.answerApproval
	case "clarify":
		answer = a.answerQuestion
	}
	if answer == nil || a.conn == nil {
		return nil
	}
	if !dhas(prompt, "prompt_id") || !dhas(prompt, "execution_generation") {
		return errors.New("KeyError: 'prompt_id'")
	}
	key := permissionKey{sessionID, pyrepr.Repr(dget(prompt, "prompt_id")), pyrepr.Repr(dget(prompt, "execution_generation"))}
	if _, open := a.permissions[key]; !open {
		var task *loopTask
		task = a.loop.createTask(a.ctx, func(ctx context.Context) (any, error) {
			if answered := answer(ctx, sessionID, prompt); ctx.Err() != nil {
				return nil, ctx.Err()
			} else {
				return answered, nil
			}
		}, func(t *loopTask) { a.retireUnanswered(key, t) })
		a.permissions[key] = task
	}
	return nil
}

// Gives up the key of a card whose response the owner did not accept
func (a *GatewayACPAgent) retireUnanswered(key permissionKey, task *loopTask) {
	if a.permissions[key] == task && (task.err != nil || task.result != true) {
		delete(a.permissions, key)
	}
}

// Answers a clarify prompt through form elicitation or a permission card, false when nothing was answered
func (a *GatewayACPAgent) answerClarify(ctx context.Context, sessionID string, prompt *pyjson.Object) (answered bool) {
	editorID := a.editorID(sessionID)
	err := func() error {
		var answer string
		var ok bool
		if a.formElicitation {
			request, err := BuildClarifyElicitation(editorID, prompt)
			if err != nil {
				return err
			}
			conn := a.conn
			var response any
			if cancelled := a.loop.awaitCtx(ctx, func() { response, err = conn.SendRequest(ctx, ElicitationMethod, request) }); cancelled != nil {
				return cancelled
			}
			if err != nil {
				return err
			}
			answer, ok = ElicitedAnswer(response)
		} else {
			toolCall, options, answers, err := BuildClarifyPermission(prompt)
			if err != nil {
				return err
			}
			response, err := a.requestPermission(ctx, editorID, toolCall, options)
			if err != nil {
				return err
			}
			if err := a.closeCard(ctx, editorID, toolCall, options, response); err != nil {
				return err
			}
			if allowed, isAllowed := response.Outcome.(*pyacp.AllowedOutcome); isAllowed {
				answer, ok = answers[allowed.OptionID]
			} else {
				answer, ok = answers[SkipOption], true
			}
		}
		if !ok {
			return nil
		}
		link, err := a.currentGateway()
		if err != nil {
			return err
		}
		if _, err := a.rpc(ctx, link, "clarify.respond", map[string]any{"session_id": sessionID, "prompt_id": dget(prompt, "prompt_id"),
			"execution_generation": dget(prompt, "execution_generation"), "answer": answer}); err != nil {
			return err
		}
		answered = true
		return nil
	}()
	if err != nil && !isCancelled(err) {
		gatewayServerLog.Info("ACP clarify viewer detached or control expired\n" + err.Error())
	}
	return answered && err == nil
}

// Answers an approval prompt through a permission card, false when nothing was answered
func (a *GatewayACPAgent) answerPermission(ctx context.Context, sessionID string, prompt *pyjson.Object) (answered bool) {
	choices := map[string]bool{}
	if raw, ok := dget(prompt, "choices").([]any); ok {
		for _, choice := range raw {
			choices[pyStr(choice)] = true
		}
	}
	var options []pyacp.PermissionOption
	for _, option := range buildPermissionOptions(choices["always"], choices["session"], false) {
		if choices[optionIDToHermes[option.OptionID]] {
			options = append(options, option)
		}
	}
	err := func() error {
		var toolCall *pyacp.ToolCallProgress
		if dhas(prompt, "edit") {
			proposal, err := editProposalOf(dget(prompt, "edit"))
			if err != nil {
				return err
			}
			toolCall = BuildACPEditToolCall(proposal)
		} else {
			command, description := "", ""
			if v := dget(prompt, "command"); v != nil {
				command = pyStr(v)
			}
			if v := dget(prompt, "description"); v != nil {
				description = pyStr(v)
			}
			toolCall = buildPermissionToolCall(command, description)
		}
		editorID := a.editorID(sessionID)
		response, err := a.requestPermission(ctx, editorID, toolCall, options)
		if err != nil {
			return err
		}
		if err := a.closeCard(ctx, a.editorID(sessionID), toolCall, options, response); err != nil {
			return err
		}
		allowed, ok := response.Outcome.(*pyacp.AllowedOutcome)
		if !ok {
			return nil
		}
		offered := false
		for _, option := range options {
			offered = offered || option.OptionID == allowed.OptionID
		}
		if !offered {
			return nil
		}
		link, err := a.currentGateway()
		if err != nil {
			return err
		}
		if _, err := a.rpc(ctx, link, "approval.respond", map[string]any{"session_id": sessionID, "prompt_id": dget(prompt, "prompt_id"),
			"execution_generation": dget(prompt, "execution_generation"), "choice": optionIDToHermes[allowed.OptionID]}); err != nil {
			return err
		}
		answered = true
		return nil
	}()
	if err != nil && !isCancelled(err) {
		gatewayServerLog.Info("ACP permission viewer detached or control expired\n" + err.Error())
	}
	return answered && err == nil
}

// Edit proposal of an approval prompt's edit payload
func editProposalOf(raw any) (*tools.EditProposal, error) {
	edit, ok := raw.(*pyjson.Object)
	if !ok {
		return nil, fmt.Errorf("TypeError: EditProposal() argument after ** must be a mapping, not %T", raw)
	}
	proposal := &tools.EditProposal{}
	for _, key := range edit.Keys {
		value := edit.Vals[key]
		switch key {
		case "tool_name":
			proposal.ToolName = pyStr(value)
		case "path":
			proposal.Path = pyStr(value)
		case "old_text":
			if text, ok := value.(string); ok {
				proposal.OldText = &text
			}
		case "new_text":
			proposal.NewText = pyStr(value)
		case "arguments":
			proposal.Arguments, _ = pyjson.Plain(value).(map[string]any)
		case "paths":
			if items, ok := value.([]any); ok {
				for _, item := range items {
					proposal.Paths = append(proposal.Paths, pyStr(item))
				}
			}
		default:
			return nil, fmt.Errorf("TypeError: EditProposal.__init__() got an unexpected keyword argument '%s'", key)
		}
	}
	for _, required := range []string{"tool_name", "path", "old_text", "new_text", "arguments"} {
		if !dhas(edit, required) {
			return nil, fmt.Errorf("TypeError: EditProposal.__init__() missing 1 required positional argument: '%s'", required)
		}
	}
	return proposal, nil
}

// Cancels the background notices and open cards, then drops the owner connection
func (a *GatewayACPAgent) Close() {
	a.loop.enter()
	defer a.loop.leave()
	var background []*loopTask
	for task := range a.background {
		background = append(background, task)
	}
	for _, task := range background {
		task.Cancel()
	}
	for _, task := range background {
		a.loop.join(task)
	}
	var cards []*loopTask
	for _, task := range a.permissions {
		cards = append(cards, task)
	}
	for _, task := range cards {
		task.Cancel()
	}
	for _, task := range cards {
		a.loop.join(task)
	}
	clear(a.permissions)
	a.teardown()
}
