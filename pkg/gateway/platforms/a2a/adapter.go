package a2a

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"math"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/gateway"
	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/internal/pydict"
	"github.com/nickheyer/icebath/pkg/internal/pyhttpx"
	"github.com/nickheyer/icebath/pkg/internal/pyint"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
	"github.com/nickheyer/icebath/pkg/internal/pyurl"
	"github.com/nickheyer/icebath/pkg/profiles"
	"github.com/nickheyer/icebath/pkg/tools"
)

const (
	defaultPort        = 9900
	minOrphanTimeout   = 300.0
	maxOrphanTimeout   = 86400.0
	watchdogInterval   = 60 * time.Second
	maxBody            = 1_048_576
	sseKeepalive       = 5.0
	defaultDescription = "Icebath Agent — a general-purpose agent reachable over A2A."
	maxForwardedInputs = 1000
	threadTimeoutMax   = 9223372036.854776
)

// Adapter handler names of the JSON-RPC surface
const (
	rpcMessageSendName     = "_rpc_message_send"
	rpcMessageStreamName   = "_rpc_message_stream"
	rpcTasksGetName        = "_rpc_tasks_get"
	rpcTasksListName       = "_rpc_tasks_list"
	rpcTasksCancelName     = "_rpc_tasks_cancel"
	rpcTasksSubscribeName  = "_rpc_tasks_subscribe"
	rpcPushConfigCreateNam = "_rpc_push_config_create"
	rpcPushConfigGetName   = "_rpc_push_config_get"
	rpcPushConfigListName  = "_rpc_push_config_list"
	rpcPushConfigDelName   = "_rpc_push_config_delete"
)

// Handler, the v1.0 PascalCase method and the legacy slash aliases still accepted
var methodTable = [][]string{
	{rpcMessageSendName, "SendMessage", "message/send"},
	{rpcMessageStreamName, "SendStreamingMessage", "message/stream"},
	{rpcTasksGetName, "GetTask", "tasks/get"},
	{rpcTasksListName, "ListTasks", "tasks/list"},
	{rpcTasksCancelName, "CancelTask", "tasks/cancel"},
	{rpcTasksSubscribeName, "SubscribeToTask", "tasks/subscribe"},
	{rpcPushConfigCreateNam, "CreateTaskPushNotificationConfig", "tasks/pushNotificationConfig/create",
		"tasks/pushNotificationConfig/set", "tasks/pushNotification/set"},
	{rpcPushConfigGetName, "GetTaskPushNotificationConfig", "tasks/pushNotificationConfig/get"},
	{rpcPushConfigListName, "ListTaskPushNotificationConfigs", "tasks/pushNotificationConfig/list"},
	{rpcPushConfigDelName, "DeleteTaskPushNotificationConfig", "tasks/pushNotificationConfig/delete"},
}

type rpcMethod struct {
	handler string
	v1      bool
}

// JSON-RPC method to its handler name and whether it is the v1.0 spelling
var rpcMethods = func() map[string]rpcMethod {
	out := map[string]rpcMethod{}
	for _, row := range methodTable {
		for i, m := range row[1:] {
			out[m] = rpcMethod{handler: row[0], v1: i == 0}
		}
	}
	return out
}()

// Module functions the hermes cases replace, each holding the real function in production
var (
	redactOutbound         = RedactOutbound
	persistMessage         = PersistMessage
	loggedInput            = LoggedInput
	forwardToOwner         = gateway.ForwardToOwner
	registeredToolsetNames = func(ctx context.Context) []string {
		return tools.Default.RegisteredToolsetNames(ctx, home.Dir(ctx))
	}
	toolNamesForToolset = func(ctx context.Context, toolset string) []string {
		return tools.Default.ToolNamesForToolset(ctx, home.Dir(ctx), toolset)
	}
	profileHome = func(ctx context.Context, profile string) string {
		if dir, err := profiles.GetProfileDir(profile); err == nil {
			return dir
		}
		if profile != "" && profile != "default" {
			return filepath.Join(home.Root(), home.ProfilesDir, profile)
		}
		return home.Dir(ctx)
	}
	loadServedConfig = func(ctx context.Context) (map[string]any, error) {
		tree, err := config.LoadContext(ctx, home.Dir(ctx), nil)
		return map[string]any(tree), err
	}
)

// Seconds to wait for the agent to answer an inbound task, at least one
func replyTimeout() float64 {
	raw, ok := os.LookupEnv("A2A_REPLY_TIMEOUT")
	if !ok {
		raw = "300"
	}
	v, err := pyint.ParseFloat(raw)
	if err != nil {
		return 300
	}
	if v > 1 {
		return v
	}
	return 1
}

// Orphan grace never expiring before the reply window yet bounded by a day
func orphanTimeout() float64 {
	grace := minOrphanTimeout
	if rt := replyTimeout(); rt > grace {
		grace = rt
	}
	return min(maxOrphanTimeout, grace)
}

// Scoped A2A_AGENT_NAME, else icebath plus the hostname
func defaultAgentName(ctx context.Context) (string, error) {
	name, err := gateway.GetScopedSecret(ctx, "A2A_AGENT_NAME", "", false)
	if err != nil {
		return "", err
	}
	if name = pystr.Strip(name); name != "" {
		return name, nil
	}
	host, err := os.Hostname()
	if err != nil {
		return "icebath-agent", nil
	}
	return "icebath-" + host, nil
}

// Single segment slug of a served agent, empty for the root agent
func cleanSlug(value string) string {
	slug := strings.Trim(pystr.Strip(value), "/")
	if slug == "" || slug == "default" || slug == "root" {
		return ""
	}
	first, _, _ := strings.Cut(slug, "/")
	return first
}

// Base URL with a trailing slash joined with the served agent prefix
func joinURL(base, prefix string) string {
	base = pystr.Strip(base)
	if base == "" {
		base = "/"
	}
	if !strings.HasSuffix(base, "/") {
		base += "/"
	}
	prefix = strings.Trim(prefix, "/")
	if prefix == "" {
		return base
	}
	joined, err := pyurl.Join(base, prefix+"/")
	if err != nil {
		panic(&PyError{"ValueError", err.Error()})
	}
	return joined
}

// Profile name of the current home, default when none resolves
func activeProfileName(ctx context.Context) string {
	if name := profiles.GetActiveProfileName(ctx); name != "" {
		return name
	}
	return "default"
}

// Listen network of a bind host, an IPv6 literal listening dual stack and anything else IPv4 only
func httpServerNetwork(host string) string {
	if strings.Contains(host, ":") {
		return "tcp"
	}
	return "tcp4"
}

// One routable agent of this server, the root agent having an empty slug and no timeout
type servedAgent struct {
	Slug               string
	Path               string
	Tenant             string
	Profile            string
	Local              bool
	Name               string
	Description        string
	AdvertisedToolsets []any
	Timeout            int
}

// Routed agent and the path below its prefix, or the tenant mismatch error
type route struct {
	agent   *servedAgent
	subpath string
	err     string
}

// Dispatched task the HTTP worker waits on for the gateway reply
type pendingTask struct {
	taskID    string
	contextID string
	peer      string
	future    *TaskWatch
	started   float64
}

type pendingEntry struct {
	contextID string
	future    *TaskWatch
}

// Inbound A2A server adapter routing peer tasks into the live gateway session
type A2AAdapter struct {
	*gateway.BasePlatformAdapter
	securityContext    *A2ASecurityContext
	port               int
	host               string
	agentName          string
	advertisedToolsets []any
	activeProfile      string
	publicURL          string
	agents             *pydict.Dict[string, *servedAgent]
	tasks              *TaskStore
	turns              *TurnTracker
	rateLimiter        *RateLimiter
	prepareTask        func(params *pyjson.Object, peer string, agent *servedAgent) (*pyjson.Object, *pendingTask)
	forwardToProfile   func(agent *servedAgent, peer, contextID, framedText, inputID string) (string, string)

	mu           sync.Mutex
	listener     net.Listener
	loopCtx      context.Context
	loop         gateway.EventLoop
	watchdogStop chan struct{}

	pendingMu       sync.Mutex
	pending         map[string]pendingEntry
	pendingOrder    map[string][]string
	activeTasks     map[string]struct{}
	forwardedInputs *pydict.Dict[string, struct{}]
}

// Adapter with the profile scoped security, port, name, toolsets and served agents captured now
func NewA2AAdapter(ctx context.Context, cfg *gateway.PlatformConfig) (adapter *A2AAdapter, err error) {
	defer func() {
		if rec := recover(); rec != nil {
			perr, ok := rec.(error)
			if !ok {
				panic(rec)
			}
			adapter, err = nil, perr
		}
	}()
	a := &A2AAdapter{tasks: NewTaskStore(), turns: NewTurnTracker(), rateLimiter: NewRateLimiter(),
		pending: map[string]pendingEntry{}, pendingOrder: map[string][]string{}, activeTasks: map[string]struct{}{},
		forwardedInputs: pydict.New[string, struct{}]()}
	a.BasePlatformAdapter = gateway.NewBasePlatformAdapter(cfg, gateway.Platform("a2a"), a)
	a.prepareTask, a.forwardToProfile = a.prepareTaskImpl, a.forwardToProfileImpl
	extra := map[string]any{}
	if cfg != nil && cfg.Extra != nil {
		extra = cfg.Extra
	}
	a.securityContext = CaptureSecurityContext(ctx)
	portEnv, err := gateway.GetScopedSecret(ctx, "A2A_PORT", "", false)
	if err != nil {
		return nil, err
	}
	var rawPort any = portEnv
	if portEnv == "" {
		rawPort = defaultPort
		if v, ok := extra["port"]; ok {
			rawPort = v
		}
	}
	port, err := pyint.Of(rawPort)
	if err != nil {
		return nil, err
	}
	switch {
	case port.IsInt64():
		a.port = int(port.Int64())
	case port.Sign() < 0:
		a.port = math.MinInt
	default:
		a.port = math.MaxInt
	}
	a.host = a.securityContext.ResolveBindHost()
	if a.agentName, err = defaultAgentName(ctx); err != nil {
		return nil, err
	}
	configured := pyIter(pyOr(extra["advertised_toolsets"], []any{}))
	if len(configured) == 0 {
		raw, err := gateway.GetScopedSecret(ctx, "A2A_ADVERTISED_TOOLSETS", "", false)
		if err != nil {
			return nil, err
		}
		for _, t := range strings.Split(raw, ",") {
			configured = append(configured, t)
		}
	}
	a.advertisedToolsets = []any{}
	for _, t := range configured {
		if pystr.Strip(pyrepr.Str(t)) == "" {
			continue
		}
		s, ok := t.(string)
		if !ok {
			panic(attributeError(t, "strip"))
		}
		a.advertisedToolsets = append(a.advertisedToolsets, pystr.Strip(s))
	}
	a.activeProfile = activeProfileName(ctx)
	publicURL, err := gateway.GetScopedSecret(ctx, "A2A_PUBLIC_URL", "", false)
	if err != nil {
		return nil, err
	}
	a.publicURL = pystr.Strip(publicURL)
	if a.agents, err = a.loadServedAgents(ctx, extra); err != nil {
		return nil, err
	}
	return a, nil
}

func (a *A2AAdapter) Name() string { return "A2A" }

// Requests are authenticated per call so the gateway allow list never rejects a peer identity
func (a *A2AAdapter) AuthorizationIsUpstream() bool { return true }

// Binds the listener, starts the server and the orphan watchdog and marks the adapter connected
func (a *A2AAdapter) Connect(ctx context.Context, isReconnect bool) (bool, error) {
	var loop gateway.EventLoop
	if runner, ok := gateway.CurrentGatewayRunner().(gateway.GatewayLoopRunner); ok && runner.OnGatewayLoop(ctx) {
		loop = runner.GatewayLoop()
	}
	a.mu.Lock()
	a.loopCtx, a.loop = context.WithoutCancel(ctx), loop
	a.mu.Unlock()
	if a.port < 0 || a.port > 65535 {
		return false, &PyError{"OverflowError", "bind(): port must be 0-65535."}
	}
	ln, err := net.Listen(httpServerNetwork(a.host), net.JoinHostPort(a.host, strconv.Itoa(a.port)))
	if err != nil {
		text := pyhttpx.OSErrorText(err)
		slog.Error(fmt.Sprintf("A2A: could not bind %s:%d — %s", a.host, a.port, text))
		a.SetFatalError(ctx, "bind_failed", "A2A bind failed: "+text, true)
		return false, nil
	}
	srv := &http.Server{Handler: requestHandler{a}, ErrorLog: slog.NewLogLogger(slog.Default().Handler(), slog.LevelDebug)}
	srv.SetKeepAlivesEnabled(false)
	stop := make(chan struct{})
	a.mu.Lock()
	a.listener, a.watchdogStop = ln, stop
	a.mu.Unlock()
	go func() { _ = srv.Serve(ln) }()
	go a.watchdogLoop(stop)
	a.MarkConnected(ctx, "")
	sec := a.securityContext
	mode := "REMOTE (bearer auth)"
	if sec.LocalhostOnly() {
		mode = "localhost-only"
	}
	slog.Info(fmt.Sprintf("A2A: serving Agent Card + JSON-RPC on http://%s:%d (%s) as %s; %d routed agent(s)", a.host, a.port, mode,
		pyrepr.StrRepr(a.agentName), a.agents.Len()))
	if sec.DispatchFailsClosed() {
		slog.Error(fmt.Sprintf("A2A: exposed on non-loopback bind %s with no A2A_TRUSTED_PEERS; every dispatch will be refused (403). "+
			"Set A2A_TRUSTED_PEERS, or A2A_ALLOW_ALL_USERS=true for a trusted network.", a.host))
	}
	a.WirePluginHandlers(ctx, nil)
	return true, nil
}

// Stops the listener and the watchdog and fails every in flight reply so blocked workers return
func (a *A2AAdapter) Disconnect(ctx context.Context) error {
	a.MarkDisconnected(ctx)
	a.mu.Lock()
	stop, ln := a.watchdogStop, a.listener
	a.watchdogStop, a.listener = nil, nil
	a.mu.Unlock()
	if stop != nil {
		close(stop)
	}
	if ln != nil {
		_ = ln.Close()
	}
	a.pendingMu.Lock()
	defer a.pendingMu.Unlock()
	for tid := range a.pending {
		a.resolveLocked(tid, StateFailed, "[agent shutting down]")
	}
	clear(a.pending)
	clear(a.pendingOrder)
	clear(a.activeTasks)
	return nil
}

// Fails orphaned tasks every watchdog interval until stopped, keeping them queryable
func (a *A2AAdapter) watchdogLoop(stop <-chan struct{}) {
	for {
		timer := time.NewTimer(watchdogInterval)
		select {
		case <-stop:
			timer.Stop()
			return
		case <-timer.C:
		}
		func() {
			defer func() {
				if rec := recover(); rec != nil {
					slog.Debug("A2A: watchdog error", "exc_info", rec)
				}
			}()
			a.failOrphansOnce()
		}()
	}
}

// Fails stale tasks that no request still owns and returns their ids
func (a *A2AAdapter) failOrphansOnce() []string {
	a.pendingMu.Lock()
	active := maps.Clone(a.activeTasks)
	a.pendingMu.Unlock()
	timeout := orphanTimeout()
	failed := a.tasks.FailOrphans(timeout, active)
	for _, tid := range failed {
		slog.Warn(fmt.Sprintf("A2A: orphaned task %s marked failed (timeout %gs)", tid, timeout))
		SharedMetrics.TasksFailed.Add(1)
	}
	return failed
}

// Served agents from platforms.a2a.extra.agents or the config fallback, the root always the live session
func (a *A2AAdapter) loadServedAgents(ctx context.Context, extra map[string]any) (*pydict.Dict[string, *servedAgent], error) {
	raw := pyOr(extra["agents"], extra["served_agents"])
	if raw == nil {
		cfg, err := loadServedConfig(ctx)
		if err != nil || cfg == nil {
			cfg = map[string]any{}
		}
		if v := cfg["a2a_served_agents"]; pyrepr.Truthy(v) {
			raw = v
		} else {
			raw = pyGet(pyOr(cfg["a2a"], map[string]any{}), "served_agents", nil)
		}
	}
	defaultDesc, err := gateway.GetScopedSecret(ctx, "A2A_AGENT_DESCRIPTION", defaultDescription, false)
	if err != nil {
		return nil, err
	}
	agents := pydict.New[string, *servedAgent]()
	agents.Set("", &servedAgent{Profile: a.activeProfile, Local: true, Name: a.agentName, Description: defaultDesc,
		AdvertisedToolsets: a.advertisedToolsets})
	reserved := map[string]bool{"health": true, "metrics": true, ".well-known": true}
	tenants := map[string]string{}
	type item struct {
		key any
		val any
	}
	var items []item
	switch {
	case isMapping(raw):
		for _, k := range mappingKeys(raw) {
			items = append(items, item{k, pyGet(raw, k, nil)})
		}
	default:
		if list, ok := raw.([]any); ok {
			for i, v := range list {
				items = append(items, item{i, v})
			}
		}
	}
	for _, it := range items {
		val := it.val
		if !isMapping(val) {
			continue
		}
		slug := cleanSlug(pyrepr.Str(pyOr(pyOr(pyGet(val, "slug", nil), pyGet(val, "id", nil)), it.key)))
		if slug == "" {
			continue
		}
		pathSegment := cleanSlug(pyrepr.Str(pyOr(pyGet(val, "path", nil), slug)))
		if pathSegment == "" || reserved[pathSegment] {
			slog.Warn(fmt.Sprintf("A2A: ignoring served agent %s with reserved/invalid path %s", pyrepr.StrRepr(slug), pyrepr.StrRepr(pathSegment)))
			continue
		}
		profile := pystr.Strip(pyrepr.Str(pyOr(pyGet(val, "profile", nil), slug)))
		toolsets := pyOr(pyOr(pyOr(pyGet(val, "advertised_toolsets", nil), pyGet(val, "toolsets", nil)), pyGet(val, "capabilities", nil)), []any{})
		if s, ok := toolsets.(string); ok {
			split := []any{}
			for _, t := range strings.Split(s, ",") {
				if t = pystr.Strip(t); t != "" {
					split = append(split, t)
				}
			}
			toolsets = split
		}
		tenant := pystr.Strip(pyrepr.Str(pyOr(pyGet(val, "tenant", nil), slug)))
		if tenant != "" {
			if owner, dup := tenants[tenant]; dup {
				slog.Warn(fmt.Sprintf("A2A: ignoring served agent %s with duplicate tenant %s already used by %s",
					pyrepr.StrRepr(slug), pyrepr.StrRepr(tenant), pyrepr.StrRepr(owner)))
				continue
			}
			tenants[tenant] = slug
		}
		ownProfile := profile
		if ownProfile == "" {
			ownProfile = slug
		}
		local := pyrepr.Truthy(pyGet(val, "local", nil)) || profile == "" || profile == "default" || profile == a.activeProfile
		timeoutValue := pyGet(val, "timeout", nil)
		if !pyrepr.Truthy(timeoutValue) {
			timeoutValue = replyTimeout()
		}
		timeout, err := pyint.Of(timeoutValue)
		if err != nil {
			return nil, err
		}
		agents.Set(slug, &servedAgent{Slug: slug, Path: "/" + pathSegment, Tenant: tenant, Profile: ownProfile, Local: local,
			Name:               pyrepr.Str(pyOr(pyGet(val, "name", nil), "Icebath "+slug)),
			Description:        pyrepr.Str(pyOr(pyGet(val, "description", nil), fmt.Sprintf("Icebath profile '%s' exposed over A2A.", ownProfile))),
			AdvertisedToolsets: slices.Clone(pyIter(pyOr(toolsets, []any{}))),
			Timeout:            int(timeout.Int64())})
	}
	return agents, nil
}

func (a *A2AAdapter) rootAgent() *servedAgent {
	root, _ := a.agents.Get("")
	return root
}

// Public URL when given, else the bind host and port with an IPv6 literal bracketed
func (a *A2AAdapter) baseURL(publicURL string) string {
	if s := pystr.Strip(publicURL); s != "" {
		return s
	}
	host := a.host
	if strings.Contains(host, ":") {
		host = "[" + host + "]"
	}
	return fmt.Sprintf("http://%s:%d/", host, a.port)
}

// Slug, name, url, tenant, profile and locality of every served agent
func (a *A2AAdapter) servedAgentSummary(publicURL string) []any {
	base := a.baseURL(publicURL)
	out := []any{}
	for _, ag := range a.agents.Values() {
		slug := ag.Slug
		if slug == "" {
			slug = "default"
		}
		var tenant any
		if ag.Tenant != "" {
			tenant = ag.Tenant
		}
		out = append(out, obj("slug", slug, "name", ag.Name, "url", joinURL(base, ag.Path), "tenant", tenant,
			"profile", ag.Profile, "local", ag.Local))
	}
	return out
}

// Longest matching path prefix agent with the remaining subpath, the root agent as fallback
func (a *A2AAdapter) routeForPath(rawPath string) route {
	if rawPath == "" {
		rawPath = "/"
	}
	parts, err := pyurl.Split(rawPath)
	if err != nil {
		panic(&PyError{"ValueError", err.Error()})
	}
	path := parts.Path
	if path == "" {
		path = "/"
	}
	agents := a.agents.Values()
	slices.SortStableFunc(agents, func(x, y *servedAgent) int {
		return utf8.RuneCountInString(y.Path) - utf8.RuneCountInString(x.Path)
	})
	for _, ag := range agents {
		if prefix := ag.Path; prefix != "" && (path == prefix || strings.HasPrefix(path, prefix+"/")) {
			sub := path[len(prefix):]
			if sub == "" {
				sub = "/"
			}
			return route{agent: ag, subpath: sub}
		}
	}
	return route{agent: a.rootAgent(), subpath: path}
}

// Path route refined by the v1.0 tenant param, the mismatch error when the tenant names another agent
func (a *A2AAdapter) routeForRequest(rawPath string, params *pyjson.Object) route {
	rt := a.routeForPath(rawPath)
	agent := rt.agent
	tenant := pyrepr.Str(pyOr(getOr(params, "tenant"), ""))
	if agent.Slug == "" && tenant != "" {
		for _, ag := range a.agents.Values() {
			if ag.Tenant == tenant {
				agent = ag
				rt = route{agent: agent, subpath: rt.subpath}
				break
			}
		}
	}
	if expected := agent.Tenant; tenant != "" && expected != "" && tenant != expected {
		slug := agent.Slug
		if slug == "" {
			slug = "default"
		}
		return route{err: fmt.Sprintf("tenant %s does not match routed agent %s", pyrepr.StrRepr(tenant), slug)}
	}
	return rt
}

// Agent Card of a served agent whose URL follows the request public URL over the bind host
func (a *A2AAdapter) buildCard(ctx context.Context, publicURL string, agent *servedAgent) *pyjson.Object {
	if agent == nil {
		agent = a.rootAgent()
	}
	name, description := agent.Name, agent.Description
	if name == "" {
		name = a.agentName
	}
	if description == "" {
		description = defaultDescription
	}
	return BuildAgentCard(AgentCardOptions{Name: name, URL: joinURL(a.baseURL(publicURL), agent.Path), Description: description,
		Skills: a.advertisedSkills(ctx, agent), Streaming: agent.Local, PushNotifications: true,
		AuthRequired: !a.securityContext.LocalhostOnly(), Tenant: agent.Tenant})
}

// Card skills from the live tool registry limited to the advertised toolsets, the configured names otherwise
func (a *A2AAdapter) advertisedSkills(ctx context.Context, agent *servedAgent) []any {
	configured := a.advertisedToolsets
	if agent != nil {
		configured = agent.AdvertisedToolsets
	}
	mapping := map[string][]string{}
	for _, n := range registeredToolsetNames(ctx) {
		if len(configured) == 0 || pyIn(n, configured) {
			mapping[n] = toolNamesForToolset(ctx, n)
		}
	}
	if len(mapping) > 0 {
		return SkillsFromToolsets(mapping)
	}
	return SkillsFromToolsets(slices.Clone(configured))
}

// Registers a reply waiter for the task in its context FIFO and marks the task owned
func (a *A2AAdapter) addPending(taskID, contextID string) *TaskWatch {
	fut := newTaskWatch()
	a.pendingMu.Lock()
	defer a.pendingMu.Unlock()
	a.activeTasks[taskID] = struct{}{}
	a.pending[taskID] = pendingEntry{contextID: contextID, future: fut}
	a.pendingOrder[contextID] = append(a.pendingOrder[contextID], taskID)
	return fut
}

func (a *A2AAdapter) activateTask(taskID string) {
	a.pendingMu.Lock()
	a.activeTasks[taskID] = struct{}{}
	a.pendingMu.Unlock()
}

// Records a forwarded admission id, true when this process already sent it as a retry
func (a *A2AAdapter) noteForwardedInput(inputID string) bool {
	a.pendingMu.Lock()
	defer a.pendingMu.Unlock()
	seen := a.forwardedInputs.Has(inputID)
	a.forwardedInputs.Delete(inputID)
	a.forwardedInputs.Set(inputID, struct{}{})
	for a.forwardedInputs.Len() > maxForwardedInputs {
		a.forwardedInputs.Delete(a.forwardedInputs.Keys()[0])
	}
	return seen
}

// Drops a fresh id a pre admission guard refused so its resend is refused again
func (a *A2AAdapter) forgetForwardedInput(inputID string) {
	a.pendingMu.Lock()
	a.forwardedInputs.Delete(inputID)
	a.pendingMu.Unlock()
}

// Releases the task ownership and its reply waiter from the context FIFO
func (a *A2AAdapter) popPending(taskID string) {
	a.pendingMu.Lock()
	defer a.pendingMu.Unlock()
	delete(a.activeTasks, taskID)
	entry, ok := a.pending[taskID]
	if !ok {
		return
	}
	delete(a.pending, taskID)
	order, present := a.pendingOrder[entry.contextID]
	if i := slices.Index(order, taskID); i >= 0 {
		order = slices.Delete(order, i, i+1)
		a.pendingOrder[entry.contextID] = order
	}
	if present && len(order) == 0 {
		delete(a.pendingOrder, entry.contextID)
	}
}

func (a *A2AAdapter) resolveLocked(taskID, state, text string) bool {
	entry, ok := a.pending[taskID]
	if !ok || entry.future.Done() {
		return false
	}
	entry.future.set(state, text)
	return true
}

func (a *A2AAdapter) resolveTask(taskID, state, text string) bool {
	a.pendingMu.Lock()
	defer a.pendingMu.Unlock()
	return a.resolveLocked(taskID, state, text)
}

// Resolves the oldest unresolved waiter of a context, false when none is waiting
func (a *A2AAdapter) resolveOldestForContext(contextID, state, text string) bool {
	a.pendingMu.Lock()
	defer a.pendingMu.Unlock()
	for _, tid := range a.pendingOrder[contextID] {
		if a.resolveLocked(tid, state, text) {
			return true
		}
	}
	return false
}

// Slug and tenant scope of an agent, the root agent when nil
func (a *A2AAdapter) scopeForAgent(agent *servedAgent) (string, string) {
	if agent == nil {
		agent = a.rootAgent()
	}
	return agent.Slug, agent.Tenant
}

// Completes a task at once, counting a failure, and builds its terminal Task
func (a *A2AAdapter) endTask(rec TaskRecord, state, text, storedReply string) (*pyjson.Object, *pendingTask) {
	a.tasks.Complete(rec.TaskID, state, storedReply)
	if state == StateFailed {
		SharedMetrics.TasksFailed.Add(1)
	}
	return BuildTask(rec.TaskID, rec.ContextID, state, text), nil
}

// Context of an HTTP worker, empty like a fresh Python server thread so the home is the process home
func workerContext() context.Context { return context.Background() }

// Validates, registers and dispatches an inbound message, a terminal task or the pending waiter
func (a *A2AAdapter) prepareTaskImpl(params *pyjson.Object, peer string, agent *servedAgent) (*pyjson.Object, *pendingTask) {
	ctx := workerContext()
	if agent == nil {
		agent = a.rootAgent()
	}
	text := ExtractText(params)
	forwarded := !agent.Local
	messageID := ""
	if forwarded {
		messageID = ExtractMessageID(params)
	}
	slug, tenant := a.scopeForAgent(agent)
	contextID := ExtractContextID(params)
	if contextID == "" {
		if messageID != "" {
			contextID = MessageContextID(strings.Join([]string{slug, tenant, peer}, "\x00"), messageID)
		} else {
			contextID = NewContextID()
		}
	}
	inputID := ""
	if messageID != "" {
		sum := sha256.Sum256([]byte(contextID + "\x00" + messageID))
		inputID = "a2a-msg:" + hex.EncodeToString(sum[:])
	}
	retry := inputID != "" && (a.noteForwardedInput(inputID) || loggedInput(ctx, contextID, inputID))
	taskID := NewTaskID()
	turn := 0
	if !retry {
		turn = a.turns.Track(contextID)
	}
	maxTurns := MaxPingpongTurns()
	recInput := ""
	if forwarded {
		recInput = inputID
		if recInput == "" {
			recInput = taskID
		}
	}
	rec := a.tasks.Create(taskID, contextID, peer, slug, tenant, recInput)
	if !retry && (turn > maxTurns || text == "") {
		a.forgetForwardedInput(inputID)
	}
	if turn > maxTurns {
		SharedMetrics.AntiLoopTriggers.Add(1)
		slog.Warn(fmt.Sprintf("A2A: anti-loop triggered for context %s (turn %d > %d)", contextID, turn, maxTurns))
		return a.endTask(rec, StateRejected, fmt.Sprintf("Anti-loop protection: context %s exceeded "+
			"%d turns. Start a new context or increase A2A_MAX_PINGPONG_TURNS.", contextID, maxTurns), "")
	}
	if text == "" {
		return a.endTask(rec, StateRejected, "Empty task — nothing to do.", "")
	}
	framed := WrapInbound(peer, text)
	Audit(ctx, "inbound", peer, taskID, text)
	if !retry {
		persistMessage(ctx, contextID, "user", text, taskID, inputID)
		SharedMetrics.InboundTotal.Add(1)
	}
	a.registerInlinePush(taskID, params, agent)
	if forwarded {
		a.activateTask(taskID)
		defer a.popPending(taskID)
		forwardInput := inputID
		if forwardInput == "" {
			forwardInput = taskID
		}
		reply, state := a.forwardToProfile(agent, peer, contextID, framed, forwardInput)
		a.recordOutcome(taskID, contextID, peer, state, reply, nil)
		return BuildTask(taskID, contextID, state, reply), nil
	}
	a.mu.Lock()
	loopCtx, loop := a.loopCtx, a.loop
	a.mu.Unlock()
	if loopCtx == nil || a.MessageHandler() == nil {
		return a.endTask(rec, StateFailed, "Agent gateway not ready to accept A2A tasks.", "")
	}
	fut := a.addPending(taskID, contextID)
	event := gateway.NewMessageEvent(framed)
	event.MessageID = &taskID
	chatName := "a2a:" + peer
	userID, userName := peer, peer
	event.Source = a.BuildSource(loopCtx, contextID, gateway.SourceFields{ChatName: &chatName, ChatType: "dm", UserID: &userID, UserName: &userName})
	if err := dispatchOnLoop(loopCtx, loop, func(c context.Context) { _ = a.HandleMessage(c, event) }); err != nil {
		msg := redactOutbound(fmt.Sprintf("Dispatch failed: %s", pyrepr.ExceptionMessage(err)))
		defer a.popPending(taskID)
		return a.endTask(rec, StateFailed, msg, msg)
	}
	a.tasks.SetState(taskID, StateWorking)
	return nil, &pendingTask{taskID: taskID, contextID: contextID, peer: peer, future: fut, started: nowSeconds()}
}

// Schedules the handler onto the gateway loop captured at connect, a goroutine when connect ran off that loop
func dispatchOnLoop(ctx context.Context, loop gateway.EventLoop, run func(context.Context)) error {
	if loop == nil {
		go run(ctx)
		return nil
	}
	return loop.CallSoonThreadsafe(func() { go run(ctx) })
}

// Forwards to the destination profile owner and returns the redacted reply with its terminal state
func (a *A2AAdapter) forwardToProfileImpl(agent *servedAgent, peer, contextID, framedText, inputID string) (string, string) {
	ctx := workerContext()
	profile := agent.Profile
	if profile == "" {
		profile = agent.Slug
	}
	profile = pystr.Strip(profile)
	ownerHome := profileHome(ctx, profile)
	if ownerHome == "" {
		return "[profile unavailable]", StateFailed
	}
	receipt, err := func() (map[string]any, error) {
		name := agent.Slug
		if name == "" {
			name = profile
		}
		if name == "" {
			name = "agent"
		}
		if inputID == "" {
			inputID = NewTaskID()
		}
		timeout := agent.Timeout
		if timeout == 0 {
			n, err := pyint.Of(replyTimeout())
			if err != nil {
				return nil, err
			}
			timeout = int(n.Int64())
		}
		return forwardToOwner(ctx, ownerHome, gateway.A2AForward{Agent: name, Tenant: agent.Tenant, Peer: peer, ContextID: contextID,
			InputID: inputID, Text: framedText}, time.Duration(timeout)*time.Second)
	}()
	var netErr net.Error
	if errors.Is(err, context.DeadlineExceeded) || (errors.As(err, &netErr) && netErr.Timeout()) {
		return "[profile reply unverified; accepted work was not cancelled]", StateFailed
	}
	if err != nil {
		slog.Warn(fmt.Sprintf("A2A: dispatch to profile %s unverified", pyrepr.StrRepr(profile)), "exc_info", err)
		return redactOutbound(fmt.Sprintf("Profile dispatch unverified: %s", pyrepr.ExceptionMessage(err))), StateFailed
	}
	result := pyOr(receipt["result"], map[string]any{})
	completed := pyrepr.Equal(receipt["outcome"], "completed") && !pyrepr.Truthy(pyGet(result, "failed", nil))
	state := StateFailed
	if completed {
		state = StateCompleted
	}
	return redactOutbound(pyrepr.Str(pyOr(pyGet(result, "final_response", nil), ""))), state
}

// Persists, audits and counts a finished task, marks it terminal and fires its push callback
func (a *A2AAdapter) recordOutcome(taskID, contextID, peer, state, reply string, started *float64) {
	ctx := workerContext()
	persistMessage(ctx, contextID, "agent", reply, taskID, "")
	Audit(ctx, "outbound", peer, taskID, reply)
	if state == StateCompleted || state == StateInputRequired {
		SharedMetrics.OutboundTotal.Add(1)
		SharedMetrics.TasksCompleted.Add(1)
		if started != nil {
			SharedMetrics.RecordLatency(nowSeconds() - *started)
		}
	} else {
		SharedMetrics.TasksFailed.Add(1)
	}
	a.tasks.Complete(taskID, state, reply)
	a.sendPushNotification(ctx, taskID, contextID, reply, state)
}

// Records a dispatched task outcome after redaction, a leading marker turning it into input required
func (a *A2AAdapter) finalizeTask(pending *pendingTask, state, reply string) (string, string) {
	defer a.popPending(pending.taskID)
	reply = redactOutbound(reply)
	stripped := pystr.Lstrip(reply)
	if state == StateCompleted && strings.HasPrefix(pystr.Upper(stripped), InputRequiredMarker) {
		state = StateInputRequired
		reply = pystr.Strip(pystr.Slice(stripped, len([]rune(InputRequiredMarker)), len([]rune(stripped))))
	}
	started := pending.started
	a.recordOutcome(pending.taskID, pending.contextID, pending.peer, state, reply, &started)
	return state, reply
}

// Waits for the future or the deadline, running the keepalive every few seconds and stopping when it fails
func awaitFuture(fut *TaskWatch, deadline float64, keepalive func() error, onTimeoutState, onTimeoutReply string) (string, string) {
	for {
		wait := sseKeepalive
		if keepalive == nil {
			wait = max(0, deadline-nowSeconds())
		}
		if wait >= threadTimeoutMax {
			return onTimeoutState, onTimeoutReply
		}
		timer := time.NewTimer(time.Duration(wait * float64(time.Second)))
		select {
		case <-fut.C():
			timer.Stop()
			return fut.state, fut.reply
		case <-timer.C:
		}
		if nowSeconds() >= deadline {
			return onTimeoutState, onTimeoutReply
		}
		if keepalive != nil {
			if err := keepalive(); err != nil {
				return StateFailed, "[client disconnected]"
			}
		}
	}
}

func (a *A2AAdapter) awaitReply(pending *pendingTask, keepalive func() error) (string, string) {
	return awaitFuture(pending.future, pending.started+replyTimeout(), keepalive, StateFailed, "[agent did not reply in time]")
}

// SendMessage answered once the agent replied, the v1.0 response wrapping the task
func (a *A2AAdapter) rpcMessageSend(reqID any, params *pyjson.Object, peer string, agent *servedAgent, v1Response bool) *pyjson.Object {
	task, pending := a.prepareTask(params, peer, agent)
	if task == nil {
		state, reply := a.awaitReply(pending, nil)
		state, reply = a.finalizeTask(pending, state, reply)
		task = BuildTask(pending.taskID, pending.contextID, state, reply)
	}
	if v1Response {
		return JsonrpcResult(reqID, SendMessageResponse(task))
	}
	return JsonrpcResult(reqID, task)
}

// Final artifact and status events then the closure marker, the request id in each envelope
func (a *A2AAdapter) emitTerminal(q *a2aRequest, taskID, contextID, state, reply string, reqID any) error {
	completed := reply != "" && state == StateCompleted
	var events []any
	statusText := reply
	if completed {
		events = append(events, ArtifactUpdate(taskID, contextID, reply))
		statusText = ""
	}
	events = append(events, StatusUpdate(taskID, contextID, state, statusText))
	for _, ev := range events {
		if err := q.sseWrite(mustSSE(ev, reqID)); err != nil {
			return err
		}
	}
	return q.sseWrite(SSEDone())
}

// SSE data frame of an event, a dump failure raised like the Python json error
func mustSSE(payload, reqID any) string {
	frame, err := SSEData(payload, reqID)
	if err != nil {
		panic(err)
	}
	return frame
}

// SendStreamingMessage as an SSE stream of JSON-RPC wrapped StreamResponse events
func (a *A2AAdapter) rpcMessageStream(q *a2aRequest, reqID any, params *pyjson.Object, peer string, agent *servedAgent) {
	SharedMetrics.StreamsStarted.Add(1)
	q.sseHeaders()
	var pending *pendingTask
	err := func() error {
		terminal, p := a.prepareTask(params, peer, agent)
		pending = p
		if terminal != nil {
			status := pyGet(terminal, "status", pyjson.NewObject())
			message, ok := pyOr(pyGet(status, "message", pyjson.NewObject()), pyjson.NewObject()).(*pyjson.Object)
			if !ok {
				message = pyjson.NewObject()
			}
			return a.emitTerminal(q, pyrepr.Str(getOr(terminal, "id")), pyrepr.Str(getOr(terminal, "contextId")),
				pyrepr.Str(getOr(status, "state")), ExtractText(message), reqID)
		}
		submitted := BuildTask(pending.taskID, pending.contextID, StateSubmitted, "")
		if err := q.sseWrite(mustSSE(StreamTask(submitted), reqID)); err != nil {
			return err
		}
		if err := q.sseWrite(mustSSE(StatusUpdate(pending.taskID, pending.contextID, StateWorking, ""), reqID)); err != nil {
			return err
		}
		state, reply := a.awaitReply(pending, q.keepalive)
		state, reply = a.finalizeTask(pending, state, reply)
		taskID, contextID := pending.taskID, pending.contextID
		pending = nil
		return a.emitTerminal(q, taskID, contextID, state, reply, reqID)
	}()
	if err != nil {
		if pending != nil {
			a.finalizeTask(pending, StateFailed, "[client disconnected]")
		}
		slog.Debug("A2A: stream client disconnected")
	}
}

// SubscribeToTask reattaching to a task stream until it reaches its terminal state
func (a *A2AAdapter) rpcTasksSubscribe(q *a2aRequest, reqID any, params *pyjson.Object, agent *servedAgent) {
	taskID, rec, errObj := a.findTask(reqID, params, agent)
	if errObj != nil {
		q.json(200, errObj)
		return
	}
	q.sseHeaders()
	err := func() error {
		slug, tenant := a.scopeForAgent(agent)
		fut := a.tasks.Watch(taskID, slug, tenant)
		if fut == nil {
			return q.sseWrite(SSEDone())
		}
		state, reply := awaitFuture(fut, nowSeconds()+replyTimeout(), q.keepalive, rec.State, rec.Reply)
		return a.emitTerminal(q, taskID, rec.ContextID, state, reply, reqID)
	}()
	if err != nil {
		slog.Debug("A2A: subscribe client disconnected")
	}
}

// Visible task record of the request, else the task not found error
func (a *A2AAdapter) findTask(reqID any, params *pyjson.Object, agent *servedAgent) (string, TaskRecord, *pyjson.Object) {
	taskID := pyrepr.Str(pyOr(pyOr(getOr(params, "taskId"), getOr(params, "id")), ""))
	slug, tenant := a.scopeForAgent(agent)
	rec, ok := a.tasks.Get(taskID, slug, tenant)
	if !ok {
		return taskID, rec, JsonrpcError(reqID, ErrTaskNotFound, "task not found: "+taskID)
	}
	return taskID, rec, nil
}

func (a *A2AAdapter) rpcTasksGet(reqID any, params *pyjson.Object, agent *servedAgent) *pyjson.Object {
	_, rec, errObj := a.findTask(reqID, params, agent)
	if errObj != nil {
		return errObj
	}
	return JsonrpcResult(reqID, ToTask(rec, true))
}

// Newest first page of visible tasks with the next page token and the total
func (a *A2AAdapter) rpcTasksList(reqID any, params *pyjson.Object, agent *servedAgent) *pyjson.Object {
	offset := gateway.CoercePort(pyOr(getOr(params, "pageToken"), 0), 0)
	pageSize := gateway.CoercePort(pyOr(getOr(params, "pageSize"), 50), 50)
	slug, tenant := a.scopeForAgent(agent)
	recs, next, total, err := a.tasks.List(ListOptions{ContextID: pyrepr.Str(pyOr(getOr(params, "contextId"), "")),
		State:    pyrepr.Str(pyOr(pyOr(getOr(params, "status"), getOr(params, "state")), "")),
		PageSize: pageSize, Offset: max(0, offset), AgentSlug: slug, Tenant: tenant})
	if err != nil {
		panic(err)
	}
	includeArtifacts := pyrepr.Truthy(pyGet(params, "includeArtifacts", false))
	tasks := make([]any, 0, len(recs))
	for _, r := range recs {
		tasks = append(tasks, ToTask(r, includeArtifacts))
	}
	nextToken := ""
	if next != 0 {
		nextToken = strconv.Itoa(next)
	}
	return JsonrpcResult(reqID, obj("tasks", tasks, "nextPageToken", nextToken, "pageSize", max(1, min(pageSize, 100)), "totalSize", total))
}

// Cancels a live task, resetting its context turns and resolving its waiter
func (a *A2AAdapter) rpcTasksCancel(reqID any, params *pyjson.Object, agent *servedAgent) *pyjson.Object {
	taskID, rec, errObj := a.findTask(reqID, params, agent)
	if errObj != nil {
		return errObj
	}
	if IsTerminalState(rec.State) {
		return JsonrpcError(reqID, ErrTaskNotCancelable, fmt.Sprintf("task %s already %s", taskID, rec.State))
	}
	a.tasks.Complete(taskID, StateCanceled, "")
	a.turns.Reset(rec.ContextID)
	a.resolveTask(taskID, StateCanceled, "")
	slug, tenant := a.scopeForAgent(agent)
	if fresh, ok := a.tasks.Get(taskID, slug, tenant); ok {
		rec = fresh
	}
	return JsonrpcResult(reqID, ToTask(rec, true))
}

// Stores the push callback a v1.0 message carries in configuration.taskPushNotificationConfig
func (a *A2AAdapter) registerInlinePush(taskID string, params *pyjson.Object, agent *servedAgent) {
	cfg := pyOr(pyGet(pyOr(getOr(params, "configuration"), map[string]any{}), "taskPushNotificationConfig", nil), map[string]any{})
	var url any = ""
	if isMapping(cfg) {
		url = pyOr(pyGet(cfg, "url", nil), "")
		if !pyrepr.Truthy(url) {
			url = pyOr(pyGet(pyOr(pyGet(cfg, "pushNotificationConfig", nil), map[string]any{}), "url", nil), "")
		}
	}
	if pyrepr.Truthy(url) {
		slug, tenant := a.scopeForAgent(agent)
		a.tasks.SetPushConfig(taskID, pyrepr.Str(url), slug, tenant)
	}
}

func (a *A2AAdapter) rpcPushConfigCreate(reqID any, params *pyjson.Object, agent *servedAgent) *pyjson.Object {
	taskID := pyrepr.Str(pyOr(getOr(params, "taskId"), ""))
	cfg := pyOr(pyOr(getOr(params, "pushNotificationConfig"), getOr(params, "config")), map[string]any{})
	url := pyrepr.Str(pyOr(pyGet(cfg, "url", nil), ""))
	if taskID == "" || url == "" {
		return JsonrpcError(reqID, ErrInvalidParams, "taskId and pushNotificationConfig.url required")
	}
	slug, tenant := a.scopeForAgent(agent)
	if stored := a.tasks.SetPushConfig(taskID, url, slug, tenant); stored != nil {
		return JsonrpcResult(reqID, stored)
	}
	return JsonrpcError(reqID, ErrTaskNotFound, "task not found: "+taskID)
}

// Shared push config get, list and delete whose op reports not found as false
func (a *A2AAdapter) pushConfigOp(reqID any, params *pyjson.Object, agent *servedAgent,
	op func(taskID, configID, slug, tenant string) (any, bool), render func(any) any) *pyjson.Object {
	taskID := pyrepr.Str(pyOr(getOr(params, "taskId"), ""))
	if taskID == "" {
		return JsonrpcError(reqID, ErrInvalidParams, "taskId required")
	}
	slug, tenant := a.scopeForAgent(agent)
	found, ok := op(taskID, pyrepr.Str(pyOr(pyOr(getOr(params, "id"), getOr(params, "configId")), "")), slug, tenant)
	if !ok {
		return JsonrpcError(reqID, ErrTaskNotFound, "push config not found for task: "+taskID)
	}
	return JsonrpcResult(reqID, render(found))
}

func (a *A2AAdapter) rpcPushConfigGet(reqID any, params *pyjson.Object, agent *servedAgent) *pyjson.Object {
	return a.pushConfigOp(reqID, params, agent, func(taskID, configID, slug, tenant string) (any, bool) {
		cfg := a.tasks.GetPushConfig(taskID, configID, slug, tenant)
		return cfg, cfg != nil && cfg.Len() > 0
	}, func(found any) any { return found })
}

// Push configs of a task, an empty list being a valid result
func (a *A2AAdapter) rpcPushConfigList(reqID any, params *pyjson.Object, agent *servedAgent) *pyjson.Object {
	return a.pushConfigOp(reqID, params, agent, func(taskID, _, slug, tenant string) (any, bool) {
		return a.tasks.ListPushConfigs(taskID, slug, tenant), true
	}, func(found any) any {
		configs := []any{}
		for _, c := range found.([]*pyjson.Object) {
			if c != nil && c.Len() > 0 {
				configs = append(configs, c)
			}
		}
		return obj("configs", configs, "nextPageToken", "")
	})
}

func (a *A2AAdapter) rpcPushConfigDelete(reqID any, params *pyjson.Object, agent *servedAgent) *pyjson.Object {
	return a.pushConfigOp(reqID, params, agent, func(taskID, configID, slug, tenant string) (any, bool) {
		return nil, a.tasks.DeletePushConfig(taskID, configID, slug, tenant)
	}, func(any) any { return obj("deleted", true) })
}

// Posts the signed v1.0 status payload to the task callback once it passes the SSRF check
func (a *A2AAdapter) sendPushNotification(ctx context.Context, taskID, contextID, reply, state string) {
	fail := func(format string, args ...any) {
		SharedMetrics.PushFailed.Add(1)
		slog.Warn(fmt.Sprintf("A2A: push notification for task %s "+format, append([]any{taskID}, args...)...))
	}
	callbackURL := a.tasks.PopPushURL(taskID)
	if callbackURL == "" {
		return
	}
	localhost := a.securityContext.LocalhostOnly()
	if !IsSafeCallbackURL(ctx, callbackURL, &localhost) {
		fail("blocked — unsafe callback URL: %s", callbackURL)
		return
	}
	payload := StatusUpdate(taskID, contextID, state, pystr.Slice(reply, 0, 2000))
	headers := map[string]string{"Content-Type": "application/json"}
	signature, err := a.securityContext.SignPushPayload(payload)
	if err != nil {
		panic(err)
	}
	if signature != "" {
		headers["X-A2A-Signature"] = signature
	}
	status, err := urlopenPost(ctx, callbackURL, payload, headers, 10*time.Second)
	if err != nil {
		fail("failed: %s", err)
		return
	}
	if status < 200 || status >= 300 {
		fail("got HTTP %d", status)
		return
	}
	SharedMetrics.PushSent.Add(1)
	slog.Debug(fmt.Sprintf("A2A: push notification sent for task %s", taskID))
}

// Fulfils the oldest reply waiter of the context, only a final notify send satisfying it
func (a *A2AAdapter) Send(ctx context.Context, chatID, content, replyTo string, metadata map[string]any) (gateway.SendResult, error) {
	if !pyrepr.Truthy(metadata["notify"]) {
		slog.Debug(fmt.Sprintf("A2A: ignoring non-final send for context %s", chatID))
	} else if !a.resolveOldestForContext(chatID, StateCompleted, content) {
		slog.Debug(fmt.Sprintf("A2A: send() for context %s had no pending waiter", chatID))
	}
	return gateway.SendResult{Success: true, MessageID: strconv.FormatInt(time.Now().UnixMilli(), 10)}, nil
}

func (a *A2AAdapter) SendTyping(ctx context.Context, chatID string, metadata map[string]any) error {
	return nil
}

func (a *A2AAdapter) GetChatInfo(ctx context.Context, chatID string) (map[string]any, error) {
	return map[string]any{"name": "a2a:" + chatID, "type": "dm"}, nil
}

// Resolves the task waiter when processing ends without a final send, a streamed reply as its text
func (a *A2AAdapter) OnProcessingComplete(ctx context.Context, event *gateway.MessageEvent, outcome gateway.ProcessingOutcome) error {
	taskID := ""
	if event.MessageID != nil {
		taskID = *event.MessageID
	}
	if taskID == "" {
		return nil
	}
	state, text := StateCompleted, ""
	if streamed := event.StreamedFinalResponse(); streamed != nil {
		text = *streamed
	}
	switch outcome {
	case gateway.ProcessingFailure:
		state, text = StateFailed, "[agent processing failed]"
	case gateway.ProcessingCancelled:
		state, text = StateCanceled, ""
	}
	a.resolveTask(taskID, state, text)
	return nil
}
