package agent

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/internal/pycopy"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
	"github.com/nickheyer/icebath/pkg/internal/pyurl"
	"github.com/nickheyer/icebath/pkg/provider"
	"github.com/nickheyer/icebath/pkg/tools"
)

// Type, message and the provider fields of an API error for the request dump
func apiErrorDebugInfo(err error) *pyjson.Object {
	info := pyjson.NewObject()
	typeName := fmt.Sprintf("%T", err)
	if i := strings.LastIndex(typeName, "."); i >= 0 {
		typeName = typeName[i+1:]
	}
	info.Set("type", strings.TrimPrefix(typeName, "*"))
	info.Set("message", err.Error())
	var apiErr *provider.APIError
	if !errors.As(err, &apiErr) {
		return info
	}
	if apiErr.StatusCode != 0 {
		info.Set("status_code", apiErr.StatusCode)
	}
	if apiErr.RequestID != "" {
		info.Set("request_id", apiErr.RequestID)
	}
	if apiErr.Code != "" {
		info.Set("code", apiErr.Code)
	}
	if apiErr.Type != "" {
		info.Set("type", apiErr.Type)
	}
	if apiErr.Body != "" {
		body := any(apiErr.Body)
		if parsed, perr := pyjson.Loads(apiErr.Body); perr == nil {
			if obj, ok := parsed.(*pyjson.Object); ok {
				body = obj
			}
		}
		info.Set("body", body)
	}
	if apiErr.StatusCode != 0 {
		info.Set("response_status", apiErr.StatusCode)
		info.Set("response_text", apiErr.Body)
	}
	return info
}

// API key or auth token of the live client, empty when the client exposes neither
func (a *AIAgent) liveClientAPIKey() (key string) {
	defer func() {
		if r := recover(); r != nil {
			if isUserInterruptPanic(r) {
				panic(r)
			}
			runAgentLog().Debug(fmt.Sprintf("Could not extract API key for debug dump: %v", r))
			key = ""
		}
	}()
	live := a.client
	if a.apiMode == "anthropic_messages" {
		live = a.anthropicClient
	}
	if c, ok := live.(interface{ APIKey() string }); ok {
		if k := c.APIKey(); k != "" {
			return k
		}
	}
	if c, ok := live.(interface{ AuthToken() string }); ok {
		return c.AuthToken()
	}
	return ""
}

// Writes the redacted request body and error to a session request dump file, empty on failure
func (a *AIAgent) dumpAPIRequestDebug(apiKwargs map[string]any, reason string, apiErr error) (path string) {
	defer func() {
		if r := recover(); r != nil {
			if isUserInterruptPanic(r) {
				panic(r)
			}
			if a.verboseLogging {
				agentRuntimeHelpersLog().Warn(fmt.Sprintf("Failed to dump API request debug payload: %v", r))
			}
			path = ""
		}
	}()
	written, err := a.writeAPIRequestDump(apiKwargs, reason, apiErr)
	if err != nil {
		panic(err)
	}
	return written
}

// Builds, redacts and atomically writes the request dump, echoing it to stdout when asked
func (a *AIAgent) writeAPIRequestDump(apiKwargs map[string]any, reason string, apiErr error) (string, error) {
	body := map[string]any{}
	if copied, ok := pycopy.DeepCopy(apiKwargs).(map[string]any); ok {
		for k, v := range copied {
			if v != nil && k != "timeout" {
				body[k] = v
			}
		}
	}
	apiKey := a.liveClientAPIKey()
	endpoint := "/chat/completions"
	switch a.apiMode {
	case "codex_responses":
		endpoint = "/responses"
	case "anthropic_messages":
		endpoint = "/messages"
	}
	masked := provider.MaskAPIKeyForLogs(apiKey)
	if masked == "" {
		masked = "None"
	}
	headers := pyjson.NewObject()
	headers.Set("Authorization", "Bearer "+masked)
	headers.Set("Content-Type", "application/json")
	request := pyjson.NewObject()
	request.Set("method", "POST")
	request.Set("url", pystr.RstripChars(a.baseURL, "/")+endpoint)
	request.Set("headers", headers)
	request.Set("body", body)
	payload := pyjson.NewObject()
	payload.Set("timestamp", naiveISOFormat(time.Now()))
	payload.Set("session_id", a.sessionID)
	payload.Set("reason", reason)
	payload.Set("request", request)
	if apiErr != nil {
		payload.Set("error", apiErrorDebugInfo(apiErr))
	}
	stamp := time.Now()
	dumpFile := filepath.Join(a.logsDir, fmt.Sprintf("request_dump_%s_%s_%06d.json", safeSessionFilenameComponent(a.sessionID), stamp.Format("20060102_150405"), stamp.Nanosecond()/1000))
	str := func(v any) (any, error) { return fmt.Sprint(v), nil }
	serialized, err := pyjson.Dumps(payload, pyjson.EnsureASCII(false), pyjson.Indent(2), pyjson.Default(str))
	if err != nil {
		return "", err
	}
	redacted, err := pyjson.Loads(tools.RedactSensitiveTextWith(a.env, serialized, tools.RedactOptions{Force: true}))
	if err != nil {
		return "", err
	}
	if err := config.AtomicJSONWriteWith(dumpFile, redacted, pyjson.Default(str)); err != nil {
		return "", err
	}
	a.vprint(a.logPrefix+"🧾 Request debug dump written to: "+dumpFile, false, false)
	if config.EnvVarEnabled("ICEBATH_DUMP_REQUEST_STDOUT") {
		echo, err := pyjson.Dumps(redacted, pyjson.EnsureASCII(false), pyjson.Indent(2), pyjson.Default(str))
		if err != nil {
			return "", err
		}
		fmt.Fprintln(os.Stdout, echo)
	}
	return dumpFile, nil
}

// Whether the agent destination is the direct Anthropic Messages host that accepts native tool markers
func (a *AIAgent) directNativeAnthropicToolCacheCapability() bool {
	if a.apiMode != "anthropic_messages" {
		return false
	}
	host, err := pyurl.BaseURLHostname(a.baseURL)
	if err != nil {
		panic(err)
	}
	return host == "api.anthropic.com"
}

// Raw sockets of the client pools, only the own in flight requests on a process shared pool
func iterPoolSockets(client any) []syscall.Conn {
	var out []syscall.Conn
	for _, conn := range provider.PoolSockets(client) {
		if sock, ok := conn.(syscall.Conn); ok {
			out = append(out, sock)
		}
	}
	return out
}
