package tools

import (
	"context"
	"errors"
	"fmt"
	"mime"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pypath"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
)

const (
	connectorSignInExpired = "sign_in_expired"
	connectorUnreachable   = "unreachable"
	connectorNotSetUp      = "not_set_up"
	connectorNotSetUpMsg   = "Hosted connectors are not set up for this user yet. Call manage_connections to connect the app first; that sets them up."
)

var tokenRejectedCodes = map[string]bool{"UNAUTHORIZED": true, "INVALID_TOKEN": true, "TOKEN_EXPIRED": true}

// ConnectorLeg is the payload or failure class of one read leg to the gateway
type ConnectorLeg struct {
	Payload map[string]any
	Failure string
}

func legFailure(err error) string {
	if ge, ok := gatewayErrorKind(err, gatewayKindAuth); ok {
		if ge.Code == connectorNotSetUp {
			return connectorNotSetUp
		}
		if ge.Status == 401 || tokenRejectedCodes[strings.ToUpper(ge.Code)] {
			return connectorSignInExpired
		}
		return ""
	}
	return connectorUnreachable
}

// The real client for the model's read and execute legs, never creating an identity
func defaultConnectorClient(env *Env, call *CallRef) (*ConnectorClient, error) {
	if guestIdentityPending(env) {
		return nil, newToolGatewayError(gatewayKindAuth, connectorNotSetUpMsg, connectorNotSetUp, 0, "", false)
	}
	return NewConnectorClient(env, call, ""), nil
}

// ConnectorSearchHits runs use case queries against the gateway as a total leg
func ConnectorSearchHits(ctx context.Context, env *Env, queries []map[string]any, call *CallRef) ConnectorLeg {
	if !ConnectorsAvailable(env) || len(queries) == 0 {
		return ConnectorLeg{}
	}
	client, err := defaultConnectorClient(env, call)
	if err != nil {
		envLog(env).Warn("Connector search failed (D32)", "error", err)
		return ConnectorLeg{Failure: legFailure(err)}
	}
	payload, err := client.Search(ctx, queries)
	if err != nil {
		if _, ok := gatewayErrorKind(err, gatewayKindUnavailable); ok {
			envLog(env).Debug("Connector search skipped: gateway dark")
			return ConnectorLeg{}
		}
		envLog(env).Warn("Connector search failed (D32)", "error", err)
		return ConnectorLeg{Failure: legFailure(err)}
	}
	if payload == nil {
		payload = map[string]any{}
	}
	return ConnectorLeg{Payload: payload}
}

// ConnectorDescribe fetches model schemas for composed connector names as a total leg
func ConnectorDescribe(ctx context.Context, env *Env, names []string, call *CallRef) ConnectorLeg {
	if !ConnectorsAvailable(env) {
		return ConnectorLeg{}
	}
	wanted := map[string][]string{}
	var order, requestSlugs []string
	for _, name := range names {
		parsed, ok := ParseConnectorName(name)
		if !ok {
			continue
		}
		if _, seen := wanted[parsed.Raw]; seen {
			continue
		}
		candidates := VendorSlugCandidates(parsed.Connector, parsed.Tool)
		wanted[parsed.Raw] = candidates
		order = append(order, parsed.Raw)
		for _, slug := range candidates {
			if !inList(requestSlugs, slug) {
				requestSlugs = append(requestSlugs, slug)
			}
		}
	}
	if len(wanted) == 0 {
		return ConnectorLeg{}
	}
	client, err := defaultConnectorClient(env, call)
	if err != nil {
		envLog(env).Warn("Connector describe failed (D32)", "error", err)
		return ConnectorLeg{Failure: legFailure(err)}
	}
	response, err := client.Schemas(ctx, requestSlugs)
	if err != nil {
		if _, ok := gatewayErrorKind(err, gatewayKindUnavailable); ok {
			envLog(env).Debug("Connector describe skipped: gateway dark")
			return ConnectorLeg{}
		}
		envLog(env).Warn("Connector describe failed (D32)", "error", err)
		return ConnectorLeg{Failure: legFailure(err)}
	}
	schemas := asMap(response["schemas"])
	tools := map[string]any{}
	for _, composed := range order {
		var schema map[string]any
		for _, slug := range wanted[composed] {
			if s := asMap(schemas[slug]); s != nil {
				schema = s
				break
			}
		}
		if schema == nil {
			continue
		}
		input := asMap(schema["input_schema"])
		if input == nil {
			input = map[string]any{}
		}
		tools[composed] = map[string]any{"description": asStr(schema["description"]), "parameters": ModelParameters(composed, input)}
	}
	return ConnectorLeg{Payload: map[string]any{"tools": tools}}
}

// RunRemote executes planned connector calls, filling failures as per entry errors
func RunRemote(ctx context.Context, env *Env, planned []PlannedCall, dispatchID, turnID, taskID string) []map[string]any {
	if !ConnectorsAvailable(env) {
		return FillRemoteFailure(planned, "Unknown tool: connectors are not available in this session.", "TOOL_NOT_FOUND")
	}
	wirePlanned := make([]PlannedCall, 0, len(planned))
	for _, plan := range planned {
		p := plan
		p.Tool = VendorSlugCandidates(plan.Connector, plan.Tool)[0]
		wirePlanned = append(wirePlanned, p)
	}
	client, err := defaultConnectorClient(env, &CallRef{TurnID: turnID, ToolCallID: dispatchID})
	if err != nil {
		return remoteFailureEntries(env, planned, dispatchID, err)
	}
	results, err := executeWithFiles(ctx, env, client, wirePlanned, taskID)
	if err != nil {
		return remoteFailureEntries(env, planned, dispatchID, err)
	}
	entries := SpliceRemoteResults(ctx, planned, results)
	var fallbackSlots []int
	var fallbackPlanned []PlannedCall
	for slot, plan := range planned {
		candidates := VendorSlugCandidates(plan.Connector, plan.Tool)
		if candidates[0] == candidates[1] || slot >= len(entries) {
			continue
		}
		if e := asMap(entries[slot]["error"]); e != nil && asStr(e["code"]) == "TOOL_NOT_FOUND" {
			fallbackSlots = append(fallbackSlots, slot)
			p := plan
			p.Tool = candidates[1]
			fallbackPlanned = append(fallbackPlanned, p)
		}
	}
	if len(fallbackPlanned) == 0 {
		return entries
	}
	var fallbackEntries []map[string]any
	fallbackResults, err := executeWithFiles(ctx, env, client, fallbackPlanned, taskID)
	if err != nil {
		fallbackEntries = remoteFailureEntries(env, fallbackPlanned, dispatchID, err)
	} else {
		fallbackEntries = SpliceRemoteResults(ctx, fallbackPlanned, fallbackResults)
	}
	for i, slot := range fallbackSlots {
		if i < len(fallbackEntries) {
			entries[slot] = fallbackEntries[i]
		}
	}
	return entries
}

func remoteFailureEntries(env *Env, planned []PlannedCall, dispatchID string, err error) []map[string]any {
	var ge *ToolGatewayError
	if errors.As(err, &ge) {
		envLog(env).Warn("Connector execute failed", "dispatch", dispatchID, "code", ge.Code, "error", err)
		return FillRemoteFailure(planned, "The connector gateway request failed: "+ge.Message, "")
	}
	envLog(env).Warn("Connector execute failed unexpectedly", "dispatch", dispatchID, "error", err)
	return FillRemoteFailure(planned, "The connector gateway request failed unexpectedly.", "")
}

// Executes planned calls with every file input staged first and every returned file saved
func executeWithFiles(ctx context.Context, env *Env, client *ConnectorClient, planned []PlannedCall, taskID string) ([]map[string]any, error) {
	results := make([]map[string]any, len(planned))
	var runnable []PlannedCall
	var runnableSlots []int
	for slot, plan := range planned {
		staged, err := StageInputs(ctx, env, client, plan, taskID)
		if err != nil {
			var fe *FileInputError
			if errors.As(err, &fe) {
				envLog(env).Warn("Connector file input refused", "tool", plan.Name, "code", fe.Code, "error", err)
				results[slot] = fe.Result()
				continue
			}
			return nil, err
		}
		runnable = append(runnable, staged)
		runnableSlots = append(runnableSlots, slot)
	}
	if len(runnable) > 0 {
		returnTo, op := ReturnToArgs(env, "")
		remote, err := client.Execute(ctx, runnable, returnTo, op)
		if err != nil {
			return nil, err
		}
		for i, slot := range runnableSlots {
			if i < len(remote) {
				results[slot] = SaveOutputs(ctx, env, client, remote[i])
			}
		}
	}
	return results, nil
}

// ---- files --------------------------------------------------------------------------------------

// DownloadMaxBytes bounds one connector file download
const DownloadMaxBytes = 50 * 1024 * 1024

const fileInputNote = "Pass a local file path."
const arrayItem = "*"
const maxSchemaDepth = 32
const maxNameChars = 150

var hostReadableBackends = map[string]bool{"local": true, "docker": true}
var unsafeNameChars = regexp.MustCompile(`[\x00-\x1f\x7f<>:"|?*\\/]`)

var fileInputs = struct {
	sync.Mutex
	byName map[string][][]string
}{byName: map[string][][]string{}}

// MediaDeliveryPathValidator returns the host path a session may send or empty, installed by the gateway
var MediaDeliveryPathValidator func(env *Env, candidate, sessionKey string) string

// FileInputError is a file input refused before the call reached execute
type FileInputError struct {
	Code string
	Msg  string
	Hint string
}

// File input refusal with its code, message and the optional hint the model reads
func newFileInputError(code, message, hint string) *FileInputError {
	return &FileInputError{Code: code, Msg: message, Hint: hint}
}

func (e *FileInputError) Error() string { return e.Msg }

// Result is the call's error entry in the shape execute errors take
func (e *FileInputError) Result() map[string]any {
	errMap := map[string]any{"code": e.Code, "message": e.Msg, "family": executeErrorFamily(e.Code, "")}
	if e.Hint != "" {
		errMap["hint"] = e.Hint
	}
	return map[string]any{"data": nil, "error": errMap}
}

type stagedDescriptor map[string]any

// ModelParameters renders the tool parameters the model sees and records its file input paths
func ModelParameters(name string, inputSchema map[string]any) map[string]any {
	if inputSchema == nil {
		return map[string]any{}
	}
	var paths [][]string
	seen := map[string]bool{}
	for _, p := range filePaths(inputSchema, inputSchema, nil, 0) {
		key := strings.Join(p, "\x00")
		if !seen[key] {
			seen[key] = true
			paths = append(paths, p)
		}
	}
	fileInputs.Lock()
	fileInputs.byName[name] = paths
	fileInputs.Unlock()
	out, _ := asModelSchema(inputSchema).(map[string]any)
	if out == nil {
		out = map[string]any{}
	}
	return out
}

func asModelSchema(node any) any {
	switch v := node.(type) {
	case []any:
		out := make([]any, len(v))
		for i, item := range v {
			out[i] = asModelSchema(item)
		}
		return out
	case map[string]any:
		if up, ok := v["file_uploadable"].(bool); ok && up {
			return map[string]any{"type": "string", "description": strings.TrimSpace(asStr(v["description"]) + " " + fileInputNote)}
		}
		out := make(map[string]any, len(v))
		for k, item := range v {
			out[k] = asModelSchema(item)
		}
		return out
	}
	return node
}

func filePaths(node any, root map[string]any, path []string, depth int) [][]string {
	m := asMap(node)
	if m == nil || depth > maxSchemaDepth {
		return nil
	}
	if resolved := asMap(ResolveLocalSchemaRef(m, root)); resolved != nil {
		m = resolved
	}
	if up, ok := m["file_uploadable"].(bool); ok && up {
		return [][]string{append([]string{}, path...)}
	}
	var out [][]string
	for _, branch := range []string{"anyOf", "oneOf", "allOf"} {
		for _, option := range asList(m[branch]) {
			out = append(out, filePaths(option, root, path, depth+1)...)
		}
	}
	if props := asMap(m["properties"]); props != nil {
		for _, key := range sortedConnectorKeys(props) {
			out = append(out, filePaths(props[key], root, append(append([]string{}, path...), key), depth+1)...)
		}
	}
	out = append(out, filePaths(m["items"], root, append(append([]string{}, path...), arrayItem), depth+1)...)
	return out
}

func knownFileInputs(ctx context.Context, env *Env, client *ConnectorClient, plan PlannedCall) [][]string {
	fileInputs.Lock()
	paths, ok := fileInputs.byName[plan.Name]
	fileInputs.Unlock()
	if ok {
		return paths
	}
	parsed, ok := ParseConnectorName(plan.Name)
	if !ok {
		return nil
	}
	candidates := VendorSlugCandidates(parsed.Connector, parsed.Tool)
	response, err := client.Schemas(ctx, candidates)
	if err != nil {
		code := ""
		var ge *ToolGatewayError
		if errors.As(err, &ge) {
			code = ge.Code
		}
		envLog(env).Warn("connector schema fetch for file inputs failed; executing without staging", "tool", plan.Name, "code", code)
		return nil
	}
	schemas := asMap(response["schemas"])
	schema := map[string]any{}
	for _, slug := range candidates {
		if s := asMap(schemas[slug]); s != nil {
			schema = s
			break
		}
	}
	input := asMap(schema["input_schema"])
	if input == nil {
		input = map[string]any{}
	}
	ModelParameters(plan.Name, input)
	fileInputs.Lock()
	defer fileInputs.Unlock()
	return fileInputs.byName[plan.Name]
}

// StageInputs replaces each file input path with a staged descriptor for the plan's tool
func StageInputs(ctx context.Context, env *Env, client *ConnectorClient, plan PlannedCall, taskID string) (PlannedCall, error) {
	if !hasText(plan.Arguments) {
		return plan, nil
	}
	paths := knownFileInputs(ctx, env, client, plan)
	if len(paths) == 0 {
		return plan, nil
	}
	var args any = plan.Arguments
	for _, path := range paths {
		next, err := substituteFileInput(args, path, func(raw string) (any, error) { return stageOne(ctx, env, client, plan, raw, taskID) })
		if err != nil {
			return plan, err
		}
		args = next
	}
	out := plan
	out.Arguments = asMap(args)
	if out.Arguments == nil {
		out.Arguments = plan.Arguments
	}
	return out, nil
}

func hasText(value any) bool {
	switch v := value.(type) {
	case string:
		return true
	case map[string]any:
		for _, item := range v {
			if hasText(item) {
				return true
			}
		}
	case []any:
		for _, item := range v {
			if hasText(item) {
				return true
			}
		}
	}
	return false
}

// Copy of the value with the leaf at path staged, missing or non path leaves left as they are
func substituteFileInput(value any, path []string, stage func(string) (any, error)) (any, error) {
	if len(path) == 0 {
		if s, ok := value.(string); ok {
			return stage(s)
		}
		if m, ok := value.(map[string]any); ok {
			if _, hasKey := m["s3key"]; hasKey {
				return nil, newFileInputError("FILE_NOT_ALLOWED", "File inputs take a local file path, not a storage descriptor.", "Pass the path of the local file to send.")
			}
		}
		return value, nil
	}
	head, rest := path[0], path[1:]
	if head == arrayItem {
		list, ok := value.([]any)
		if !ok {
			return value, nil
		}
		out := make([]any, len(list))
		for i, item := range list {
			next, err := substituteFileInput(item, rest, stage)
			if err != nil {
				return nil, err
			}
			out[i] = next
		}
		return out, nil
	}
	m, ok := value.(map[string]any)
	if staged, isStaged := value.(stagedDescriptor); isStaged {
		m, ok = staged, true
	}
	if !ok {
		return value, nil
	}
	child, present := m[head]
	if !present {
		return value, nil
	}
	next, err := substituteFileInput(child, rest, stage)
	if err != nil {
		return nil, err
	}
	out := make(map[string]any, len(m))
	for k, v := range m {
		out[k] = v
	}
	out[head] = next
	return out, nil
}

func stageOne(ctx context.Context, env *Env, client *ConnectorClient, plan PlannedCall, raw, taskID string) (any, error) {
	path, err := readableFile(env, raw, taskID)
	if err != nil {
		return nil, err
	}
	data, err := readFileBytes(raw, path)
	if err != nil {
		return nil, err
	}
	name := filepath.Base(path)
	if r := []rune(name); len(r) > 255 {
		name = string(r[len(r)-255:])
	}
	mimetype := mime.TypeByExtension(filepath.Ext(name))
	if mimetype == "" {
		mimetype = "application/octet-stream"
	} else if i := strings.Index(mimetype, ";"); i > 0 {
		mimetype = mimetype[:i]
	}
	staged, err := client.StageFile(ctx, strings.ToLower(plan.Connector), plan.Tool, name, mimetype, len(data))
	if err != nil {
		var ge *ToolGatewayError
		if errors.As(err, &ge) && ge.Code == "TOOL_NOT_ALLOWED" {
			return nil, newFileInputError("TOOL_NOT_ALLOWED", ge.Message, "")
		}
		return nil, newFileInputError("FILE_UPLOAD_FAILED", fmt.Sprintf("The tool gateway could not stage %s: %v", name, err), "")
	}
	headers := map[string]string{}
	for k, v := range asMap(staged["headers"]) {
		headers[k], _ = v.(string)
	}
	if err := client.PutFile(ctx, asStr(staged["upload_url"]), headers, data); err != nil {
		return nil, newFileInputError("FILE_UPLOAD_FAILED", fmt.Sprintf("Uploading %s to storage failed: %v", name, err), "")
	}
	return stagedDescriptor(asMap(staged["descriptor"])), nil
}

// The host path of a file the model may send by the media delivery rule
func readableFile(env *Env, raw, taskID string) (string, error) {
	candidate, err := backendPath(env, raw, taskID)
	if err != nil {
		return "", err
	}
	if MediaDeliveryPathValidator == nil {
		panic("tools: MediaDeliveryPathValidator must be installed by the gateway before connector file inputs are staged")
	}
	if allowed := MediaDeliveryPathValidator(env, candidate, sessionValue(env, "key", "ICEBATH_SESSION_KEY")); allowed != "" {
		return allowed, nil
	}
	if !isFile(candidate) {
		return "", newFileInputError("FILE_NOT_FOUND", raw+" is not a regular file on this machine.", "Pass the path of an existing file.")
	}
	return "", newFileInputError("FILE_NOT_ALLOWED", raw+" may not leave this machine: the file-sending policy refuses it.",
		"Files under the Icebath downloads and cache folders, or a folder listed in gateway.media_delivery_allow_dirs, can be sent.")
}

// The raw path resolved the way the file tools resolve it, refusing off host backends
func backendPath(env *Env, raw, taskID string) (string, error) {
	backend := terminalBackendName(env)
	if taskID == "" {
		taskID = "default"
	}
	if active, err := GetActiveEnv(env, taskID); err == nil && active != nil {
		backend = active.EnvType()
	}
	if !hostReadableBackends[backend] {
		return "", newFileInputError("FILE_NOT_ALLOWED", fmt.Sprintf("Files in the %s terminal backend are not on this machine, so they cannot be sent as a connector file input.", backend),
			"Use a file on this machine, or switch to the local or Docker terminal backend.")
	}
	return resolvePathForTask(env, raw)
}

func readFileBytes(raw, path string) ([]byte, error) {
	st, err := os.Stat(path)
	if err != nil {
		return nil, newFileInputError("FILE_NOT_FOUND", fmt.Sprintf("%s could not be read: %v.", raw, errnoText(err)), "")
	}
	if st.Size() > connectorFileMaxBytes {
		return nil, newFileInputError("FILE_TOO_LARGE", fmt.Sprintf("%s is %d MiB; connector file inputs are limited to %d MiB.", raw, st.Size()/(1024*1024), connectorFileMaxBytes/(1024*1024)), "")
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, newFileInputError("FILE_NOT_FOUND", fmt.Sprintf("%s could not be read: %v.", raw, errnoText(err)), "")
	}
	if len(data) == 0 {
		return nil, newFileInputError("FILE_NOT_FOUND", raw+" is empty.", "")
	}
	return data, nil
}

func errnoText(err error) string {
	var pe *os.PathError
	if errors.As(err, &pe) {
		return pe.Err.Error()
	}
	return err.Error()
}

// SaveOutputs downloads each file a result returns, replacing its URL with a local path
func SaveOutputs(ctx context.Context, env *Env, client *ConnectorClient, result map[string]any) map[string]any {
	if result == nil || result["error"] != nil || result["data"] == nil {
		return result
	}
	out := make(map[string]any, len(result))
	for k, v := range result {
		out[k] = v
	}
	out["data"] = savedOutputs(ctx, env, client, result["data"])
	return out
}

// Value with every returned file downloaded, objects keeping their key order
func savedOutputs(ctx context.Context, env *Env, client *ConnectorClient, value any) any {
	switch v := value.(type) {
	case []any:
		out := make([]any, len(v))
		for i, item := range v {
			out[i] = savedOutputs(ctx, env, client, item)
		}
		return out
	case *pyjson.Object:
		if isFileOutput(v.Vals) {
			return downloadOutputObject(ctx, env, client, v)
		}
		out := pyjson.NewObject()
		for _, k := range v.Keys {
			out.Set(k, savedOutputs(ctx, env, client, v.Vals[k]))
		}
		return out
	case map[string]any:
		if isFileOutput(v) {
			return downloadOutput(ctx, env, client, v)
		}
		out := make(map[string]any, len(v))
		for k, item := range v {
			out[k] = savedOutputs(ctx, env, client, item)
		}
		return out
	}
	return value
}

// Downloaded file entry of an ordered file object, its url and expiry dropped and its path or error added
func downloadOutputObject(ctx context.Context, env *Env, client *ConnectorClient, file *pyjson.Object) *pyjson.Object {
	kept := pyjson.NewObject()
	for _, k := range file.Keys {
		if k != "url" && k != "expiresAt" {
			kept.Set(k, file.Vals[k])
		}
	}
	result := downloadOutput(ctx, env, client, file.Vals)
	for _, k := range []string{"path", "error"} {
		if v, has := result[k]; has {
			kept.Set(k, v)
		}
	}
	return kept
}

func isFileOutput(v map[string]any) bool {
	for _, key := range []string{"name", "mimetype", "url"} {
		if _, ok := v[key].(string); !ok {
			return false
		}
	}
	u := v["url"].(string)
	return strings.HasPrefix(u, "https://") || strings.HasPrefix(u, "http://")
}

func downloadOutput(ctx context.Context, env *Env, client *ConnectorClient, file map[string]any) map[string]any {
	kept := map[string]any{}
	for k, v := range file {
		if k != "url" && k != "expiresAt" {
			kept[k] = v
		}
	}
	path, err := fetchToDownloads(ctx, env, client, file["url"].(string), file["name"].(string))
	if err != nil {
		envLog(env).Warn("connector file download failed", "name", safeFileName(file["name"].(string)), "error", err)
		kept["error"] = "download failed: " + err.Error()
		return kept
	}
	kept["path"] = path
	return kept
}

// Streams a URL into a temp file then links it under a fresh name in the connector downloads folder
func fetchToDownloads(ctx context.Context, env *Env, client *ConnectorClient, u, name string) (string, error) {
	folder, err := home.Mkdir(WithEnvHome(ctx, env), filepath.Join(env.Home, "downloads", "connectors"))
	if err != nil {
		return "", err
	}
	handle, err := os.CreateTemp(folder, ".partial-*")
	if err != nil {
		return "", err
	}
	tempName := handle.Name()
	defer os.Remove(tempName)
	sink := func(chunk []byte) error {
		_, err := handle.Write(chunk)
		return err
	}
	if err := client.GetFile(ctx, u, sink, DownloadMaxBytes); err != nil {
		handle.Close()
		return "", err
	}
	if err := handle.Close(); err != nil {
		return "", err
	}
	target, err := reserveDownloadName(folder, safeFileName(name))
	if err != nil {
		return "", err
	}
	if err := os.Rename(tempName, target); err != nil {
		return "", err
	}
	return target, nil
}

// Empty file created under a fresh UTC stamped name in the folder, the download then replacing it
func reserveDownloadName(folder, name string) (string, error) {
	stamp := time.Now().UTC().Format("20060102T150405Z")
	stem, ext := pypath.Splitext(name)
	for attempt := 0; attempt < 1000; attempt++ {
		candidate := stamp + "-" + name
		if attempt > 0 {
			candidate = fmt.Sprintf("%s-%s-%d%s", stamp, stem, attempt, ext)
		}
		target := filepath.Join(folder, candidate)
		f, err := os.OpenFile(target, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
		if err != nil {
			if os.IsExist(err) {
				continue
			}
			return "", err
		}
		f.Close()
		return target, nil
	}
	return "", fmt.Errorf("no free name for %s", name)
}

// Basename safe on every OS with no separators, control characters or leading and trailing dots
func safeFileName(name string) string {
	parts := regexp.MustCompile(`[\\/]`).Split(name, -1)
	base := parts[len(parts)-1]
	cleaned := strings.Trim(strings.TrimSpace(unsafeNameChars.ReplaceAllString(base, "_")), ".")
	if r := []rune(cleaned); len(r) > maxNameChars {
		cleaned = string(r[len(r)-maxNameChars:])
	}
	if cleaned == "" {
		return "file"
	}
	return cleaned
}

// ---- data use -----------------------------------------------------------------------------------

type dataUseSinkKey struct{}

// DataUseSink collects the data use requirements of connector results during one dispatch
type DataUseSink struct {
	mu       sync.Mutex
	requires map[string]bool
}

// CollectDataUse binds a sink around one dispatch so batch entries union into it
func CollectDataUse(ctx context.Context) (context.Context, *DataUseSink) {
	sink := &DataUseSink{requires: map[string]bool{}}
	return context.WithValue(ctx, dataUseSinkKey{}, sink), sink
}

// Requirements lists the collected requirements sorted
func (s *DataUseSink) Requirements() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]string, 0, len(s.requires))
	for r := range s.requires {
		out = append(out, r)
	}
	sortStrings(out)
	return out
}

// Records requirements into the bound sink, a no op outside an agent dispatch
func recordDataUse(ctx context.Context, requires []string) {
	if ctx == nil {
		return
	}
	sink, ok := ctx.Value(dataUseSinkKey{}).(*DataUseSink)
	if !ok || sink == nil {
		return
	}
	sink.mu.Lock()
	for _, r := range requires {
		sink.requires[r] = true
	}
	sink.mu.Unlock()
}

// ---- dispatch -----------------------------------------------------------------------------------

// Routes one connector call through the gateway and returns the model facing JSON
func dispatchConnectorCall(ctx context.Context, env *Env, call *Call) (string, error) {
	partition := PartitionCalls([]any{map[string]any{"name": call.Name, "arguments": call.Args}})
	for i := range partition.Remote {
		partition.Remote[i].Typed = orderedArgsObject(call.RawArgs, call.Args)
	}
	entries := RunRemote(ctx, env, partition.Remote, call.ToolCallID, call.TurnID, call.TaskID)
	if len(entries) == 0 {
		return toolErrorJSON("Malformed connector tool name; expected connectors__<connector>__{tool}.", nil), nil
	}
	entry := entries[0]
	if du, ok := entry["data_use"].([]string); ok {
		recordDataUse(ctx, du)
	}
	ordered := modelEntry(entry)
	out := pyjson.NewObject()
	for _, key := range ordered.Keys {
		if key == "response" || key == "error" || key == "account" {
			out.Set(key, ordered.Vals[key])
		}
	}
	return JSON(out), nil
}

// Dispatches a connector only batch entry by entry through the normal policy pipeline
func dispatchConnectorBatch(ctx context.Context, d *Dispatcher, call *Call, calls []any, opts DispatchOptions) string {
	if len(calls) > MaxCallsPerDispatch {
		return toolErrorJSON(fmt.Sprintf("too many calls: %d > max %d. Retry with fewer calls per batch.", len(calls), MaxCallsPerDispatch), nil)
	}
	partition := PartitionCalls(calls)
	if len(partition.Local) > 0 {
		first, _ := pyjson.AsObject(calls[0])
		if first == nil {
			first = pyjson.NewObject()
		}
		arguments, has := first.Get("arguments")
		if !has {
			arguments = pyjson.NewObject()
		}
		return toolErrorJSON(localBatchError(first.Vals["name"], arguments, len(calls)), nil)
	}
	typed, _ := NormalizeToolCallEntries(coercePythonToolArgs(ctx, d.Registry, call.Env, call.Name, pythonCallArgs(call)))
	entries := modelEntries(partition.Errors)
	for offset, plan := range partition.Remote {
		if IsInterrupted(ctx) {
			entries = append(entries, modelEntries(FillRemoteFailure(partition.Remote[offset:], "Stopped by the user before this call was made.", "INTERRUPTED"))...)
			break
		}
		inner := *call
		inner.Name = plan.Name
		inner.Args = plan.Arguments
		if len(typed) == len(calls) {
			if raw, err := pyjson.Dumps(typed[plan.Position].Arguments, pyjson.EnsureASCII(false)); err == nil {
				inner.RawArgs = raw
			}
		}
		if call.ToolCallID != "" {
			inner.ToolCallID = fmt.Sprintf("%s#%d", call.ToolCallID, plan.Position)
		}
		entryOpts := opts
		entryOpts.SuppressPostToolCallHook = false
		entryOpts.SkipPreToolCallHook = false
		entryOpts.SkipToolRequestMiddleware = false
		entryOpts.SkipToolExecutionMiddleware = false
		entryOpts.Trace = append([]map[string]any(nil), opts.Trace...)
		payload := d.DispatchWith(ctx, &inner, entryOpts)
		var value any = payload
		if decoded, err := pyjson.Loads(payload); err == nil {
			value = decoded
		}
		entry := pyjson.ObjectOf("index", plan.Position, "name", plan.Name)
		vm, isDict := value.(*pyjson.Object)
		if !isDict {
			vm = pyjson.NewObject()
		}
		if errVal, has := vm.Get("error"); has {
			if em, ok := errVal.(*pyjson.Object); ok {
				entry.Set("error", em)
			} else {
				entry.Set("error", pyjson.ObjectOf("code", "TOOL_ERROR", "message", pyrepr.Str(errVal)))
			}
		} else if isDict {
			entry.Set("response", vm.GetOr("response", vm))
		} else {
			entry.Set("response", value)
		}
		if pyrepr.Truthy(vm.Vals["account"]) {
			entry.Set("account", vm.Vals["account"])
		}
		entries = append(entries, entry)
	}
	return JSON(AssembleResults(len(calls), entries))
}

// ---- search -------------------------------------------------------------------------------------

// The connectors field explaining why hosted tools could not be searched or described
func connectorsUnavailable(env *Env, failure, verb string, names []string) *pyjson.Object {
	if failure == connectorNotSetUp {
		hint := connectorNotSetUpMsg + " Hosted apps (Gmail, Calendar, Notion and more) are then searchable. If the request is better served by the tools above, use those instead."
		if last := guestSetupFailure(env); last != "" {
			hint += " The last attempt failed, so tell the user before retrying: " + last
		}
		field := Dict("status", connectorNotSetUp, "hint", hint)
		if len(names) > 0 {
			field.Set("names", names)
		}
		return field
	}
	hint := fmt.Sprintf("Hosted connector tools could not be %s right now. Do not conclude the app is missing.", verb)
	if failure == connectorSignInExpired {
		hint += " The user must sign in to Nous again."
	}
	field := Dict("status", "unavailable", "reason", failure, "hint", hint)
	if len(names) > 0 {
		field.Set("names", names)
	}
	return field
}

func connectorEntry(name, connector, slug string, schema map[string]any) CatalogEntry {
	description := asStr(schema["description"])
	parameters := ModelParameters(name, asMap(schema["input_schema"]))
	toolDef := map[string]any{"type": "function", "function": map[string]any{"name": name, "description": description, "parameters": parameters}}
	text := connector + " " + strings.ReplaceAll(slug, "_", " ") + " " + description
	return CatalogEntry{Name: name, Description: description, Schema: toolDef, Source: "connectors", SourceName: connector, Tokens: catalogTokenize(text)}
}

// Hosted catalog entries per query from the gateway search leg
func connectorEntriesByGroup(ctx context.Context, env *Env, queries []string, call *Call) ([][]CatalogEntry, string) {
	perQuery := make([][]CatalogEntry, len(queries))
	wireQueries := make([]map[string]any, 0, len(queries))
	for _, q := range queries {
		wireQueries = append(wireQueries, map[string]any{"use_case": q})
	}
	leg := ConnectorSearchHits(ctx, env, wireQueries, callRefOf(call))
	if leg.Failure != "" {
		return perQuery, leg.Failure
	}
	hits := leg.Payload
	schemas := asMap(hits["schemas"])
	groups, groupsOK := hits["results"].([]any)
	if schemas == nil || !groupsOK {
		if len(hits) > 0 {
			return perQuery, connectorUnreachable
		}
		return perQuery, ""
	}
	if len(groups) > len(queries) {
		groups = groups[:len(queries)]
	}
	for position, raw := range groups {
		group := asMap(raw)
		if group == nil {
			continue
		}
		if echoed, ok := group["use_case"].(string); ok && echoed != "" && echoed != queries[position] {
			continue
		}
		type pick struct {
			slug  string
			entry CatalogEntry
		}
		picked := map[string]pick{}
		var order []string
		slugs := asList(group["tools"])
		if wire, ok := group["tools"].([]string); ok {
			for _, slug := range wire {
				slugs = append(slugs, slug)
			}
		}
		for _, rawSlug := range slugs {
			slug := asStr(rawSlug)
			schema := asMap(schemas[slug])
			if schema == nil || asStr(schema["connector"]) == "" {
				continue
			}
			connector := strings.ToLower(asStr(schema["connector"]))
			name := FormatConnectorName(connector, slug)
			if prior, ok := picked[name]; ok {
				if prior.slug != slug {
					reaches := VendorSlugCandidates(connector, strings.SplitN(name, "__", 3)[2])[0]
					envLog(env).Warn("connector vendor slugs both compose to one name", "connector", connector, "first", prior.slug, "second", slug, "name", name, "reaches", reaches)
					if slug != reaches {
						continue
					}
				} else {
					continue
				}
			} else {
				order = append(order, name)
			}
			picked[name] = pick{slug: slug, entry: connectorEntry(name, asStr(schema["connector"]), slug, schema)}
		}
		for _, name := range order {
			perQuery[position] = append(perQuery[position], picked[name].entry)
		}
	}
	return perQuery, ""
}

// Model schemas for composed connector names when manage_connections is in scope
func connectorRemoteSchemasFor(ctx context.Context, env *Env, names []string, currentToolDefs []map[string]any, call *Call) (map[string]map[string]any, string) {
	var connectorNames []string
	for _, n := range names {
		if IsConnectorName(n) {
			connectorNames = append(connectorNames, n)
		}
	}
	if len(connectorNames) == 0 || !connectionsInScope(currentToolDefs) {
		return map[string]map[string]any{}, ""
	}
	leg := ConnectorDescribe(ctx, env, connectorNames, callRefOf(call))
	if leg.Failure != "" {
		return map[string]map[string]any{}, leg.Failure
	}
	tools := asMap(leg.Payload["tools"])
	if tools == nil {
		if len(leg.Payload) > 0 {
			return map[string]map[string]any{}, connectorUnreachable
		}
		return map[string]map[string]any{}, ""
	}
	out := map[string]map[string]any{}
	for name, v := range tools {
		if m := asMap(v); m != nil {
			out[name] = m
		}
	}
	return out, ""
}

func callRefOf(call *Call) *CallRef {
	if call == nil {
		return nil
	}
	return &CallRef{TurnID: call.TurnID, ToolCallID: call.ToolCallID}
}
