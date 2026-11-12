package mcp

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/tools"
)

// Wall clock bound of the OSV malware preflight before a stdio spawn
var osvMalwareCheckTimeout = 12 * time.Second

// Seconds the stderr tee may drain after a failed stdio session before its tail is read
const stderrDrainTimeout = 2 * time.Second

// JSON-RPC initialize body the content type preflight POSTs
var probeInitializeBody = `{"jsonrpc":"2.0","id":"_probe","method":"initialize","params":{"protocolVersion":"` + probeProtocolVersion +
	`","capabilities":{},"clientInfo":{"name":"icebath-probe","version":"0.1"}}}`

// Transport failure wrapped with the HTTP cause the transport error alone hides
type mcpConnectionError struct {
	msg   string
	cause error
}

func (e *mcpConnectionError) Error() string { return e.msg }

func (e *mcpConnectionError) Unwrap() error { return e.cause }

// Command, env and cwd a stdio child is spawned with, resolved in the current profile scope
func stdioLaunch(ctx context.Context, env *tools.Env, r *Rendered) (string, map[string]string, string, error) {
	cfg := r.Config
	userEnv, _ := cfg["env"].(map[string]any)
	safeEnv, err := buildSafeEnv(ctx, env, userEnv, r.EnvUnset)
	if err != nil {
		return "", nil, "", err
	}
	command, safeEnv, err := resolveStdioCommand(ctx, pyStr(cfg["command"]), safeEnv)
	if err != nil {
		return "", nil, "", err
	}
	cwd := ""
	if v, ok := cfg["cwd"]; ok && v != nil {
		cwd = pyStr(v)
	} else if dir, err := tools.ResolveContextCwd(env, true); err == nil {
		cwd = dir
	}
	return command, safeEnv, cwd, nil
}

// URL and configured headers, overlaid by a server_json live endpoint URL and bearer
func httpEndpoint(ctx context.Context, serverName string, cfg map[string]any) (string, map[string]string, error) {
	url := pyStr(cfg["url"])
	headers := map[string]string{}
	if h, ok := cfg["headers"].(map[string]any); ok {
		for _, k := range orderedKeys(h) {
			headers[k] = pyStr(h[k])
		}
	}
	liveURL, liveHeaders, ok, err := liveEndpoint(ctx, serverName)
	if err != nil {
		return "", nil, err
	}
	if ok {
		url = liveURL
		for k, v := range liveHeaders {
			headers[k] = v
		}
	}
	return url, headers, nil
}

// Resolver of a connection's inputs, swapped by tests that make one identity fail
var connectInputs = resolveConnectInputs

// Inputs a connection opens with plus the configured header names captured before the identity header merges
func resolveConnectInputs(ctx context.Context, env *tools.Env, serverName string, r *Rendered) ([]any, map[string]bool, error) {
	cfg := r.Config
	if _, ok := cfg["url"]; ok {
		url, headers, err := httpEndpoint(ctx, serverName, cfg)
		if err != nil {
			return nil, nil, err
		}
		names := map[string]bool{}
		for k := range headers {
			names[strings.ToLower(k)] = true
		}
		return []any{url, applyIdentityHeader(ctx, serverName, cfg, headers)}, names, nil
	}
	command, safeEnv, cwd, err := stdioLaunch(ctx, env, r)
	if err != nil {
		return nil, nil, err
	}
	return []any{command, safeEnv, cwd}, map[string]bool{}, nil
}

// Malware verdict and npx cache lookup the stdio preflight consults, replaced in tests to observe call order
var (
	malwareCheck   = checkPackageForMalware
	npxCacheLookup = npxCachedBin
)

// OSV malware preflight on the real argv, then the packaged node or cached npx swap
func preflightStdioCommand(ctx context.Context, serverName, command string, args []string) (string, []string, error) {
	type verdict struct {
		msg string
		err error
	}
	ch := make(chan verdict, 1)
	check := malwareCheck
	go func() {
		msg, err := check(command, args)
		ch <- verdict{msg, err}
	}()
	t := time.NewTimer(osvMalwareCheckTimeout)
	defer t.Stop()
	var malware string
	select {
	case v := <-ch:
		if v.err != nil {
			return "", nil, v.err
		}
		malware = v.msg
	case <-t.C:
		mcpLogger().Warn(fmt.Sprintf("MCP server '%s': OSV malware preflight timed out after %.0fs (network slow/unreachable) — proceeding without the check.",
			serverName, osvMalwareCheckTimeout.Seconds()))
	case <-ctx.Done():
		return "", nil, ctx.Err()
	}
	if malware != "" {
		return "", nil, fmt.Errorf("MCP server '%s': %s", serverName, malware)
	}
	if strings.HasPrefix(strings.ToLower(filepath.Base(command)), "npx") {
		pc, pargs, ok, err := packagedNpxEntry(ctx, command, args)
		if err != nil {
			return "", nil, err
		}
		if ok {
			mcpLogger().Debug(fmt.Sprintf("MCP server '%s': running %s on the packaged node", serverName, pargs[0]))
			return pc, pargs, nil
		}
		dc, dargs, ok, err := npxCacheLookup(args)
		if err != nil {
			return "", nil, err
		}
		if ok {
			mcpLogger().Debug(fmt.Sprintf("MCP server '%s': using cached npx binary %s (skipping the resident `npm exec` parent)", serverName, dc))
			command, args = dc, dargs
		}
	}
	return command, args, nil
}

// Session options carrying the sampling and elicitation handlers and the tools refresh hook
func (s *serverTask) sessionOptions() SessionOptions {
	s.mu.Lock()
	defer s.mu.Unlock()
	return SessionOptions{Name: s.name, Env: s.env, Sampler: s.sampler, Elicitor: s.elicitor, OnToolsListChanged: s.scheduleToolsRefresh}
}

// Protocol era the handshake negotiates, auto when unset
func protocolMode(cfg map[string]any) string {
	v, ok := cfg["protocol"]
	if !ok {
		return "auto"
	}
	return strings.ToLower(strings.TrimSpace(pyStr(v)))
}

// Float config value with a default when absent
func floatOr(cfg map[string]any, key string, def float64) float64 {
	v, ok := cfg[key]
	if !ok {
		return def
	}
	if f, ok := config.AsFloat(v); ok {
		return f
	}
	return def
}

// Handshakes, discovers, publishes readiness and serves until a lifecycle event, leaving the session unproven
func (s *serverTask) serveSession(ctx context.Context, sess *Session, connectTimeout float64, label string, markLifecycle bool, drop <-chan struct{}) (string, error) {
	res, err := sess.Negotiate(ctx, protocolMode(s.currentConfig()), secondsDuration(connectTimeout))
	if err != nil {
		return "", err
	}
	s.mu.Lock()
	s.initializeResult = res
	s.session = sess
	s.mu.Unlock()
	if opener, ok := sess.t.(interface{ OpenStandaloneStream(context.Context) }); ok {
		opener.OpenStandaloneStream(sess.ctx)
	}
	if markLifecycle {
		s.markLifecycleStarted()
	}
	if err := s.discoverTools(ctx); err != nil {
		return "", err
	}
	s.ready.Set()
	s.mu.Lock()
	s.everConnected = true
	s.sessionProven = false
	s.mu.Unlock()
	resetServerError(ctx, s.name)
	stopEvents := startEventsRefreshTask(ctx, s)
	reason, err := s.waitForLifecycleEvent(ctx, drop)
	if stopEvents != nil {
		stopEvents()
	}
	if err != nil {
		return "", err
	}
	if label != "" && reason == "reconnect" {
		mcpLogger().Info(fmt.Sprintf("MCP server '%s': reconnect requested — tearing down %s session", s.name, label))
	}
	return reason, nil
}

// Opens a remote transport, serves a session over it and maps a live session drop to a reconnect
func (s *serverTask) serveTransport(ctx context.Context, open func() (Transport, error), label string, connectTimeout float64) (string, error) {
	t, err := open()
	if err != nil {
		return "", err
	}
	sess := NewSession(t, s.sessionOptions())
	reason, err := s.serveSession(ctx, sess, connectTimeout, label, false, sess.Done())
	_ = sess.Close()
	if rj, ok := t.(interface{ Rejection() HTTPRejection }); ok {
		s.mu.Lock()
		s.httpRejection = rj.Rejection()
		s.mu.Unlock()
	}
	var drop *sessionDropError
	if errors.As(err, &drop) {
		return s.reconnectOrReraise(ctx, drop)
	}
	return reason, err
}

// Reconnects at once after a live remote session dropped, re-raising during shutdown or before readiness
func (s *serverTask) reconnectOrReraise(ctx context.Context, drop *sessionDropError) (string, error) {
	if s.shutdownEv.IsSet() || ctx.Err() != nil || !s.ready.IsSet() || isProcessExit(drop.err) || firstProcessExitLeaf(drop.err) != nil || carriesCancellation(drop.err) {
		return "", drop.err
	}
	mcpLogger().Debug(fmt.Sprintf("MCP server '%s': transport TaskGroup exited after a live session (%s) — reconnecting immediately instead of backing off", s.name, excStr(drop.err)))
	return "reconnect", nil
}

// Ledgers freshly spawned stdio children with their process groups and start times
func (s *serverTask) trackSpawnedChildren(ctx context.Context, pids []int) {
	pgids := map[int]int{}
	starts := map[int]int64{}
	for _, pid := range pids {
		if runtime.GOOS != "windows" {
			pgids[pid] = processGroup(pid)
		}
		if start, ok := leaderStartTime(pid); ok {
			starts[pid] = start
		}
	}
	mcpMu.Lock()
	for _, pid := range pids {
		stdioPIDs[pid] = s.name
	}
	for pid, pg := range pgids {
		stdioPGIDs[pid] = pg
	}
	for pid, st := range starts {
		stdioStartTimes[pid] = st
	}
	mcpMu.Unlock()
	for _, pid := range pids {
		if !tools.RegisterChild(ctx, pid, "mcp-helper", "") {
			mcpLogger().Debug(fmt.Sprintf("spawn-ledger register_child failed for MCP helper pid %d", pid))
		}
	}
	groups := make([]int, 0, len(pgids))
	for _, pg := range pgids {
		groups = append(groups, pg)
	}
	tools.OwnProcessGroups(groups)
}

// Drops ledger entries, marking a child or group still alive as orphaned for the sweep
func (s *serverTask) releaseSpawnedChildren(pids []int) {
	var released []int
	mcpMu.Lock()
	for _, pid := range pids {
		delete(stdioPIDs, pid)
		pg, hasPG := stdioPGIDs[pid]
		if pidExists(pid) || (hasPG && groupAlive(pg)) {
			orphanStdioPIDs[pid] = true
			orphanStdioPIDServers[pid] = s.name
			continue
		}
		if hasPG {
			delete(stdioPGIDs, pid)
			released = append(released, pg)
		}
		delete(stdioStartTimes, pid)
	}
	mcpMu.Unlock()
	tools.ReleaseProcessGroups(released)
}

// String list of a config args value
func configArgs(v any) []string {
	list, _ := v.([]any)
	out := make([]string, 0, len(list))
	for _, a := range list {
		out = append(out, pyStr(a))
	}
	if ss, ok := v.([]string); ok {
		out = append(out, ss...)
	}
	return out
}

// Runs one stdio transport, surfacing a node ABI mismatch from the stderr tail when the session fails
func (s *serverTask) runStdio(ctx context.Context, r *Rendered) (string, error) {
	cfg := r.Config
	if v, ok := cfg["identity_header"]; ok && v != nil {
		mcpLogger().Warn(fmt.Sprintf("MCP server '%s': identity_header is only supported on HTTP/SSE transports — ignored for stdio servers", s.name))
	}
	if !pyTruthy(cfg["command"]) {
		return "", fmt.Errorf("MCP server '%s' has no 'command' in config", s.name)
	}
	inputs, _, err := connectInputs(ctx, s.env, s.name, r)
	if err != nil {
		return "", err
	}
	digest := identityDigest(inputs)
	s.mu.Lock()
	s.resolvedIdentity = digest
	s.mu.Unlock()
	command, args, err := preflightStdioCommand(ctx, s.name, inputs[0].(string), configArgs(cfg["args"]))
	if err != nil {
		return "", err
	}
	safeEnv, _ := inputs[1].(map[string]string)
	if len(safeEnv) == 0 {
		safeEnv = nil
	}
	if !tools.AttachSelfToKillOnCloseJob() {
		mcpLogger().Debug("job-object self-attach failed before stdio spawn")
	}
	killOrphanedMCPChildren(false, "")
	writeStderrLogHeader(ctx, s.name)
	var logw io.Writer = io.Discard
	if f := getStderrLog(ctx); f != nil {
		logw = f
	}
	tee, err := newStderrTee(logw)
	if err != nil {
		return "", err
	}
	defer tee.Close(0)
	t, err := StartStdio(ctx, StdioParams{Command: command, Args: args, Env: safeEnv, Cwd: inputs[2].(string), Stderr: tee.Sink})
	if err != nil {
		return "", s.withNodeABI(err, tee)
	}
	newPIDs := spawnedMCPChildren(t.PID())
	if len(newPIDs) > 0 {
		s.trackSpawnedChildren(ctx, newPIDs)
		defer s.releaseSpawnedChildren(newPIDs)
	}
	s.mu.Lock()
	s.stdioChildPIDs = append([]int(nil), newPIDs...)
	s.mu.Unlock()
	sess := NewSession(t, s.sessionOptions())
	reason, err := s.serveSession(ctx, sess, floatOr(cfg, "connect_timeout", defaultConnectTimeout), "", true, nil)
	_ = sess.Close()
	if err != nil {
		return "", s.withNodeABI(err, tee)
	}
	return reason, nil
}

// Node ABI mismatch read from the drained stderr tail in place of err, err itself when the tail names none
func (s *serverTask) withNodeABI(err error, tee *stderrTee) error {
	if abi := nodeABIError(s.name, tee.Close(stderrDrainTimeout)); abi != nil {
		return abi
	}
	return err
}

// Probes a Streamable HTTP URL and fails fast when a 2xx answers with a definite non MCP content type
func (s *serverTask) preflightContentType(ctx context.Context, url string, headers map[string]string, sslVerify, clientCert, clientKey any, timeout time.Duration, strict bool) error {
	names := map[string]bool{}
	for k := range headers {
		names[strings.ToLower(k)] = true
	}
	oc, err := newOwnedClient(HTTPOptions{URL: url, ConfiguredHeaderNames: names, StrictRedirectHeaders: strict,
		SSLVerify: sslVerify, ClientCert: clientCert, ClientKey: clientKey, ConnectTimeout: timeout, ReadTimeout: timeout, ServerName: s.name})
	if err != nil {
		return err
	}
	client := *oc.client
	client.Timeout = timeout
	isMCPType := func(ct string) bool { return mcpContentTypes[ct] }
	nonMCP2xx := func(resp *http.Response) bool {
		ct := contentTypeBase(resp)
		return is2xx(resp) && ct != "" && !isMCPType(ct)
	}
	do := func(method string, body string, extra map[string]string) (*http.Response, error) {
		var rd io.Reader
		if body != "" {
			rd = strings.NewReader(body)
		}
		req, err := http.NewRequestWithContext(ctx, method, url, rd)
		if err != nil {
			return nil, err
		}
		for k, v := range headers {
			req.Header.Set(k, v)
		}
		for k, v := range extra {
			req.Header.Set(k, v)
		}
		resp, err := client.Do(req)
		if err != nil {
			return nil, err
		}
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, MaxHTTPBodyBytes))
		resp.Body.Close()
		return resp, nil
	}
	resp, err := do(http.MethodHead, "", nil)
	if err == nil && (resp.StatusCode == 405 || resp.StatusCode == 501) {
		resp, err = do(http.MethodGet, "", nil)
	}
	if err == nil && nonMCP2xx(resp) {
		post, perr := do(http.MethodPost, probeInitializeBody, map[string]string{"Content-Type": "application/json", "Accept": "application/json, text/event-stream"})
		if perr != nil {
			err = perr
		} else if is2xx(post) && isMCPType(contentTypeBase(post)) {
			resp = post
		}
	}
	if err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return nil
	}
	if !nonMCP2xx(resp) {
		return nil
	}
	return newNonMCPEndpointError(fmt.Sprintf("MCP server '%s' at %s returned Content-Type '%s', not an MCP response (expected one of: %s). The URL most likely points at a web page rather than an MCP endpoint — check it resolves to a Streamable HTTP / SSE endpoint (e.g. https://host/mcp, not https://host/).",
		s.name, url, contentTypeBase(resp), "application/json, text/event-stream"))
}

// OAuth provider from the central manager when the server uses OAuth, nil otherwise
func (s *serverTask) buildOAuthAuth(ctx context.Context, url string, cfg map[string]any, transport HTTPOptions) (Authenticator, error) {
	s.mu.Lock()
	oauth := s.authType == "oauth"
	s.mu.Unlock()
	if !oauth {
		return nil, nil
	}
	oauthCfg, _ := cfg["oauth"].(map[string]any)
	p, err := getManager().getOrBuildProvider(ctx, s.name, url, oauthCfg, transport)
	if err != nil {
		mcpLogger().Warn(fmt.Sprintf("MCP OAuth setup failed for '%s': %s", s.name, excStr(err)))
		return nil, err
	}
	return p, nil
}

// Runs one Streamable HTTP or SSE transport with the HTTP to SSE fallback on an initial rejection
func (s *serverTask) runHTTP(ctx context.Context, r *Rendered) (string, error) {
	cfg := r.Config
	inputs, configured, err := connectInputs(ctx, s.env, s.name, r)
	if err != nil {
		return "", err
	}
	digest := identityDigest(inputs)
	url := inputs[0].(string)
	headers := inputs[1].(map[string]string)
	mcpLogger().Debug(fmt.Sprintf("MCP server '%s': connecting to %s", s.name, url))
	s.mu.Lock()
	s.resolvedIdentity = digest
	s.httpRejection = HTTPRejection{}
	s.mu.Unlock()
	hasVersion := false
	for k := range headers {
		if strings.ToLower(k) == "mcp-protocol-version" {
			hasVersion = true
		}
	}
	if !hasVersion {
		headers["mcp-protocol-version"] = LatestHandshakeVersion
	}
	connectTimeout := floatOr(cfg, "connect_timeout", defaultConnectTimeout)
	sslVerify, ok := cfg["ssl_verify"]
	if !ok {
		sslVerify = true
	}
	strict := pyTruthy(cfg["strict_redirect_headers"])
	opts := HTTPOptions{URL: url, Headers: headers, ConfiguredHeaderNames: configured, StrictRedirectHeaders: strict,
		SSLVerify: sslVerify, ClientCert: cfg["client_cert"], ClientKey: cfg["client_key"],
		ConnectTimeout: secondsDuration(connectTimeout), ReadTimeout: 300 * time.Second, ServerName: s.name}
	auth, err := s.buildOAuthAuth(ctx, url, cfg, opts)
	if err != nil {
		return "", err
	}
	opts.Auth = auth
	openSSE := func() (Transport, error) {
		if strict {
			return nil, fmt.Errorf("MCP server '%s': strict_redirect_headers is not supported on the SSE transport.", s.name)
		}
		return StartSSE(ctx, opts, secondsDuration(connectTimeout))
	}
	if pyStr(cfg["transport"]) == "sse" {
		return s.serveTransport(ctx, openSSE, "SSE", connectTimeout)
	}
	s.mu.Lock()
	latched := s.sseFallback
	s.mu.Unlock()
	if latched {
		mcpLogger().Info(fmt.Sprintf("MCP server '%s': using latched SSE fallback transport", s.name))
		return s.serveTransport(ctx, openSSE, "SSE", connectTimeout)
	}
	reason, err := s.serveTransport(ctx, func() (Transport, error) { return StartStreamableHTTP(opts) }, "HTTP", connectTimeout)
	if err == nil {
		return reason, nil
	}
	s.mu.Lock()
	rejection := s.httpRejection
	ever := s.everConnected
	s.mu.Unlock()
	detail := describeHTTPFailure(err, rejection)
	if ever || strict || !isStreamableHTTPRejection(err) {
		if detail != excStr(err) {
			return "", &mcpConnectionError{msg: fmt.Sprintf("MCP server '%s': Streamable HTTP connect failed (%s)", s.name, detail), cause: err}
		}
		return "", err
	}
	mcpLogger().Warn(fmt.Sprintf("MCP server '%s': Streamable HTTP rejected the initial connect (%s) — retrying over SSE. If this connects, set `transport: sse` for this server in config.yaml to skip the failed attempt on future startups.", s.name, detail))
	s.mu.Lock()
	s.sseFallback = true
	s.mu.Unlock()
	reason, sseErr := s.serveTransport(ctx, openSSE, "SSE", connectTimeout)
	if sseErr == nil {
		return reason, nil
	}
	s.mu.Lock()
	ever = s.everConnected
	if !ever {
		s.sseFallback = false
	}
	s.mu.Unlock()
	if ever {
		return "", sseErr
	}
	return "", &mcpConnectionError{msg: fmt.Sprintf("MCP server '%s': both Streamable HTTP and SSE transports failed (Streamable HTTP: %s; SSE: %s). Check the URL points at an MCP endpoint, or pin `transport: sse` if the server is SSE-only.",
		s.name, detail, excStr(sseErr)), cause: sseErr}
}

// Lists the server tools when it advertises them and publishes them when none are registered yet
func (s *serverTask) discoverTools(ctx context.Context) error {
	s.mu.Lock()
	s.pingUnsupported = false
	sess := s.session
	s.mu.Unlock()
	if sess == nil {
		return nil
	}
	if !s.advertisesTools() {
		mcpLogger().Info(fmt.Sprintf("MCP server '%s': does not advertise 'tools' capability — skipping tools/list (prompts/resources remain available)", s.name))
		s.mu.Lock()
		s.tools = nil
		s.mu.Unlock()
	} else {
		if err := s.rpc.Lock(ctx); err != nil {
			return err
		}
		hints := CacheHints{}
		list, err := sess.ListTools(ctx, &hints)
		s.rpc.Unlock()
		if err != nil {
			return err
		}
		s.mu.Lock()
		s.listCacheMeta = hints
		s.tools = list
		s.mu.Unlock()
	}
	return s.registerDiscoveredToolsIfNeeded(ctx)
}

// Publishes discovered tools when none are registered and the task is owned or ready
func (s *serverTask) registerDiscoveredToolsIfNeeded(ctx context.Context) error {
	if len(s.registeredToolNames()) > 0 {
		return nil
	}
	mcpMu.Lock()
	var owned []ServerKey
	for k, live := range mcpServers {
		if live == s {
			owned = append(owned, k)
		}
	}
	mcpMu.Unlock()
	if len(owned) == 0 && !s.ready.IsSet() {
		return nil
	}
	names, err := registerServerTools(ctx, s.name, s, s.currentConfig())
	if err != nil {
		return err
	}
	s.mu.Lock()
	s.toolNames = names
	s.toolNamesSet = true
	s.mu.Unlock()
	mcpMu.Lock()
	for _, k := range owned {
		if mcpServers[k] == s {
			delete(serverConnectErrors, k)
		}
	}
	mcpMu.Unlock()
	return nil
}
