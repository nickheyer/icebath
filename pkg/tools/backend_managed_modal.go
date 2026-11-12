package tools

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
)

var managedModalTerminalStatuses = map[string]bool{"completed": true, "failed": true, "cancelled": true, "timeout": true}

const managedModalClientTimeoutGrace = 10 * time.Second

// Positive seconds from an env var, the default otherwise
func managedModalTimeoutEnv(name string, fallback float64) time.Duration {
	if f, err := strconv.ParseFloat(strings.TrimSpace(os.Getenv(name)), 64); err == nil && f > 0 {
		return time.Duration(f * float64(time.Second))
	}
	return time.Duration(fallback * float64(time.Second))
}

// Exec result in the tool shape
func managedModalResult(output string, code int) *ExecResult {
	return &ExecResult{Output: output, ReturnCode: code, OutputTotalChars: len([]rune(output))}
}

// managedModalEnv is a gateway owned Modal sandbox whose command preparation and cwd tracking happen server side
type managedModalEnv struct {
	baseEnv
	gatewayOrigin string
	userToken     string
	taskID        string
	persistent    bool
	image         string
	kwargs        map[string]any
	idempotency   string
	sandboxID     string
	connectTO     time.Duration
	pollReadTO    time.Duration
	cancelReadTO  time.Duration
}

// NewManagedModalEnvironment creates a sandbox through the Nous tool gateway
func NewManagedModalEnvironment(ctx context.Context, env *Env, o ModalOptions) (*managedModalEnv, error) {
	if o.Cwd == "" {
		o.Cwd = "/root"
	}
	if o.Timeout == 0 {
		o.Timeout = 60
	}
	if o.TaskID == "" {
		o.TaskID = "default"
	}
	if len(GetCredentialFileMounts(env)) > 0 {
		return nil, errors.New("Managed Modal does not support host credential-file passthrough. " +
			"Use TERMINAL_MODAL_MODE=direct when skills or config require " +
			"credential files inside the sandbox.")
	}
	gateway, err := ResolveManagedToolGateway(env, "modal", BuildVendorGatewayURL, readNousAccessToken)
	if err != nil || gateway == nil {
		return nil, errors.New("Managed Modal requires a configured tool gateway and Nous user token")
	}
	m := &managedModalEnv{gatewayOrigin: strings.TrimRight(gateway.GatewayOrigin, "/"), userToken: gateway.NousUserToken,
		taskID: o.TaskID, persistent: o.Persistent, image: o.Image, kwargs: o.SandboxKwargs, idempotency: uuid.NewString(),
		connectTO:    managedModalTimeoutEnv("TERMINAL_MANAGED_MODAL_CONNECT_TIMEOUT_SECONDS", 1.0),
		pollReadTO:   managedModalTimeoutEnv("TERMINAL_MANAGED_MODAL_POLL_READ_TIMEOUT_SECONDS", 5.0),
		cancelReadTO: managedModalTimeoutEnv("TERMINAL_MANAGED_MODAL_CANCEL_READ_TIMEOUT_SECONDS", 5.0)}
	m.stdinMode = "payload"
	initBaseEnv(&m.baseEnv, m, env, o.Cwd, o.Timeout, nil, true)
	id, err := m.createSandbox()
	if err != nil {
		return nil, err
	}
	m.sandboxID = id
	return m, nil
}

func (m *managedModalEnv) EnvType() string { return "modal" }

// Python class name of the environment as hermes logs and records it
func (m *managedModalEnv) ClassName() string { return "ManagedModalEnvironment" }

var _ interface{ ClassName() string } = (*managedModalEnv)(nil)

// Managed sandboxes never spawn local processes, every command goes through Execute
func (m *managedModalEnv) runBash(ctx context.Context, cmd string, login bool, timeout int, stdinData *string) (ProcessHandle, error) {
	return nil, errors.New("Managed Modal runs commands through the tool gateway, not a local process")
}

// HTTP client with a connect timeout and a response header timeout
func managedModalClient(connect, read time.Duration) *http.Client {
	transport := &http.Transport{DialContext: (&net.Dialer{Timeout: connect}).DialContext, ResponseHeaderTimeout: read, TLSHandshakeTimeout: connect + read}
	return &http.Client{Transport: transport}
}

// Gateway request with the bearer token, JSON body and per call timeouts
func (m *managedModalEnv) request(method, path string, body any, connect, read time.Duration, extra map[string]string) (*http.Response, []byte, error) {
	var reader io.Reader
	if body != nil {
		raw, err := pyjson.EncodeWTF8ASCII(body)
		if err != nil {
			return nil, nil, err
		}
		reader = bytes.NewReader(raw)
	}
	req, err := http.NewRequest(method, m.gatewayOrigin+path, reader)
	if err != nil {
		return nil, nil, err
	}
	req.Header.Set("Authorization", "Bearer "+m.userToken)
	req.Header.Set("Content-Type", "application/json")
	for k, v := range extra {
		req.Header.Set(k, v)
	}
	resp, err := managedModalClient(connect, read).Do(req)
	if err != nil {
		return nil, nil, err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return resp, nil, err
	}
	return resp, data, nil
}

// Prefix plus the gateway error, message, code or body text, else the HTTP status
func managedModalFormatError(prefix string, resp *http.Response, data []byte) string {
	if decoded, err := pyjson.LoadsBytes(data); err == nil {
		if payload, ok := decoded.(*pyjson.Object); ok {
			var message any
			for _, key := range []string{"error", "message", "code"} {
				if message = payload.Vals[key]; pyrepr.Truthy(message) {
					break
				}
			}
			if s, ok := message.(string); ok && s != "" {
				return prefix + ": " + s
			}
			if text, err := pyjson.Dumps(payload, pyjson.EnsureASCII(false)); err == nil {
				return prefix + ": " + text
			}
		}
	}
	if text := strings.TrimSpace(string(data)); text != "" {
		return prefix + ": " + text
	}
	return fmt.Sprintf("%s: HTTP %d", prefix, resp.StatusCode)
}

// Final result when the exec body reports a terminal status
func managedModalResultFromBody(body map[string]any) *ExecResult {
	status, _ := body["status"].(string)
	if !managedModalTerminalStatuses[status] {
		return nil
	}
	output, _ := body["output"].(string)
	code := 1
	if n, ok := pyIntValue(body["returncode"]); ok {
		code = n
	}
	return managedModalResult(output, code)
}

func (m *managedModalEnv) createSandbox() (string, error) {
	cpu := 1.0
	if f, ok := pyFloatValue(m.kwargs["cpu"]); ok {
		cpu = f
	}
	memory := 5120.0
	if v, ok := m.kwargs["memoryMiB"]; ok {
		if f, ok := pyFloatValue(v); ok {
			memory = f
		}
	} else if f, ok := pyFloatValue(m.kwargs["memory"]); ok {
		memory = f
	}
	payload := map[string]any{"image": m.image, "cwd": m.Cwd(), "cpu": cpu, "memoryMiB": memory, "timeoutMs": 3_600_000,
		"idleTimeoutMs": max(300_000, m.timeout*1000), "persistentFilesystem": m.persistent, "logicalKey": m.taskID}
	for _, key := range []string{"ephemeral_disk", "diskMiB"} {
		if f, ok := pyFloatValue(m.kwargs[key]); ok {
			payload["diskMiB"] = f
			break
		}
	}
	resp, data, err := m.request("POST", "/v1/sandboxes", payload, 60*time.Second, 60*time.Second, map[string]string{"x-idempotency-key": m.idempotency})
	if err != nil {
		return "", err
	}
	if resp.StatusCode >= 400 {
		return "", errors.New(managedModalFormatError("Managed Modal create failed", resp, data))
	}
	decoded, err := pyjson.LoadsBytes(data)
	if err != nil {
		return "", err
	}
	body, ok := decoded.(*pyjson.Object)
	if !ok {
		return "", fmt.Errorf("'%s' object has no attribute 'get'", pyrepr.TypeName(decoded))
	}
	id, _ := body.Vals["id"].(string)
	if id == "" {
		return "", errors.New("Managed Modal create did not return a sandbox id")
	}
	return id, nil
}

// Execute runs the command through the gateway and polls until a terminal status, interrupt, or timeout
func (m *managedModalEnv) Execute(ctx context.Context, command string, opts ExecOptions) (*ExecResult, error) {
	execCommand, sudoStdin := m.prepareCommand(ctx, command)
	if sudoStdin != nil {
		execCommand = "printf '%s\\n' " + shQuote(strings.TrimRight(*sudoStdin, "\n")) + " | " + execCommand
	}
	timeout := opts.Timeout
	if timeout <= 0 {
		timeout = m.timeout
	}
	cwd := opts.Cwd
	if cwd == "" {
		cwd = m.Cwd()
	}
	execID := uuid.NewString()
	payload := map[string]any{"execId": execID, "command": execCommand, "cwd": cwd, "timeoutMs": timeout * 1000}
	if opts.Stdin != nil {
		payload["stdinData"] = *opts.Stdin
	}
	resp, data, err := m.request("POST", "/v1/sandboxes/"+m.sandboxID+"/execs", payload, 10*time.Second, 10*time.Second, nil)
	if err != nil {
		return managedModalResult("Managed Modal exec failed: "+err.Error(), 1), nil
	}
	if resp.StatusCode >= 400 {
		return managedModalResult(managedModalFormatError("Managed Modal exec failed", resp, data), 1), nil
	}
	decoded, err := pyjson.LoadsBytes(data)
	if err != nil {
		return managedModalResult("Managed Modal exec failed: "+err.Error(), 1), nil
	}
	if decoded == nil {
		return managedModalResult(managedModalFormatError("Managed Modal exec failed", resp, data), 1), nil
	}
	started, ok := decoded.(*pyjson.Object)
	if !ok {
		return nil, fmt.Errorf("'%s' object has no attribute 'get'", pyrepr.TypeName(decoded))
	}
	body := started.Vals
	if final := managedModalResultFromBody(body); final != nil {
		return final, nil
	}
	if body["execId"] != execID {
		return managedModalResult("Managed Modal exec start did not return the expected exec id", 1), nil
	}
	deadline := time.Now().Add(time.Duration(timeout)*time.Second + managedModalClientTimeoutGrace)
	activity := newActivityState()
	for {
		if IsInterrupted(ctx) {
			m.cancelExec(execID)
			return managedModalResult("[Command interrupted - Modal sandbox exec cancelled]", 130), nil
		}
		if result := m.pollExec(execID); result != nil {
			return result, nil
		}
		if !time.Now().Before(deadline) {
			m.cancelExec(execID)
			return managedModalResult(fmt.Sprintf("Managed Modal exec timed out after %ds", timeout), 124), nil
		}
		touchActivityIfDue(ctx, m.env, activity, "modal command running")
		time.Sleep(250 * time.Millisecond)
	}
}

// One poll of the exec, a result on a terminal status or failure, nil while still running
func (m *managedModalEnv) pollExec(execID string) *ExecResult {
	resp, data, err := m.request("GET", "/v1/sandboxes/"+m.sandboxID+"/execs/"+execID, nil, m.connectTO, m.pollReadTO, nil)
	if err != nil {
		return managedModalResult("Managed Modal exec poll failed: "+err.Error(), 1)
	}
	if resp.StatusCode == 404 {
		return managedModalResult("Managed Modal exec not found", 1)
	}
	if resp.StatusCode >= 400 {
		return managedModalResult(managedModalFormatError("Managed Modal exec poll failed", resp, data), 1)
	}
	decoded, err := pyjson.LoadsBytes(data)
	if err != nil {
		return managedModalResult("Managed Modal exec failed: "+err.Error(), 1)
	}
	body, ok := decoded.(*pyjson.Object)
	if !ok {
		return managedModalResult("Managed Modal exec failed: '"+pyrepr.TypeName(decoded)+"' object has no attribute 'get'", 1)
	}
	return managedModalResultFromBody(body.Vals)
}

func (m *managedModalEnv) cancelExec(execID string) {
	if _, _, err := m.request("POST", "/v1/sandboxes/"+m.sandboxID+"/execs/"+execID+"/cancel", nil, m.connectTO, m.cancelReadTO, nil); err != nil {
		envLog(m.env).Warn("Managed Modal exec cancel failed", "error", err)
	}
}

// Cleanup terminates the sandbox, snapshotting first when persistent
func (m *managedModalEnv) Cleanup(forceRemove bool) error {
	if m.sandboxID == "" {
		return nil
	}
	defer func() { m.sandboxID = "" }()
	if _, _, err := m.request("POST", "/v1/sandboxes/"+m.sandboxID+"/terminate", map[string]any{"snapshotBeforeTerminate": m.persistent}, 60*time.Second, 60*time.Second, nil); err != nil {
		envLog(m.env).Warn("Managed Modal cleanup failed", "error", err)
	}
	return nil
}

// FetchFile reads a file through a gateway exec since no local process exists
func (m *managedModalEnv) FetchFile(ctx context.Context, remotePath, localDest string, maxBytes int) error {
	result, err := m.Execute(ctx, "base64 < "+shQuote(remotePath), ExecOptions{Timeout: 60})
	if err != nil {
		return err
	}
	if result.ReturnCode != 0 {
		return &FileFetchError{Msg: strings.TrimSpace(result.Output)}
	}
	data, err := decodeBase64Loose(result.Output)
	if err != nil {
		return err
	}
	if maxBytes > 0 && len(data) > maxBytes {
		return &FileFetchError{Msg: fmt.Sprintf("file exceeds %d bytes", maxBytes)}
	}
	return os.WriteFile(localDest, data, 0o600)
}

// FetchRealpath resolves a path through a gateway exec
func (m *managedModalEnv) FetchRealpath(ctx context.Context, remotePath string) string {
	result, err := m.Execute(ctx, "realpath "+shQuote(remotePath), ExecOptions{Timeout: 30})
	if err != nil || result.ReturnCode != 0 {
		return ""
	}
	return strings.TrimSpace(result.Output)
}

// Decodes base64 text ignoring whitespace and a missing trailing pad
func decodeBase64Loose(text string) ([]byte, error) {
	cleaned := strings.Map(func(r rune) rune {
		if r == '\n' || r == '\r' || r == ' ' || r == '\t' {
			return -1
		}
		return r
	}, text)
	if rem := len(cleaned) % 4; rem != 0 {
		cleaned += strings.Repeat("=", 4-rem)
	}
	return base64.StdEncoding.DecodeString(cleaned)
}
