package tools

import (
	"archive/tar"
	"bufio"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net"
	"net/http"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
)

const (
	vercelAPIBase             = "https://api.vercel.com"
	vercelDefaultDiskMB       = 51200
	vercelCreateRetryAttempts = 3
	vercelWriteRetryAttempts  = 3
	vercelRunningWaitTimeout  = 30 * time.Second
	vercelRunningWaitPoll     = 250 * time.Millisecond
	vercelStopTimeout         = 15 * time.Second
	vercelStopPoll            = 500 * time.Millisecond
	vercelMinSandboxTimeout   = 5 * time.Minute
	vercelSnapshotStoreName   = "vercel_sandbox_snapshots.json"
)

var vercelTransientStatuses = map[int]bool{408: true, 425: true, 429: true, 500: true, 502: true, 503: true, 504: true}

// VercelAuthStatus describes the Vercel auth configuration without exposing secret values
type VercelAuthStatus struct {
	OK          bool
	Label       string
	DetailLines []string
}

var vercelTokenTupleVars = []string{"VERCEL_TOKEN", "VERCEL_PROJECT_ID", "VERCEL_TEAM_ID"}

// DescribeVercelAuth reports the auth mode from the secret scope, OIDC first then the token tuple
func DescribeVercelAuth(env *Env) VercelAuthStatus {
	var present, missing []string
	for _, name := range vercelTokenTupleVars {
		if secretValue(env, name) != "" {
			present = append(present, name)
		} else {
			missing = append(missing, name)
		}
	}
	if secretValue(env, "VERCEL_OIDC_TOKEN") != "" {
		details := []string{"mode: OIDC", "active env: VERCEL_OIDC_TOKEN",
			"note: OIDC tokens are development-only; use access-token auth for deployments and long-running processes"}
		if len(present) > 0 {
			details = append(details, "also present: "+strings.Join(present, ", "))
		}
		return VercelAuthStatus{true, "OIDC token via VERCEL_OIDC_TOKEN", details}
	}
	if len(missing) == 0 {
		return VercelAuthStatus{true, "access token + project/team via VERCEL_TOKEN, VERCEL_PROJECT_ID, VERCEL_TEAM_ID",
			[]string{"mode: access token", "active env: VERCEL_TOKEN, VERCEL_PROJECT_ID, VERCEL_TEAM_ID"}}
	}
	if len(present) > 0 {
		return VercelAuthStatus{false, fmt.Sprintf("partial access-token auth (missing %s)", strings.Join(missing, ", ")),
			[]string{"mode: incomplete access token", "present env: " + strings.Join(present, ", "), "missing env: " + strings.Join(missing, ", "),
				"recommended: set VERCEL_TOKEN, VERCEL_PROJECT_ID, and VERCEL_TEAM_ID together"}}
	}
	return VercelAuthStatus{false, "not configured", []string{"recommended: set VERCEL_TOKEN, VERCEL_PROJECT_ID, and VERCEL_TEAM_ID",
		"development-only alternative: set VERCEL_OIDC_TOKEN"}}
}

// vercelCredentials are the token and the team and project it acts for
type vercelCredentials struct {
	token, teamID, projectID string
}

// Credentials from the OIDC token claims, else the full token tuple
func resolveVercelCredentials(env *Env) (vercelCredentials, error) {
	if oidc := secretValue(env, "VERCEL_OIDC_TOKEN"); oidc != "" {
		parts := strings.Split(oidc, ".")
		if len(parts) < 2 {
			return vercelCredentials{}, errors.New("Invalid Vercel OIDC token: not a JWT")
		}
		raw, err := base64.RawURLEncoding.DecodeString(strings.TrimRight(parts[1], "="))
		if err != nil {
			return vercelCredentials{}, fmt.Errorf("Invalid Vercel OIDC token: %v", err)
		}
		var claims struct {
			ProjectID string `json:"project_id"`
			OwnerID   string `json:"owner_id"`
		}
		if err := json.Unmarshal(raw, &claims); err != nil || claims.ProjectID == "" || claims.OwnerID == "" {
			return vercelCredentials{}, errors.New("Invalid Vercel OIDC token: missing project_id or owner_id claims")
		}
		return vercelCredentials{token: oidc, teamID: claims.OwnerID, projectID: claims.ProjectID}, nil
	}
	creds := vercelCredentials{token: secretValue(env, "VERCEL_TOKEN"), projectID: secretValue(env, "VERCEL_PROJECT_ID"), teamID: secretValue(env, "VERCEL_TEAM_ID")}
	if creds.token == "" || creds.projectID == "" || creds.teamID == "" {
		return vercelCredentials{}, errors.New("Vercel Sandbox auth is not configured: set VERCEL_TOKEN, VERCEL_PROJECT_ID, and VERCEL_TEAM_ID together, or VERCEL_OIDC_TOKEN for local development")
	}
	return creds, nil
}

// vercelAPIError is a non success HTTP reply
type vercelAPIError struct {
	Status int
	Body   string
	Code   string
}

func (e *vercelAPIError) Error() string {
	if e.Code != "" {
		return fmt.Sprintf("Vercel API HTTP %d (%s): %s", e.Status, e.Code, e.Body)
	}
	return fmt.Sprintf("Vercel API HTTP %d: %s", e.Status, e.Body)
}

// Reports whether the error looks retryable, a transient status or a network failure
func isTransientVercelError(err error) bool {
	var apiErr *vercelAPIError
	if errors.As(err, &apiErr) {
		return vercelTransientStatuses[apiErr.Status]
	}
	var netErr net.Error
	if errors.As(err, &netErr) {
		return true
	}
	return errors.Is(err, io.ErrUnexpectedEOF) || errors.Is(err, io.EOF)
}

// Retries a transient failure up to attempts with a growing pause
func retryVercelCall[T any](env *Env, label string, attempts int, call func() (T, error)) (T, error) {
	var zero T
	for attempt := 1; ; attempt++ {
		out, err := call()
		if err == nil {
			return out, nil
		}
		if attempt >= attempts || !isTransientVercelError(err) {
			return zero, err
		}
		envLog(env).Warn(fmt.Sprintf("Vercel: %s failed; retrying", label), "error", err, "attempt", attempt, "attempts", attempts)
		time.Sleep(time.Duration(100*attempt) * time.Millisecond)
	}
}

// vercelClient talks to the sandbox REST API for one team
type vercelClient struct {
	creds vercelCredentials
	http  *http.Client
}

// Test seam replacing the API origin
var vercelAPIBaseOverride = ""

// Request with the bearer token and team query, the response body returned for non success statuses as an error
func (c *vercelClient) do(ctx context.Context, method, p string, body []byte, contentType string, headers map[string]string, timeout time.Duration) (*http.Response, error) {
	origin := vercelAPIBase
	if vercelAPIBaseOverride != "" {
		origin = vercelAPIBaseOverride
	}
	u, err := url.Parse(origin + p)
	if err != nil {
		return nil, err
	}
	q := u.Query()
	q.Set("teamId", c.creds.teamID)
	u.RawQuery = q.Encode()
	reqCtx := ctx
	var cancel context.CancelFunc
	if timeout > 0 {
		reqCtx, cancel = context.WithTimeout(ctx, timeout)
	}
	var reader io.Reader
	if body != nil {
		reader = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(reqCtx, method, u.String(), reader)
	if err != nil {
		if cancel != nil {
			cancel()
		}
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+c.creds.token)
	req.Header.Set("Content-Type", contentType)
	req.Header.Set("User-Agent", icebathUserAgent())
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		if cancel != nil {
			cancel()
		}
		return nil, err
	}
	if cancel != nil {
		resp.Body = &cancelOnClose{ReadCloser: resp.Body, cancel: cancel}
	}
	if resp.StatusCode >= 400 {
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<16))
		resp.Body.Close()
		apiErr := &vercelAPIError{Status: resp.StatusCode, Body: strings.TrimSpace(string(raw))}
		var parsed struct {
			Error struct {
				Code    string `json:"code"`
				Message string `json:"message"`
			} `json:"error"`
		}
		if json.Unmarshal(raw, &parsed) == nil {
			apiErr.Code = parsed.Error.Code
			if parsed.Error.Message != "" {
				apiErr.Body = parsed.Error.Message
			}
		}
		return nil, apiErr
	}
	return resp, nil
}

// cancelOnClose releases the request context when the body closes
type cancelOnClose struct {
	io.ReadCloser
	cancel context.CancelFunc
}

func (c *cancelOnClose) Close() error {
	err := c.ReadCloser.Close()
	c.cancel()
	return err
}

// JSON request returning the decoded object
func (c *vercelClient) json(ctx context.Context, method, p string, payload any, timeout time.Duration) (map[string]any, error) {
	var body []byte
	if payload != nil {
		raw, err := json.Marshal(payload)
		if err != nil {
			return nil, err
		}
		body = raw
	}
	resp, err := c.do(ctx, method, p, body, "application/json", nil, timeout)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	out := map[string]any{}
	if len(bytes.TrimSpace(raw)) > 0 {
		if err := json.Unmarshal(raw, &out); err != nil {
			return nil, fmt.Errorf("Vercel API returned non JSON: %s", strings.TrimSpace(string(raw)))
		}
	}
	return out, nil
}

// vercelSession is the running VM of a sandbox
type vercelSession struct {
	id, cwd, status string
}

// Session fields of a create or get reply
func vercelSessionFrom(reply map[string]any) vercelSession {
	session := asMapOrEmpty(reply["session"])
	id, _ := session["id"].(string)
	cwd, _ := session["cwd"].(string)
	status, _ := session["status"].(string)
	return vercelSession{id, cwd, status}
}

// vercelCreateParams are the sandbox creation settings
type vercelCreateParams struct {
	timeout         time.Duration
	runtime, image  string
	vcpus, memoryMB int
}

// Creates a session, from a snapshot when snapshotID is given, runtime pinning the v2 endpoint
func (c *vercelClient) createSession(ctx context.Context, p vercelCreateParams, snapshotID string) (vercelSession, error) {
	payload := map[string]any{"projectId": c.creds.projectID, "timeout": int(p.timeout / time.Millisecond)}
	if p.vcpus > 0 || p.memoryMB > 0 {
		resources := map[string]any{}
		if p.vcpus > 0 {
			resources["vcpus"] = p.vcpus
		}
		if p.memoryMB > 0 {
			resources["memory"] = p.memoryMB
		}
		payload["resources"] = resources
	}
	endpoint := "/v3/sandboxes"
	if snapshotID != "" {
		payload["source"] = map[string]any{"type": "snapshot", "snapshotId": snapshotID}
	} else if p.runtime != "" {
		payload["runtime"] = p.runtime
		endpoint = "/v2/sandboxes"
	} else if p.image != "" {
		payload["image"] = p.image
	}
	reply, err := c.json(ctx, "POST", endpoint, payload, 120*time.Second)
	if err != nil {
		return vercelSession{}, err
	}
	session := vercelSessionFrom(reply)
	if session.id == "" {
		return vercelSession{}, errors.New("Vercel sandbox create did not return a session id")
	}
	return session, nil
}

func (c *vercelClient) getSession(ctx context.Context, id string) (vercelSession, error) {
	reply, err := c.json(ctx, "GET", "/v2/sandboxes/sessions/"+id, nil, 30*time.Second)
	if err != nil {
		return vercelSession{}, err
	}
	return vercelSessionFrom(reply), nil
}

func (c *vercelClient) stopSession(ctx context.Context, id string) error {
	_, err := c.json(ctx, "POST", "/v2/sandboxes/sessions/"+id+"/stop", nil, 60*time.Second)
	return err
}

// Snapshot id of the running session's filesystem
func (c *vercelClient) snapshotSession(ctx context.Context, id string) (string, error) {
	reply, err := c.json(ctx, "POST", "/v2/sandboxes/sessions/"+id+"/snapshot", nil, 120*time.Second)
	if err != nil {
		return "", err
	}
	snapshot := asMapOrEmpty(reply["snapshot"])
	for _, key := range []string{"snapshot_id", "snapshotId", "id"} {
		if s, ok := snapshot[key].(string); ok && s != "" {
			return s, nil
		}
	}
	for _, key := range []string{"snapshot_id", "snapshotId", "id"} {
		if s, ok := reply[key].(string); ok && s != "" {
			return s, nil
		}
	}
	return "", nil
}

// vercelFile is one file to write into the sandbox
type vercelFile struct {
	path    string
	content []byte
	mode    int64
}

// Writes files as one gzipped tar extracted at the root, paths made relative to it
func (c *vercelClient) writeFiles(ctx context.Context, sessionID string, files []vercelFile) error {
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	for _, f := range files {
		name := strings.TrimLeft(path.Clean(f.path), "/")
		mode := f.mode
		if mode == 0 {
			mode = 0o644
		}
		if err := tw.WriteHeader(&tar.Header{Name: name, Mode: mode, Size: int64(len(f.content)), ModTime: time.Now(), Typeflag: tar.TypeReg}); err != nil {
			return err
		}
		if _, err := tw.Write(f.content); err != nil {
			return err
		}
	}
	if err := tw.Close(); err != nil {
		return err
	}
	if err := gz.Close(); err != nil {
		return err
	}
	resp, err := c.do(ctx, "POST", "/v2/sandboxes/sessions/"+sessionID+"/fs/write", buf.Bytes(), "application/gzip", map[string]string{"x-cwd": "/"}, 120*time.Second)
	if err != nil {
		return err
	}
	resp.Body.Close()
	return nil
}

// Reads one file's bytes
func (c *vercelClient) readFile(ctx context.Context, sessionID, p string) ([]byte, error) {
	raw, _ := json.Marshal(map[string]any{"path": p})
	resp, err := c.do(ctx, "POST", "/v2/sandboxes/sessions/"+sessionID+"/fs/read", raw, "application/json", nil, 120*time.Second)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	return io.ReadAll(resp.Body)
}

// vercelCommandResult is the combined output and exit code of a finished command
type vercelCommandResult struct {
	output   string
	exitCode int
}

// Runs a command waiting for completion over the ND-JSON stream, output being the log lines in arrival order
func (c *vercelClient) runCommand(ctx context.Context, sessionID, command string, args []string, cwd string) (vercelCommandResult, error) {
	payload, _ := json.Marshal(map[string]any{"command": command, "args": args, "cwd": cwd, "env": map[string]any{}, "wait": true, "logs": true})
	resp, err := c.do(ctx, "POST", "/v2/sandboxes/sessions/"+sessionID+"/cmd", payload, "application/json", nil, 0)
	if err != nil {
		return vercelCommandResult{}, err
	}
	defer resp.Body.Close()
	if !strings.HasPrefix(resp.Header.Get("Content-Type"), "application/x-ndjson") {
		return vercelCommandResult{}, errors.New("Expected a stream of command data")
	}
	scanner := bufio.NewScanner(resp.Body)
	scanner.Buffer(make([]byte, 1024*1024), 64*1024*1024)
	var output strings.Builder
	cmdID := ""
	started := false
	for scanner.Scan() {
		line := bytes.TrimSpace(scanner.Bytes())
		if len(line) == 0 {
			continue
		}
		var chunk map[string]any
		if err := json.Unmarshal(line, &chunk); err != nil {
			continue
		}
		if cmd, ok := chunk["command"].(map[string]any); ok {
			if !started {
				started = true
				cmdID, _ = cmd["id"].(string)
				continue
			}
			if code, ok := pyIntValue(cmd["exitCode"]); ok {
				return vercelCommandResult{output.String(), code}, nil
			}
			continue
		}
		stream, _ := chunk["stream"].(string)
		if stream == "error" {
			data := asMapOrEmpty(chunk["data"])
			msg, _ := data["message"].(string)
			return vercelCommandResult{}, fmt.Errorf("Vercel sandbox stream error: %s", msg)
		}
		if data, ok := chunk["data"].(string); ok {
			output.WriteString(data)
		}
	}
	if !started || cmdID == "" {
		return vercelCommandResult{}, errors.New("Stream ended before command data was received")
	}
	reply, err := c.json(ctx, "GET", "/v2/sandboxes/sessions/"+sessionID+"/cmd/"+cmdID+"?wait=true", nil, 0)
	if err != nil {
		return vercelCommandResult{}, err
	}
	code := 1
	if n, ok := pyIntValue(asMapOrEmpty(reply["command"])["exitCode"]); ok {
		code = n
	}
	return vercelCommandResult{output.String(), code}, nil
}

// Per profile snapshot store keyed by task id
func vercelSnapshotStore(env *Env) string { return filepath.Join(env.Home, vercelSnapshotStoreName) }

func vercelStoreSnapshot(ctx context.Context, env *Env, taskID, snapshotID string) {
	if taskID != "" && snapshotID != "" {
		envUpdateJSONStore(WithEnvHome(ctx, env), vercelSnapshotStore(env), func(s *pyjson.Object) { s.Set(taskID, snapshotID) })
	}
}

// Drops the stored id for the task only while it still equals the snapshot id
func vercelDeleteSnapshot(ctx context.Context, env *Env, taskID, snapshotID string) {
	envUpdateJSONStore(WithEnvHome(ctx, env), vercelSnapshotStore(env), func(s *pyjson.Object) {
		if taskID != "" && s.Vals[taskID] == snapshotID {
			s.Delete(taskID)
		}
	})
}

// Reports whether a session status is final
func vercelStatusTerminal(status string) bool {
	return status == "aborted" || status == "failed" || status == "stopped"
}

// vercelEnv runs commands in a Vercel cloud sandbox through the shared shell contract
type vercelEnv struct {
	baseEnv
	client        *vercelClient
	session       *vercelSession
	persistent    bool
	taskID        string
	requestedCwd  string
	workspaceRoot string
	remoteHome    string
	createParams  vercelCreateParams
	lock          sync.Mutex
	syncManager   *FileSyncManager
}

// VercelOptions are the sandbox settings, memory and disk in MB
type VercelOptions struct {
	Runtime, Image, Cwd string
	Timeout, Memory     int
	CPU                 float64
	Disk                int
	Persistent          bool
	TaskID              string
}

// NewVercelSandboxEnvironment creates or restores the sandbox, waits for it to run, wires the sync and starts the session
func NewVercelSandboxEnvironment(ctx context.Context, env *Env, o VercelOptions) (*vercelEnv, error) {
	if o.Cwd == "" {
		o.Cwd = VercelSandboxDefaultCwd
	}
	if o.Timeout == 0 {
		o.Timeout = 60
	}
	if o.TaskID == "" {
		o.TaskID = "default"
	}
	if o.Disk != 0 && o.Disk != vercelDefaultDiskMB {
		return nil, errors.New("Vercel Sandbox does not support configurable container_disk. Use the default shared setting.")
	}
	creds, err := resolveVercelCredentials(env)
	if err != nil {
		return nil, err
	}
	v := &vercelEnv{client: &vercelClient{creds: creds, http: webHTTPClient(0)}, persistent: o.Persistent, taskID: o.TaskID, requestedCwd: o.Cwd,
		workspaceRoot: VercelSandboxDefaultCwd, remoteHome: VercelSandboxDefaultCwd}
	v.stdinMode = "payload"
	initBaseEnv(&v.baseEnv, v, env, o.Cwd, o.Timeout, nil, true)
	if o.Runtime != "" {
		envLog(env).Warn(fmt.Sprintf("Vercel: terminal.vercel_runtime is deprecated by Vercel; set terminal.vercel_image (default %s) instead", defaultVercelImage))
	}
	timeout := time.Duration(max(o.Timeout, 0)) * time.Second
	if timeout < vercelMinSandboxTimeout {
		timeout = vercelMinSandboxTimeout
	}
	image := o.Image
	if image == "" {
		image = defaultVercelImage
	}
	if o.Runtime != "" {
		image = ""
	}
	v.createParams = vercelCreateParams{timeout: timeout, runtime: o.Runtime, image: image}
	if o.CPU > 0 {
		v.createParams.vcpus = int(math.Floor(o.CPU))
	}
	if o.Memory > 0 {
		v.createParams.memoryMB = o.Memory
	}
	if err := v.attachFreshSandbox(ctx, o.Cwd); err != nil {
		return nil, err
	}
	v.syncManager.Sync(true)
	v.initSession(ctx)
	return v, nil
}

func (v *vercelEnv) EnvType() string { return "vercel_sandbox" }

// Home directory of the sandbox user that remote media and image delivery resolve tilde paths against
func (v *vercelEnv) RemoteHome() (string, bool) { return v.remoteHome, v.remoteHome != "" }

func (v *vercelEnv) remoteIcebathDir() string {
	return strings.TrimRight(v.remoteHome, "/") + "/.icebath"
}

// Creates a session, restoring the stored snapshot first and falling back to a fresh sandbox when it fails
func (v *vercelEnv) createSandbox(ctx context.Context) (vercelSession, error) {
	snapshotID := ""
	if v.persistent && v.taskID != "" {
		snapshotID, _ = envLoadJSONStore(vercelSnapshotStore(v.env)).Vals[v.taskID].(string)
	}
	if snapshotID != "" {
		session, err := retryVercelCall(v.env, "sandbox restore", vercelCreateRetryAttempts, func() (vercelSession, error) {
			return v.client.createSession(ctx, v.createParams, snapshotID)
		})
		if err == nil {
			return session, nil
		}
		envLog(v.env).Warn("Vercel: failed to restore snapshot; falling back to a fresh sandbox", "snapshot", snapshotID, "task", v.taskID, "error", err)
		vercelDeleteSnapshot(ctx, v.env, v.taskID, snapshotID)
	}
	return retryVercelCall(v.env, "sandbox create", vercelCreateRetryAttempts, func() (vercelSession, error) {
		return v.client.createSession(ctx, v.createParams, "")
	})
}

// Creates a sandbox, waits until it runs, then wires cwd, home and the sync manager
func (v *vercelEnv) attachFreshSandbox(ctx context.Context, requestedCwd string) error {
	session, err := v.createSandbox(ctx)
	if err != nil {
		return err
	}
	v.session = &session
	if err := v.waitForRunning(ctx); err != nil {
		return err
	}
	if strings.HasPrefix(session.cwd, "/") {
		v.workspaceRoot = session.cwd
	} else {
		v.workspaceRoot = VercelSandboxDefaultCwd
	}
	v.remoteHome = v.detectRemoteHome(ctx)
	base := v.remoteIcebathDir()
	v.syncManager = NewFileSyncManager(v.env, func() []syncFile { return iterSyncFiles(v.env, base) },
		func(h, r string) error { return v.vercelBulkUpload(ctx, []syncFile{{h, r}}) }, func(paths []string) error { return v.vercelDelete(ctx, paths) },
		func(files []syncFile) error { return v.vercelBulkUpload(ctx, files) }, func(dest string) error { return v.vercelBulkDownload(ctx, dest) })
	switch requestedCwd {
	case "~":
		v.SetCwd(v.remoteHome)
	case "", VercelSandboxDefaultCwd:
		v.SetCwd(v.workspaceRoot)
	default:
		v.SetCwd(requestedCwd)
	}
	return nil
}

// Remote home from the login shell, the workspace root when detection fails
func (v *vercelEnv) detectRemoteHome(ctx context.Context) string {
	result, err := v.client.runCommand(ctx, v.session.id, "sh", []string{"-lc", `printf %s "$HOME"`}, v.workspaceRoot)
	if err != nil {
		envLog(v.env).Debug("Vercel: home detection failed", "task", v.taskID, "error", err)
		return v.workspaceRoot
	}
	home := strings.TrimSpace(result.output)
	if strings.HasPrefix(home, "/") {
		return home
	}
	return v.workspaceRoot
}

// Polls until the session runs, failing on a terminal status or the wait budget
func (v *vercelEnv) waitForRunning(ctx context.Context) error {
	if v.session.status == "" || v.session.status == "running" {
		return nil
	}
	if vercelStatusTerminal(v.session.status) {
		return fmt.Errorf("Sandbox entered terminal state: %s", v.session.status)
	}
	deadline := time.Now().Add(vercelRunningWaitTimeout)
	for time.Now().Before(deadline) {
		time.Sleep(vercelRunningWaitPoll)
		session, err := v.client.getSession(ctx, v.session.id)
		if err != nil {
			return err
		}
		v.session.status = session.status
		if session.status == "running" {
			return nil
		}
		if vercelStatusTerminal(session.status) {
			return fmt.Errorf("Sandbox entered terminal state: %s", session.status)
		}
	}
	return fmt.Errorf("Sandbox did not reach running state (last status: %s)", v.session.status)
}

// Stops a session and waits for it to report stopped
func (v *vercelEnv) stopSandbox(session *vercelSession) {
	if session == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	if err := v.client.stopSession(ctx, session.id); err != nil {
		return
	}
	deadline := time.Now().Add(vercelStopTimeout)
	for time.Now().Before(deadline) {
		current, err := v.client.getSession(ctx, session.id)
		if err != nil || vercelStatusTerminal(current.status) {
			return
		}
		time.Sleep(vercelStopPoll)
	}
}

// Snapshots the sandbox filesystem for the task when persistent
func (v *vercelEnv) snapshotSandbox(session *vercelSession) {
	if !v.persistent || v.taskID == "" || session == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 180*time.Second)
	defer cancel()
	snapshotID, err := v.client.snapshotSession(ctx, session.id)
	if err != nil {
		envLog(v.env).Warn("Vercel: filesystem snapshot failed", "task", v.taskID, "error", err)
		return
	}
	if snapshotID == "" {
		envLog(v.env).Warn("Vercel: filesystem snapshot did not return a snapshot id", "task", v.taskID)
		return
	}
	vercelStoreSnapshot(ctx, v.env, v.taskID, snapshotID)
	envLog(v.env).Info("Vercel: saved filesystem snapshot", "snapshot", snapshotID, "task", v.taskID)
}

// Reuses a healthy sandbox, recreating it when the refresh fails or it reached a terminal state
func (v *vercelEnv) ensureSandboxReady(ctx context.Context) error {
	requestedCwd := v.Cwd()
	if requestedCwd == "" {
		requestedCwd = v.requestedCwd
	}
	if requestedCwd == "" {
		requestedCwd = VercelSandboxDefaultCwd
	}
	if v.session != nil {
		current, err := v.client.getSession(ctx, v.session.id)
		if err != nil {
			envLog(v.env).Warn("Vercel: sandbox refresh failed; recreating", "task", v.taskID, "error", err)
		} else {
			v.session.status = current.status
			if !vercelStatusTerminal(current.status) {
				return v.waitForRunning(ctx)
			}
			envLog(v.env).Warn("Vercel: sandbox entered a terminal state; recreating", "status", current.status, "task", v.taskID)
		}
		v.MarkRecreated()
	}
	return v.attachFreshSandbox(ctx, requestedCwd)
}

// Runs a login shell script and fails with its output on a non zero exit
func (v *vercelEnv) runChecked(ctx context.Context, script, label string) error {
	result, err := v.client.runCommand(ctx, v.session.id, "bash", []string{"-lc", script}, v.workspaceRoot)
	if err != nil {
		return err
	}
	if result.exitCode != 0 {
		return fmt.Errorf("Vercel %s failed: %s", label, strings.TrimSpace(result.output))
	}
	return nil
}

func (v *vercelEnv) vercelBulkUpload(ctx context.Context, files []syncFile) error {
	if len(files) == 0 {
		return nil
	}
	payload := make([]vercelFile, 0, len(files))
	for _, f := range files {
		data, err := os.ReadFile(f.host)
		if err != nil {
			return err
		}
		payload = append(payload, vercelFile{path: f.remote, content: data})
	}
	_, err := retryVercelCall(v.env, "write_files", vercelWriteRetryAttempts, func() (struct{}, error) {
		return struct{}{}, v.client.writeFiles(ctx, v.session.id, payload)
	})
	return err
}

func (v *vercelEnv) vercelDelete(ctx context.Context, remotePaths []string) error {
	if len(remotePaths) == 0 {
		return nil
	}
	return v.runChecked(ctx, quotedRmCommand(remotePaths), "delete")
}

// Downloads the remote icebath tree as a tar through a pid suffixed remote temp path
func (v *vercelEnv) vercelBulkDownload(ctx context.Context, dest string) error {
	member := strings.TrimLeft(v.remoteIcebathDir(), "/")
	remoteTar := fmt.Sprintf("/tmp/.icebath_sync.%d.tar", os.Getpid())
	defer v.client.runCommand(ctx, v.session.id, "bash", []string{"-lc", "rm -f " + shQuote(remoteTar)}, v.workspaceRoot)
	if err := v.runChecked(ctx, "tar cf "+shQuote(remoteTar)+" --exclude='*.sock' -C / "+shQuote(member), "bulk download"); err != nil {
		return err
	}
	data, err := v.client.readFile(ctx, v.session.id, remoteTar)
	if err != nil {
		return err
	}
	return os.WriteFile(dest, data, 0o600)
}

func (v *vercelEnv) beforeExecute(ctx context.Context) error {
	v.lock.Lock()
	defer v.lock.Unlock()
	if err := v.ensureSandboxReady(ctx); err != nil {
		return err
	}
	if v.syncManager != nil {
		v.syncManager.Sync(false)
	}
	return nil
}

// Spawn per call through a threaded handle, payload stdin staged as a sandbox file so it never becomes argv
func (v *vercelEnv) runBash(ctx context.Context, cmd string, login bool, timeout int, stdinData *string) (ProcessHandle, error) {
	if v.session == nil {
		return nil, errors.New("Vercel sandbox is not attached")
	}
	session, workspaceRoot := *v.session, v.workspaceRoot
	var stateMu sync.Mutex
	cancelled, dispatched := false, false
	staged := ""
	scrubStaged := func() {
		if staged != "" && !dispatched {
			v.client.writeFiles(context.Background(), session.id, []vercelFile{{path: staged, content: nil, mode: 0o600}})
			staged = ""
		}
	}
	cancel := func() {
		stateMu.Lock()
		cancelled = true
		scrubStaged()
		stateMu.Unlock()
		v.stopSandbox(&session)
	}
	execFn := func() (string, int) {
		command := cmd
		if stdinData != nil && *stdinData != "" {
			stateMu.Lock()
			if cancelled {
				stateMu.Unlock()
				return "", 130
			}
			stateMu.Unlock()
			remoteStdin := v.stagedStdinPath()
			payload, err := pystr.SurrogateEscapeEncode(*stdinData)
			if err != nil {
				return "", 1
			}
			if _, err := retryVercelCall(v.env, "stdin upload", vercelWriteRetryAttempts, func() (struct{}, error) {
				return struct{}{}, v.client.writeFiles(context.Background(), session.id, []vercelFile{{path: remoteStdin, content: payload, mode: 0o600}})
			}); err != nil {
				return "Vercel stdin upload failed: " + err.Error(), 1
			}
			command = redirectStdinFromFile(cmd, remoteStdin)
			stateMu.Lock()
			staged = remoteStdin
			stateMu.Unlock()
		}
		stateMu.Lock()
		if cancelled {
			scrubStaged()
			stateMu.Unlock()
			return "", 130
		}
		dispatched = true
		stateMu.Unlock()
		flag := "-c"
		if login {
			flag = "-lc"
		}
		result, err := v.client.runCommand(context.Background(), session.id, "bash", []string{flag, command}, workspaceRoot)
		if err != nil {
			return "Vercel exec failed: " + err.Error(), 1
		}
		return result.output, result.exitCode
	}
	return NewThreadedProcessHandle(execFn, cancel)
}

// Cleanup syncs back, snapshots when persistent, and always stops the sandbox
func (v *vercelEnv) Cleanup(forceRemove bool) error {
	v.lock.Lock()
	session, syncManager := v.session, v.syncManager
	if session != nil && syncManager != nil {
		syncManager.SyncBack(v.env.Home)
	}
	v.session, v.syncManager = nil, nil
	v.lock.Unlock()
	if session == nil {
		return nil
	}
	v.snapshotSandbox(session)
	v.stopSandbox(session)
	return nil
}

// ---- provider -----------------------------------------------------------------------------------

type vercelSandboxBackend struct{}

func (vercelSandboxBackend) Name() string        { return "vercel_sandbox" }
func (vercelSandboxBackend) DisplayName() string { return "Vercel Sandbox" }
func (vercelSandboxBackend) Description() string {
	return "Cloud microVM with snapshot filesystem persistence"
}
func (vercelSandboxBackend) EnvDescription() string                 { return "a Vercel sandbox (Linux)" }
func (vercelSandboxBackend) IsRemote() bool                         { return true }
func (vercelSandboxBackend) IsContainer() bool                      { return true }
func (vercelSandboxBackend) SkipContainerGuards() bool              { return true }
func (vercelSandboxBackend) CachePathBase() string                  { return "~/.icebath" }
func (vercelSandboxBackend) DefaultCwd() string                     { return VercelSandboxDefaultCwd }
func (vercelSandboxBackend) StripEnvKeys() []string                 { return nil }
func (vercelSandboxBackend) SessionIsolatedWhenNonpersistent() bool { return false }
func (vercelSandboxBackend) SupportsStreams() bool                  { return false }

// IsAvailable reports whether the process environment holds a usable auth configuration
func (vercelSandboxBackend) IsAvailable() bool { return DescribeVercelAuth(nil).OK }

// CheckRequirements checks the runtime, the disk setting, then the auth tuple or OIDC token
func (vercelSandboxBackend) CheckRequirements(env *Env, cfg map[string]any) error {
	runtime := strings.TrimSpace(argS(cfg, "vercel_runtime"))
	if !IsSupportedVercelRuntime(runtime) {
		return fmt.Errorf("Vercel Sandbox runtime %s is not supported. Set TERMINAL_VERCEL_RUNTIME to one of: %s.", pyRepr(runtime), strings.Join(SupportedVercelRuntimes, ", "))
	}
	disk := vercelDefaultDiskMB
	if v, ok := cfg["container_disk"]; ok && v != nil {
		if n, ok := pyIntValue(v); ok {
			disk = n
		} else {
			disk = -1
		}
	}
	if disk != 0 && disk != vercelDefaultDiskMB {
		return fmt.Errorf("Vercel Sandbox does not support custom TERMINAL_CONTAINER_DISK=%v. Use the default shared setting (51200 MB).", cfg["container_disk"])
	}
	if secretValue(env, "VERCEL_OIDC_TOKEN") != "" {
		return nil
	}
	present := 0
	for _, name := range vercelTokenTupleVars {
		if secretValue(env, name) != "" {
			present++
		}
	}
	if present == len(vercelTokenTupleVars) {
		return nil
	}
	head := "selected but no supported auth configuration was found. Set VERCEL_TOKEN, VERCEL_PROJECT_ID, and VERCEL_TEAM_ID for normal use."
	if present > 0 {
		head = "selected with token auth, but VERCEL_TOKEN, VERCEL_PROJECT_ID, and VERCEL_TEAM_ID must all be set together."
	}
	return fmt.Errorf("Vercel Sandbox backend %s VERCEL_OIDC_TOKEN is supported for one-off local development only.", head)
}

// Probe reports the auth status for the picker
func (vercelSandboxBackend) Probe() (string, string) {
	status := DescribeVercelAuth(nil)
	if status.OK {
		return "ready", status.Label
	}
	return "needs_setup", "Vercel auth " + status.Label
}

// SetupInstructions explains the image, persistence and auth settings
func (vercelSandboxBackend) SetupInstructions(env *Env) []string {
	return []string{
		"Cloud microVM sandboxes with snapshot-backed filesystem persistence.",
		"Set terminal.vercel_image to a Vercel managed image or a VCR repository[:tag] (default " + defaultVercelImage + ").",
		"Snapshots restore files only; live processes do not continue after sandbox recreation.",
		"Set VERCEL_TOKEN, VERCEL_PROJECT_ID, and VERCEL_TEAM_ID together, or VERCEL_OIDC_TOKEN for one-off local development.",
	}
}

// DoctorChecks reports the runtime, disk, auth and persistence rows
func (vercelSandboxBackend) DoctorChecks() []DoctorCheck {
	runtime := strings.TrimSpace(os.Getenv("TERMINAL_VERCEL_RUNTIME"))
	if runtime == "" {
		runtime = "node24"
	}
	supported := strings.Join(SupportedVercelRuntimes, ", ")
	var rows []DoctorCheck
	if containsString(SupportedVercelRuntimes, runtime) {
		rows = append(rows, DoctorCheck{OK: true, Label: "Vercel runtime", Detail: "(" + runtime + ")"})
	} else {
		rows = append(rows, DoctorCheck{OK: false, Label: "Vercel runtime unsupported", Detail: "(" + runtime + "; use " + supported + ")"})
	}
	disk := strings.TrimSpace(os.Getenv("TERMINAL_CONTAINER_DISK"))
	if disk == "" || disk == "0" || disk == "51200" {
		rows = append(rows, DoctorCheck{OK: true, Label: "Vercel disk setting", Detail: "(uses platform default)"})
	} else {
		rows = append(rows, DoctorCheck{OK: false, Label: "Vercel custom disk unsupported", Detail: "(reset terminal.container_disk to 51200)"})
	}
	auth := DescribeVercelAuth(nil)
	switch {
	case auth.OK:
		rows = append(rows, DoctorCheck{OK: true, Label: "Vercel auth", Detail: "(" + auth.Label + ")"})
	case strings.HasPrefix(auth.Label, "partial"):
		rows = append(rows, DoctorCheck{OK: false, Label: "Vercel auth incomplete", Detail: "(" + auth.Label + ")"})
	default:
		rows = append(rows, DoctorCheck{OK: false, Label: "Vercel auth not configured", Detail: "(" + auth.Label + ")"})
	}
	for _, line := range auth.DetailLines {
		rows = append(rows, DoctorCheck{OK: true, Label: "Vercel auth " + line, Detail: ""})
	}
	persistent := truthyEnvValue(os.Getenv("TERMINAL_CONTAINER_PERSISTENT"))
	if os.Getenv("TERMINAL_CONTAINER_PERSISTENT") == "" {
		persistent = true
	}
	if persistent {
		rows = append(rows, DoctorCheck{OK: true, Label: "Vercel persistence", Detail: "snapshot filesystem only; live processes do not survive sandbox recreation"})
	} else {
		rows = append(rows, DoctorCheck{OK: true, Label: "Vercel persistence", Detail: "ephemeral filesystem"})
	}
	return rows
}

// CreateEnvironment builds the sandbox from the shaped resources and the runtime or image setting
func (vercelSandboxBackend) CreateEnvironment(ctx context.Context, env *Env, opts CreateOptions) (Environment, error) {
	res := sandboxResources(opts.ContainerConfig)
	cpu, _ := pyFloatValue(res["cpu"])
	memory, _ := pyIntValue(res["memory"])
	disk, _ := pyIntValue(res["disk"])
	return NewVercelSandboxEnvironment(ctx, env, VercelOptions{Runtime: argS(opts.ContainerConfig, "vercel_runtime"), Image: argS(opts.ContainerConfig, "vercel_image"),
		Cwd: opts.Cwd, Timeout: opts.Timeout, CPU: cpu, Memory: memory, Disk: disk, Persistent: truthy(res["persistent_filesystem"]), TaskID: opts.TaskID})
}

// Registers the ssh, modal, daytona and vercel providers
func registerRemoteTerminalBackends() {
	registerBuiltinBackend(sshBackend{})
	registerBuiltinBackend(modalBackend{})
	registerBuiltinBackend(daytonaBackend{})
	registerBuiltinBackend(vercelSandboxBackend{})
}
