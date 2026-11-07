package a2a

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/gateway"
	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/internal/pycodecs"
	"github.com/nickheyer/icebath/pkg/internal/pyint"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
	"github.com/nickheyer/icebath/pkg/plugins"
	"github.com/nickheyer/icebath/pkg/tools"
)

const (
	defaultToolTimeout    = 120
	orchestrateMaxWorkers = 6
)

// PyError is a Python exception raised by the client tools, named for the handler error text
type PyError struct {
	Type string
	Msg  string
}

func (e *PyError) Error() string      { return e.Msg }
func (e *PyError) PyTypeName() string { return e.Type }

func attributeError(v any, attr string) error {
	return &PyError{"AttributeError", fmt.Sprintf("'%s' object has no attribute '%s'", pyrepr.TypeName(v), attr)}
}

// HTTPError is urllib.error.HTTPError, a non success status from a peer
type HTTPError struct {
	Code   int
	Reason string
}

func (e *HTTPError) Error() string      { return fmt.Sprintf("HTTP Error %d: %s", e.Code, e.Reason) }
func (e *HTTPError) PyTypeName() string { return "HTTPError" }

// Read only config view the tools read their peers from, swapped by tests
var loadToolsConfig = func(ctx context.Context) (map[string]any, error) {
	tree, err := config.LoadReadonly(ctx, home.Dir(ctx), nil)
	return map[string]any(tree), err
}

// Python dict.get on a mapping value, the AttributeError for any other value
func pyGet(v any, key string, def any) any {
	switch m := v.(type) {
	case map[string]any:
		if x, ok := m[key]; ok {
			return x
		}
		return def
	case *pyjson.Object:
		if m != nil {
			if x, ok := m.Get(key); ok {
				return x
			}
			return def
		}
	}
	panic(attributeError(v, "get"))
}

// Value or the fallback when the value is falsy, the Python or idiom
func pyOr(v, fallback any) any {
	if pyrepr.Truthy(v) {
		return v
	}
	return fallback
}

func isMapping(v any) bool {
	switch m := v.(type) {
	case map[string]any:
		return true
	case *pyjson.Object:
		return m != nil
	}
	return false
}

// Keys of a mapping in file or insertion order
func mappingKeys(v any) []string {
	switch m := v.(type) {
	case map[string]any:
		return config.MappingKeys(m)
	case *pyjson.Object:
		return slices.Clone(m.Keys)
	}
	return nil
}

// Iteration of a Python value, list items, string characters or mapping keys, the TypeError otherwise
func pyIter(v any) []any {
	switch x := v.(type) {
	case nil:
		panic(&PyError{"TypeError", "'NoneType' object is not iterable"})
	case []any:
		return x
	case string:
		var out []any
		for _, r := range x {
			out = append(out, string(r))
		}
		return out
	case map[string]any, *pyjson.Object:
		var out []any
		for _, k := range mappingKeys(x) {
			out = append(out, k)
		}
		return out
	}
	panic(&PyError{"TypeError", fmt.Sprintf("'%s' object is not iterable", pyrepr.TypeName(v))})
}

// Python membership of needle in a list, a substring of a string or a key of a mapping
func pyIn(needle string, container any) bool {
	switch c := container.(type) {
	case []any:
		for _, item := range c {
			if pyrepr.Equal(item, needle) {
				return true
			}
		}
		return false
	case string:
		return strings.Contains(c, needle)
	case map[string]any, *pyjson.Object:
		return slices.Contains(mappingKeys(c), needle)
	}
	panic(&PyError{"TypeError", fmt.Sprintf("argument of type '%s' is not a container or iterable", pyrepr.TypeName(container))})
}

// Configured a2a_agents mapping of the config, an empty mapping when unset
func configuredPeers(ctx context.Context) (any, error) {
	cfg, err := loadToolsConfig(ctx)
	if err != nil {
		return nil, err
	}
	return pyOr(pyGet(pyOr(cfg, map[string]any{}), "a2a_agents", nil), map[string]any{}), nil
}

type peerSpec struct {
	URL          any
	Auth         any
	Timeout      int
	Capabilities any
	Tenant       any
}

// Python int of a config value with its ValueError or TypeError
func pyIntOf(v any) int {
	n, err := pyint.Of(v)
	if err != nil {
		panic(err)
	}
	return int(n.Int64())
}

func peerFromEntry(entry any) peerSpec {
	return peerSpec{URL: pyGet(entry, "url", ""), Auth: pyOr(pyGet(entry, "auth", map[string]any{}), map[string]any{}),
		Timeout: pyIntOf(pyGet(entry, "timeout", defaultToolTimeout))}
}

// Peer for a configured name or a direct URL, false when the name is unknown
func resolvePeer(ctx context.Context, agent string) (peerSpec, bool) {
	if strings.HasPrefix(agent, "http://") || strings.HasPrefix(agent, "https://") {
		return peerSpec{URL: agent, Auth: map[string]any{}, Timeout: defaultToolTimeout, Capabilities: []any{}}, true
	}
	peers, err := configuredPeers(ctx)
	if err != nil {
		panic(err)
	}
	entry := pyGet(peers, agent, nil)
	if !pyrepr.Truthy(entry) {
		return peerSpec{}, false
	}
	p := peerFromEntry(entry)
	p.Capabilities = pyOr(pyGet(entry, "capabilities", []any{}), []any{})
	p.Tenant = pyGet(entry, "tenant", "")
	return p, true
}

// Bearer Authorization header of a peer auth block, empty for any other auth
func authHeader(auth any) map[string]string {
	if pyrepr.Truthy(auth) && pyrepr.Equal(pyGet(auth, "type", nil), "bearer") && pyrepr.Truthy(pyGet(auth, "token", nil)) {
		return map[string]string{"Authorization": "Bearer " + pyrepr.Str(pyGet(auth, "token", nil))}
	}
	return map[string]string{}
}

// Request a peer endpoint and decode its UTF-8 JSON body, the HTTPError for a non success status
func httpJSON(ctx context.Context, url string, headers map[string]string, timeout int, method string, data []byte) (any, error) {
	var body io.Reader
	if data != nil {
		body = bytes.NewReader(data)
	}
	req, err := http.NewRequestWithContext(ctx, method, url, body)
	if err == nil && req.URL.Scheme == "" {
		err = errors.New("no scheme")
	}
	if err != nil {
		return nil, &PyError{"ValueError", "unknown url type: " + pyrepr.StrRepr(url)}
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	client := &http.Client{Timeout: time.Duration(timeout) * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return nil, &PyError{"URLError", fmt.Sprintf("<urlopen error %s>", err)}
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, &PyError{"IncompleteRead", err.Error()}
	}
	if resp.StatusCode >= 400 {
		return nil, &HTTPError{Code: resp.StatusCode, Reason: strings.TrimPrefix(resp.Status, fmt.Sprintf("%d ", resp.StatusCode))}
	}
	text, err := pycodecs.Decode(raw, "utf-8")
	if err != nil {
		return nil, &PyError{"ValueError", err.Error()}
	}
	v, err := pyjson.Loads(text)
	if err != nil {
		return nil, &PyError{"ValueError", err.Error()}
	}
	return v, nil
}

// GET of a peer JSON document, swapped by tests
var httpGetJSON = func(ctx context.Context, url string, headers map[string]string, timeout int) (any, error) {
	return httpJSON(ctx, url, headers, timeout, http.MethodGet, nil)
}

// POST of a JSON-RPC body with the A2A version header, swapped by tests
var httpPostJSON = func(ctx context.Context, url string, body *pyjson.Object, headers map[string]string, timeout int) (any, error) {
	hdrs := map[string]string{"Content-Type": "application/json", "A2A-Version": ProtocolVersion}
	for k, v := range headers {
		hdrs[k] = v
	}
	data, err := pyjson.Dumps(body)
	if err != nil {
		return nil, err
	}
	return httpJSON(ctx, url, hdrs, timeout, http.MethodPost, []byte(data))
}

// Agent card of a base URL, the v0.2 agent.json alias after a 404 on agent-card.json
func fetchCard(ctx context.Context, baseURL string, headers map[string]string, timeout int) (any, error) {
	base := strings.TrimRight(baseURL, "/")
	card, err := httpGetJSON(ctx, base+"/.well-known/agent-card.json", headers, timeout)
	var he *HTTPError
	if err == nil || !errors.As(err, &he) || he.Code != 404 {
		return card, err
	}
	return httpGetJSON(ctx, base+"/.well-known/agent.json", headers, timeout)
}

// JSONRPC entry of the card supportedInterfaces that names a URL, nil when none does
func selectJSONRPCInterface(card any) any {
	if !isMapping(card) {
		return nil
	}
	for _, iface := range pyIter(pyOr(pyGet(card, "supportedInterfaces", []any{}), []any{})) {
		if isMapping(iface) && pyrepr.Equal(pyGet(iface, "protocolBinding", nil), "JSONRPC") && pyrepr.Truthy(pyGet(iface, "url", nil)) {
			return iface
		}
	}
	return nil
}

// RPC URL from the card JSONRPC interface, then its legacy top level url, then the base
func rpcURL(baseURL any, card any) string {
	if iface := selectJSONRPCInterface(card); iface != nil {
		return pyrepr.Str(pyGet(iface, "url", nil))
	}
	if isMapping(card) {
		if u, ok := pyGet(card, "url", nil).(string); ok && u != "" {
			return u
		}
	}
	base, ok := baseURL.(string)
	if !ok {
		panic(attributeError(baseURL, "rstrip"))
	}
	return strings.TrimRight(base, "/")
}

// One SendMessage to a peer returning the reply text, context id and state, with redaction, audit and metrics
func sendTask(ctx context.Context, agentLabel string, peer peerSpec, message, contextID string) (reply, replyCtx, state string, err error) {
	defer func() {
		if rec := recover(); rec != nil {
			e, ok := rec.(error)
			if !ok {
				panic(rec)
			}
			err = e
		}
	}()
	headers := authHeader(pyOr(peer.Auth, map[string]any{}))
	timeout := peer.Timeout
	var card any
	if base, ok := peer.URL.(string); ok {
		if c, cerr := fetchCard(ctx, base, headers, min(timeout, 30)); cerr == nil {
			card = c
		}
	}
	cid := contextID
	if cid == "" {
		cid = NewContextID()
	}
	safe := RedactOutbound(message)
	params := pyjson.ObjectOf("message", TextMessage(RoleUser, safe, cid))
	rpcBody := pyjson.ObjectOf("jsonrpc", "2.0", "id", NewTaskID(), "method", "SendMessage", "params", params)
	tenant := pyrepr.Str(pyOr(peer.Tenant, ""))
	if iface := selectJSONRPCInterface(card); iface != nil && pyrepr.Truthy(pyGet(iface, "tenant", nil)) {
		tenant = pyrepr.Str(pyGet(iface, "tenant", nil))
	}
	if tenant != "" {
		params.Set("tenant", tenant)
	}
	rpcID, _ := rpcBody.Get("id")
	Audit(ctx, "outbound", agentLabel, rpcID.(string), safe)
	PersistMessage(ctx, cid, "user", safe, rpcID.(string), "")
	SharedMetrics.OutboundTotal.Add(1)
	resp, err := httpPostJSON(ctx, rpcURL(peer.URL, card), rpcBody, headers, timeout)
	if err != nil {
		return "", "", "", err
	}
	if pyIn("error", resp) {
		if _, isStr := resp.(string); isStr {
			panic(&PyError{"TypeError", "string indices must be integers, not 'str'"})
		}
		if !isMapping(resp) {
			panic(&PyError{"TypeError", fmt.Sprintf("%s indices must be integers or slices, not str", pyrepr.TypeName(resp))})
		}
		e := pyGet(resp, "error", nil)
		return "", "", "", &PyError{"ValueError", fmt.Sprintf("Peer '%s' returned an error: %s", agentLabel,
			pyrepr.Str(pyGet(e, "message", e)))}
	}
	payload := UnwrapSendMessageResponse(pyGet(resp, "result", map[string]any{}))
	reply = replyTextFromResult(payload)
	replyCtx = cid
	if isMapping(payload) {
		replyCtx = pyrepr.Str(pyGet(payload, "contextId", cid))
		st := pyGet(pyOr(pyGet(payload, "status", nil), map[string]any{}), "state", "")
		s, ok := st.(string)
		if !ok {
			panic(attributeError(st, "replace"))
		}
		state = s
	}
	PersistMessage(ctx, replyCtx, "agent", reply, rpcID.(string), "")
	SharedMetrics.InboundTotal.Add(1)
	return reply, replyCtx, state, nil
}

// Object form of a mapping value for the protocol text helpers
func asObject(v any) *pyjson.Object {
	switch m := v.(type) {
	case *pyjson.Object:
		if m != nil {
			return m
		}
	case map[string]any:
		o := pyjson.NewObject()
		for _, k := range mappingKeys(m) {
			o.Set(k, m[k])
		}
		return o
	}
	panic(attributeError(v, "get"))
}

// Reply text of a send result, artifacts first, then the status message, else the bare message
func replyTextFromResult(result any) string {
	result = UnwrapSendMessageResponse(result)
	if !isMapping(result) {
		return pyrepr.Str(result)
	}
	for _, artifact := range pyIter(pyOr(pyGet(result, "artifacts", []any{}), []any{})) {
		if txt := ExtractText(asObject(artifact)); txt != "" {
			return txt
		}
	}
	return ExtractText(asObject(pyOr(pyGet(pyOr(pyGet(result, "status", map[string]any{}), map[string]any{}), "message", nil), result)))
}

const authErrText = "Error: peer '%s' rejected auth (HTTP %d). Check the configured token."

func httpCallError(agent string, code int) string {
	switch code {
	case 401, 403:
		return fmt.Sprintf(authErrText, agent, code)
	case 429:
		return fmt.Sprintf("Error: peer '%s' rate limited us (HTTP 429). Retry later.", agent)
	}
	return fmt.Sprintf("Error: call to '%s' failed — HTTP %d.", agent, code)
}

// Stripped text of the first truthy argument among the names, the str of it
func argText(args map[string]any, names ...string) string {
	var v any
	for _, n := range names {
		if v = args[n]; pyrepr.Truthy(v) {
			break
		}
	}
	return pystr.Strip(pyrepr.Str(pyOr(v, "")))
}

// Agent Card of a URL summarized as name, protocol, capabilities and skills
func A2ADiscover(ctx context.Context, args map[string]any) string {
	url := argText(args, "url")
	if url == "" {
		return "Error: 'url' is required (e.g. http://localhost:9999)."
	}
	card, err := fetchCard(ctx, url, map[string]string{}, defaultToolTimeout)
	if err != nil {
		var he *HTTPError
		if errors.As(err, &he) {
			return fmt.Sprintf("Error: discovery failed — HTTP %d from %s.", he.Code, url)
		}
		return fmt.Sprintf("Error: could not reach %s — %s.", url, err)
	}
	caps := pyOr(pyGet(card, "capabilities", map[string]any{}), map[string]any{})
	skills := pyOr(pyGet(card, "skills", []any{}), []any{})
	auth := "no"
	if pyrepr.Truthy(pyGet(card, "security", nil)) {
		auth = "yes"
	}
	var protos []string
	for _, i := range pyIter(pyOr(pyGet(card, "supportedInterfaces", []any{}), []any{})) {
		if isMapping(i) {
			protos = append(protos, fmt.Sprintf("%s v%s", pyrepr.Str(pyGet(i, "protocolBinding", "?")), pyrepr.Str(pyGet(i, "protocolVersion", "?"))))
		}
	}
	proto := strings.Join(protos, ", ")
	if proto == "" {
		proto = fmt.Sprintf("v%s (pre-1.0 card)", pyrepr.Str(pyGet(card, "protocolVersion", "?")))
	}
	skillList := pyIter(skills)
	lines := []string{"Agent: " + pyrepr.Str(pyGet(card, "name", "?")), "Description: " + pyrepr.Str(pyGet(card, "description", "")),
		"URL: " + rpcURL(url, card), "Protocol: " + proto,
		fmt.Sprintf("Streaming: %s  Push: %s  Auth required: %s", pyrepr.Str(pyrepr.Truthy(pyGet(caps, "streaming", nil))),
			pyrepr.Str(pyrepr.Truthy(pyGet(caps, "pushNotifications", nil))), auth),
		fmt.Sprintf("Skills (%d):", len(skillList))}
	if _, isDict := skills.([]any); !isDict && isMapping(skills) {
		panic(&PyError{"KeyError", "slice(None, 20, None)"})
	}
	for _, s := range skillList[:min(len(skillList), 20)] {
		lines = append(lines, fmt.Sprintf("  - %s: %s", pyrepr.Str(pyGet(s, "name", pyGet(s, "id", "?"))), pyrepr.Str(pyGet(s, "description", ""))))
	}
	return strings.Join(lines, "\n")
}

// Task sent to a configured peer or URL, the reply framed with its context id and state
func A2ACall(ctx context.Context, args map[string]any) string {
	agent := argText(args, "agent", "agent_name", "name")
	message := argText(args, "message", "text", "task")
	contextID := argText(args, "context_id", "contextId")
	if agent == "" || message == "" {
		return "Error: both 'agent' and 'message' are required."
	}
	peer, ok := resolvePeer(ctx, agent)
	if !ok || !pyrepr.Truthy(peer.URL) {
		return fmt.Sprintf("Error: unknown agent '%s'. Configure it under 'a2a_agents' in config.yaml or pass a full http(s):// URL.", agent)
	}
	reply, replyCtx, state, err := sendTask(ctx, agent, peer, message, contextID)
	if err != nil {
		var he *HTTPError
		var pe *PyError
		switch {
		case errors.As(err, &he):
			return httpCallError(agent, he.Code)
		case errors.As(err, &pe) && pe.Type == "ValueError":
			return pe.Msg
		}
		return fmt.Sprintf("Error: call to '%s' failed — %s.", agent, err)
	}
	shortState := strings.ToLower(strings.ReplaceAll(strings.ReplaceAll(state, "TASK_STATE_", ""), "_", "-"))
	header := fmt.Sprintf("[%s · context %s", agent, replyCtx)
	if state != "" {
		header += " · " + shortState
	}
	header += "]"
	body := reply
	if body == "" {
		body = "(no text reply)"
	}
	if state == StateInputRequired {
		body += fmt.Sprintf("\n\n(The peer needs more input — answer by calling a2a_call again with context_id '%s'.)", replyCtx)
	}
	return header + "\n" + body
}

// Python str.join of a capabilities value, the TypeError for a non string item
func joinStrings(v any, sep string) string {
	var parts []string
	for i, item := range pyIter(v) {
		s, ok := item.(string)
		if !ok {
			panic(&PyError{"TypeError", fmt.Sprintf("sequence item %d: expected str instance, %s found", i, pyrepr.TypeName(item))})
		}
		parts = append(parts, s)
	}
	return strings.Join(parts, sep)
}

// Configured peers, persisted conversations and the metrics line
func A2AList(ctx context.Context, _ map[string]any) string {
	peers, err := configuredPeers(ctx)
	if err != nil {
		panic(err)
	}
	var lines []string
	if pyrepr.Truthy(peers) {
		names := mappingKeys(peers)
		if !isMapping(peers) {
			panic(attributeError(peers, "items"))
		}
		lines = append(lines, fmt.Sprintf("Configured peers (%d):", len(names)))
		for _, name := range names {
			entry := pyGet(peers, name, nil)
			caps := pyGet(entry, "capabilities", []any{})
			line := fmt.Sprintf("  - %s: %s (auth: %s)", name, pyrepr.Str(pyGet(entry, "url", "?")),
				pyrepr.Str(pyGet(pyOr(pyGet(entry, "auth", nil), map[string]any{}), "type", "none")))
			if pyrepr.Truthy(caps) {
				line += " caps: " + joinStrings(caps, ", ")
			}
			lines = append(lines, line)
		}
	} else {
		lines = append(lines, "No peers configured. Add them under 'a2a_agents' in config.yaml.")
	}
	if convos := ListConversations(ctx); len(convos) > 0 {
		lines = append(lines, "", fmt.Sprintf("Persisted conversations (%d) — recall with a2a_history:", len(convos)))
		for _, c := range convos[:min(len(convos), 25)] {
			lines = append(lines, "  - "+c)
		}
	}
	m := SharedMetrics.Snapshot()
	g := func(k string) string { v, _ := m.Get(k); return pyrepr.Str(v) }
	lines = append(lines, "", fmt.Sprintf("Metrics: %s in / %s out, %s completed, %s failed, %s streams, %s push sent, "+
		"%s anti-loop, %s rate-limited, avg %sms", g("inbound_total"), g("outbound_total"), g("tasks_completed"),
		g("tasks_failed"), g("streams_started"), g("push_sent"), g("anti_loop_triggers"), g("rate_limit_triggers"),
		g("avg_latency_ms")))
	return strings.Join(lines, "\n")
}

// Persisted conversation transcript of a context id, each message capped at a thousand characters
func A2AHistory(ctx context.Context, args map[string]any) string {
	contextID := argText(args, "context_id", "contextId")
	if contextID == "" {
		return "Error: 'context_id' is required (see a2a_list for known conversations)."
	}
	limit := max(1, min(gateway.CoercePort(pyOr(args["limit"], int64(50)), 50), 200))
	messages := LoadConversation(ctx, contextID, limit)
	if len(messages) == 0 {
		return fmt.Sprintf("No persisted conversation for context '%s'.", contextID)
	}
	lines := []string{fmt.Sprintf("Conversation %s (last %d messages):", contextID, len(messages))}
	for _, m := range messages {
		raw, _ := m.Get("text")
		text := pystr.Strip(pyrepr.Str(pyOr(raw, "")))
		if pystr.Len(text) > 1000 {
			text = pystr.Slice(text, 0, 1000) + " …[truncated]"
		}
		role, ok := m.Get("role")
		if !ok {
			role = "?"
		}
		lines = append(lines, fmt.Sprintf("[%s] %s", pyrepr.Str(role), text))
	}
	return strings.Join(lines, "\n")
}

type peerMatch struct {
	name  string
	entry any
}

// Configured peers advertising the capability, the star matching every peer
func matchPeersByCapability(ctx context.Context, capability string) []peerMatch {
	peers, err := configuredPeers(ctx)
	if err != nil {
		panic(err)
	}
	if !isMapping(peers) {
		panic(attributeError(peers, "items"))
	}
	var out []peerMatch
	for _, name := range mappingKeys(peers) {
		entry := pyGet(peers, name, nil)
		if pyIn(capability, pyOr(pyGet(entry, "capabilities", []any{}), []any{})) || capability == "*" {
			out = append(out, peerMatch{name, entry})
		}
	}
	return out
}

// One peer call returning its name and reply, any raised error rendered as an Error line
var callPeerSync = func(ctx context.Context, agentName string, peerEntry any, message, contextID string) (name, reply string) {
	defer func() {
		if rec := recover(); rec != nil {
			e, ok := rec.(error)
			if !ok {
				panic(rec)
			}
			name, reply = agentName, "Error: "+e.Error()
		}
	}()
	r, _, _, err := sendTask(ctx, agentName, peerFromEntry(peerEntry), message, contextID)
	if err != nil {
		return agentName, "Error: " + err.Error()
	}
	if r == "" {
		r = "(no reply)"
	}
	return agentName, r
}

// Task fanned out to the peers with a capability, aggregated as all, first or best replies
func A2AOrchestrate(ctx context.Context, args map[string]any) string {
	capability := argText(args, "capability")
	message := argText(args, "message", "task")
	mode := pystr.Lower(argText(args, "mode"))
	if mode == "" {
		mode = "all"
	}
	if mode != "all" && mode != "first" && mode != "best" {
		mode = "all"
	}
	contextID := argText(args, "context_id")
	if message == "" {
		return "Error: 'message' is required."
	}
	if capability == "" {
		return "Error: 'capability' is required (or use '*' for all peers)."
	}
	matches := matchPeersByCapability(ctx, capability)
	if len(matches) == 0 {
		return fmt.Sprintf("Error: no configured peers advertise capability '%s'.", capability)
	}
	results := runPeerPool(ctx, matches, message, contextID, mode == "first")
	slices.SortStableFunc(results, func(a, b [2]string) int { return strings.Compare(a[0], b[0]) })
	var successes [][2]string
	for _, r := range results {
		if !strings.HasPrefix(r[1], "Error:") {
			successes = append(successes, r)
		}
	}
	if mode == "best" || mode == "first" {
		if len(successes) == 0 {
			lines := []string{"All peers failed:"}
			for _, r := range results {
				lines = append(lines, fmt.Sprintf("  %s: %s", r[0], r[1]))
			}
			return strings.Join(lines, "\n")
		}
		pick := successes[0]
		if mode == "best" {
			for _, s := range successes[1:] {
				if pystr.Len(s[1]) > pystr.Len(pick[1]) {
					pick = s
				}
			}
		}
		return fmt.Sprintf("[%s: %s]\n%s", mode, pick[0], pick[1])
	}
	lines := []string{fmt.Sprintf("Orchestrated '%s' to %d peer(s):", capability, len(matches))}
	for _, r := range results {
		lines = append(lines, fmt.Sprintf("\n--- %s ---", r[0]), r[1])
	}
	return strings.Join(lines, "\n")
}

// Peer calls on a bounded pool in completion order, a first mode success cancelling peers not yet started
func runPeerPool(ctx context.Context, matches []peerMatch, message, contextID string, stopOnSuccess bool) [][2]string {
	jobs := make(chan peerMatch, len(matches))
	for _, m := range matches {
		jobs <- m
	}
	close(jobs)
	done := make(chan [2]string, len(matches))
	var cancelled sync.Once
	stop := make(chan struct{})
	var wg sync.WaitGroup
	for range min(len(matches), orchestrateMaxWorkers) {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for m := range jobs {
				select {
				case <-stop:
					continue
				default:
				}
				name, reply := callPeerSync(ctx, m.name, m.entry, message, contextID)
				done <- [2]string{name, reply}
			}
		}()
	}
	var results [][2]string
	for range matches {
		r := <-done
		results = append(results, r)
		if stopOnSuccess && !strings.HasPrefix(r[1], "Error:") {
			cancelled.Do(func() { close(stop) })
			break
		}
		if len(results) == len(matches) {
			break
		}
	}
	wg.Wait()
	return results
}

func strProp(description string) map[string]any {
	return map[string]any{"type": "string", "description": description}
}

type a2aTool struct {
	name        string
	handler     func(context.Context, map[string]any) string
	description string
	properties  map[string]any
	required    []string
}

var a2aTools = []a2aTool{
	{"a2a_discover", A2ADiscover,
		"Fetch a remote agent's A2A Agent Card (name, capabilities, skills) from a URL. " +
			"Use this to find out what a remote agent can do before calling it.",
		map[string]any{"url": strProp("Base URL of the remote A2A agent, e.g. http://localhost:9999")}, []string{"url"}},
	{"a2a_call", A2ACall,
		"Send a natural-language task to a remote A2A agent and return its reply. The agent is a peer " +
			"(any A2A-compliant framework), not a sub-agent you control. Pass 'context_id' from a previous " +
			"reply to continue a multi-turn exchange.",
		map[string]any{"agent": strProp("Configured peer name (from a2a_agents) or a full http(s):// URL."),
			"message":    strProp("The task / message to send the peer, in natural language."),
			"context_id": strProp("Optional: context id from a prior reply, to continue the conversation.")},
		[]string{"agent", "message"}},
	{"a2a_list", A2AList, "List configured A2A peer agents, persisted A2A conversations, and metrics.", map[string]any{}, nil},
	{"a2a_history", A2AHistory,
		"Recall a persisted A2A conversation transcript by context_id. It survives restarts and " +
			"context compaction; use a2a_list to see known context ids.",
		map[string]any{"context_id": strProp("Context id of the conversation to recall."),
			"limit": map[string]any{"type": "integer", "description": "Max messages to return (default 50, max 200)."}},
		[]string{"context_id"}},
	{"a2a_orchestrate", A2AOrchestrate,
		"Fan-out a task to multiple peer agents by capability. Peers are matched from config.yaml " +
			"a2a_agents.*.capabilities. Modes: 'all' (return all replies), 'first' (first successful), " +
			"'best' (longest successful reply).",
		map[string]any{"capability": strProp("Capability to match (e.g. 'research', 'code') or '*' for all peers."),
			"message": strProp("The task to send to all matching peers."),
			"mode": map[string]any{"type": "string", "enum": []any{"all", "first", "best"},
				"description": "How to aggregate results. Default: 'all'."},
			"context_id": strProp("Optional: shared context id for all peers.")},
		[]string{"capability", "message"}},
}

// Serves the client tools only when peers are configured, the inbound platform is enabled or A2A_PORT is set
func a2aToolsAvailable(env *tools.Env) (available bool) {
	ctx := context.Background()
	if env != nil && env.Home != "" {
		ctx = home.WithOverride(ctx, env.Home)
	}
	var cfg any = map[string]any{}
	if loaded, ok := func() (got any, ok bool) {
		defer func() {
			if recover() != nil {
				ok = false
			}
		}()
		c, err := loadToolsConfig(ctx)
		if err != nil {
			return nil, false
		}
		return c, true
	}(); ok {
		cfg = loaded
		if func() (hit bool) {
			defer func() {
				if recover() != nil {
					hit = false
				}
			}()
			return pyrepr.Truthy(pyGet(cfg, "a2a_agents", nil))
		}() {
			return true
		}
	}
	defer func() {
		if recover() != nil {
			available = false
		}
	}()
	if tools.ScopedCredential(env, "A2A_PORT") != "" {
		return true
	}
	a2aCfg := pyOr(pyGet(pyOr(pyGet(cfg, "platforms", nil), map[string]any{}), "a2a", nil), map[string]any{})
	return isMapping(a2aCfg) && pyrepr.Truthy(pyGet(a2aCfg, "enabled", nil))
}

// Tool specs of the a2a toolset, each gated on the opt in check
func toolSpecs() []plugins.ToolSpec {
	specs := make([]plugins.ToolSpec, 0, len(a2aTools))
	for _, t := range a2aTools {
		parameters := map[string]any{"type": "object", "properties": t.properties}
		if len(t.required) > 0 {
			parameters["required"] = t.required
		}
		handler := t.handler
		specs = append(specs, plugins.ToolSpec{Name: t.name, Toolset: "a2a", Description: t.description,
			Schema:  map[string]any{"name": t.name, "description": t.description, "parameters": parameters},
			Handler: func(ctx context.Context, call *tools.Call) string { return handler(ctx, call.Args) },
			Emoji:   "\U0001f9e9", Check: a2aToolsAvailable})
	}
	return specs
}

// Registers the five client tools in the a2a toolset behind the config gate
func RegisterTools(ctx context.Context, pctx *plugins.Context) error {
	for _, spec := range toolSpecs() {
		if _, err := pctx.RegisterTool(ctx, spec); err != nil {
			return err
		}
	}
	return nil
}
