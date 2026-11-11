package gateway

import (
	"fmt"
	"log/slog"
	"maps"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pypath"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
)

const maxControlRequestBytes = 64 * 1024

// Verb handler answering a request with its params, an empty map when the request carries none
type ControlVerbHandler func(params map[string]any) (any, error)

// Private verb handler answering only an authenticated same user peer
type ControlPrivateHandler func(params map[string]any, peerSubject string) (any, error)

// Gateway owned control listener answering identify, status and the verbs the gateway registers
type GatewayControlServer struct {
	home            string
	TicketStore     *TicketStore
	mu              sync.Mutex
	privateHandlers map[string]ControlPrivateHandler
	handlers        map[string]ControlVerbHandler
	listener        net.Listener
	pipeServer      *NativeControlServer
	conns           sync.WaitGroup
	bindPath        string
	pointerFile     string
	fallbackRoot    string
	fileIdentities  map[string]os.FileInfo
}

// Server for home, the process home when empty, answering the default verbs plus the given ones
func NewGatewayControlServer(home string, verbHandlers map[string]ControlVerbHandler) (*GatewayControlServer, error) {
	if home == "" {
		home = processHermesHome()
	}
	resolved, err := pypath.Resolve(home)
	if err != nil {
		return nil, err
	}
	handlers := map[string]ControlVerbHandler{
		"identify": func(map[string]any) (any, error) { return BuildIdentifyPayload(), nil },
		"status":   func(map[string]any) (any, error) { return BuildStatusPayload(), nil },
	}
	maps.Copy(handlers, verbHandlers)
	return &GatewayControlServer{home: resolved, handlers: handlers,
		privateHandlers: map[string]ControlPrivateHandler{}, fileIdentities: map[string]os.FileInfo{}}, nil
}

// Resolved home the listener serves
func (s *GatewayControlServer) Home() string { return s.home }

// Path the listener bound, empty before a POSIX start
func (s *GatewayControlServer) BindPath() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.bindPath
}

// Adds private verbs only an authenticated same user peer reaches
func (s *GatewayControlServer) AddPrivateHandlers(handlers map[string]ControlPrivateHandler) {
	s.mu.Lock()
	maps.Copy(s.privateHandlers, handlers)
	s.mu.Unlock()
}

// Private handler of a verb, false when none is registered
func (s *GatewayControlServer) privateHandler(verb string) (ControlPrivateHandler, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	handler, ok := s.privateHandlers[verb]
	return handler, ok
}

// Binds and starts serving, false after logging why it could not
func (s *GatewayControlServer) Start() bool {
	if err := s.startPlatform(); err != nil {
		s.Stop()
		slog.Warn(fmt.Sprintf("Gateway control socket failed to start: %s", pyrepr.ExceptionMessage(err)))
		return false
	}
	return true
}

// Stops serving, revokes outstanding tickets and removes the socket and pointer files
func (s *GatewayControlServer) Stop() {
	if s.TicketStore != nil {
		s.TicketStore.Revoke()
	}
	s.mu.Lock()
	listener, pipe := s.listener, s.pipeServer
	s.listener, s.pipeServer = nil, nil
	s.mu.Unlock()
	if listener != nil {
		_ = listener.Close()
		s.conns.Wait()
	}
	if pipe != nil {
		_ = pipe.Close()
	}
	s.CleanupFiles()
}

// Removes the socket and pointer files this server still owns, leaving a replacement alone
func (s *GatewayControlServer) CleanupFiles() {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, path := range []string{s.bindPath, s.pointerFile} {
		if path == "" {
			continue
		}
		owned, known := s.fileIdentities[path]
		info, err := controlLstat(path)
		if err == nil && known && os.SameFile(owned, info) {
			_ = os.Remove(path)
		}
	}
	clear(s.fileIdentities)
	if s.fallbackRoot != "" {
		if h, err := controlHomeHash(s.home); err == nil {
			if os.Remove(filepath.Join(s.fallbackRoot, "icebath-gw-"+h)) == nil {
				_ = os.Remove(s.fallbackRoot)
			}
		}
		s.fallbackRoot = ""
	}
}

// Records the identity of a file this server created so cleanup never unlinks a replacement
func (s *GatewayControlServer) noteFileIdentity(path string) error {
	info, err := controlLstat(path)
	if err != nil {
		return err
	}
	s.fileIdentities[path] = info
	return nil
}

// One JSON request line to one JSON response line, never failing
func (s *GatewayControlServer) HandleRequestLine(raw []byte, peerSubject string) []byte {
	var requestID any
	response, err := s.answer(raw, peerSubject, &requestID)
	if err != nil {
		response = pyjson.ObjectOf("ok", false, "error", controlErrorText(err), "protocol", int64(ControlProtocolVersion))
	}
	if requestID != nil {
		response.Set("id", requestID)
	}
	encoded, err := pyjson.Dumps(response, pyjson.Default(func(v any) (any, error) { return pyrepr.Str(v), nil }))
	if err != nil {
		encoded = `{"ok": false, "error": "response serialization failed"}`
	}
	if len(encoded) > maxControlResponseBytes {
		tooLarge := pyjson.ObjectOf("ok", false, "error", "response_too_large", "code", "response_too_large",
			"limit", int64(maxControlResponseBytes), "protocol", int64(ControlProtocolVersion))
		if controlIDEchoable(requestID) {
			tooLarge.Set("id", requestID)
		}
		encoded, _ = pyjson.Dumps(tooLarge)
	}
	return append([]byte(encoded), '\n')
}

// Whether a request id may ride on the oversized answer, an int or str of at most 128 characters
func controlIDEchoable(id any) bool {
	switch id.(type) {
	case string, bool, int64:
		return len([]rune(pyrepr.Str(id))) <= 128
	}
	_, isInt := pyNumber(id)
	_, isFloat := id.(float64)
	return isInt && !isFloat && len([]rune(pyrepr.Str(id))) <= 128
}

// Python type name and message of a handler failure
func controlErrorText(err error) string {
	name := pyErrorKind(err)
	if name == "" {
		name = pyrepr.ExceptionName(err)
	}
	return name + ": " + pyrepr.ExceptionMessage(err)
}

// Dispatches one decoded request to its verb, the request id stored for the response
func (s *GatewayControlServer) answer(raw []byte, peerSubject string, requestID *any) (*pyjson.Object, error) {
	if !utf8.Valid(raw) {
		return nil, &PyError{Kind: "UnicodeDecodeError", Msg: "'utf-8' codec can't decode the control request"}
	}
	decoded, err := pyjson.Loads(string(raw))
	if err != nil {
		return nil, &PyError{Kind: "JSONDecodeError", Msg: err.Error()}
	}
	request, ok := decoded.(*pyjson.Object)
	if !ok {
		return nil, &PyError{Kind: "ValueError", Msg: "request must be a JSON object"}
	}
	*requestID, _ = request.Get("id")
	verbValue, _ := request.Get("verb")
	verb, verbIsStr := verbValue.(string)
	var handler ControlVerbHandler
	if verbIsStr {
		handler = s.handlers[verb]
	}
	if private, ok := s.privateHandler(verb); verbIsStr && ok {
		protocol, _ := request.Get("protocol")
		paramsValue, _ := request.Get("params")
		params, paramsIsDict := paramsValue.(*pyjson.Object)
		if peerSubject == "" || !pyEq(protocol, int64(1)) || controlExtraKeys(request, "protocol", "verb", "id", "params") || !paramsIsDict {
			return nil, &PyError{Kind: "PermissionError", Msg: "authenticated private request required"}
		}
		result, err := private(controlPlainParams(params), peerSubject)
		if err != nil {
			return nil, err
		}
		return pyjson.ObjectOf("ok", true, "protocol", int64(ControlProtocolVersion), "result", result), nil
	}
	if verbIsStr && verb == "session-ticket" {
		result, err := s.sessionTicket(request, peerSubject)
		if err != nil {
			return nil, err
		}
		return pyjson.ObjectOf("ok", true, "protocol", int64(ControlProtocolVersion), "result", result), nil
	}
	if handler == nil {
		supported := slices.Sorted(maps.Keys(s.handlers))
		names := make([]any, len(supported))
		for i, name := range supported {
			names[i] = name
		}
		return pyjson.ObjectOf("ok", false, "error", "unknown verb: "+pyrepr.Repr(verbValue),
			"protocol", int64(ControlProtocolVersion), "supported_verbs", names), nil
	}
	params := map[string]any{}
	if value, _ := request.Get("params"); value != nil {
		if dict, isDict := value.(*pyjson.Object); isDict {
			params = controlPlainParams(dict)
		}
	}
	result, err := handler(params)
	if err != nil {
		return nil, err
	}
	return pyjson.ObjectOf("ok", true, "protocol", int64(ControlProtocolVersion), "result", result), nil
}

// Plain map form of decoded request params
func controlPlainParams(params *pyjson.Object) map[string]any {
	plain, _ := pyjson.Plain(params).(map[string]any)
	if plain == nil {
		plain = map[string]any{}
	}
	return plain
}

// Whether the object carries a key outside the allowed set
func controlExtraKeys(obj *pyjson.Object, allowed ...string) bool {
	for _, key := range obj.Keys {
		if !slices.Contains(allowed, key) {
			return true
		}
	}
	return false
}

// Whether the object carries every key
func controlHasKeys(obj *pyjson.Object, keys ...string) bool {
	for _, key := range keys {
		if _, ok := obj.Get(key); !ok {
			return false
		}
	}
	return true
}

// Mints a profile bound attach ticket for an authenticated same user peer
func (s *GatewayControlServer) sessionTicket(request *pyjson.Object, peerSubject string) (map[string]any, error) {
	store := s.TicketStore
	if peerSubject == "" || store == nil {
		return nil, &PyError{Kind: "PermissionError", Msg: "authenticated runtime bootstrap unavailable"}
	}
	protocol, _ := request.Get("protocol")
	if controlExtraKeys(request, "protocol", "verb", "id", "params") || !pyEq(protocol, int64(1)) {
		return nil, &PyError{Kind: "PermissionError", Msg: "invalid bootstrap envelope"}
	}
	paramsValue, _ := request.Get("params")
	params, isDict := paramsValue.(*pyjson.Object)
	invalidParams := &PyError{Kind: "PermissionError", Msg: "invalid bootstrap parameters"}
	if !isDict || !controlHasKeys(params, "profile_id", "instance_id", "purpose") ||
		controlExtraKeys(params, "profile_id", "instance_id", "purpose", "scope", "subject", "restart_wait") {
		return nil, invalidParams
	}
	if restartWait, present := params.Get("restart_wait"); present && !pyEq(restartWait, "refuse") {
		return nil, invalidParams
	}
	instanceID, _ := params.Get("instance_id")
	if !pyEq(instanceID, store.InstanceID) {
		return nil, &PyError{Kind: "PermissionError", Msg: "stale runtime instance"}
	}
	subject := peerSubject
	if named, present := params.Get("subject"); present {
		text, isStr := named.(string)
		if !isStr || text == "" || len([]rune(text)) > 256 || !structuredLoginSubject(text) {
			return nil, &PyError{Kind: "PermissionError", Msg: "invalid bootstrap subject"}
		}
		subject = text
	} else if len([]rune(subject)) > 256 {
		return nil, &PyError{Kind: "PermissionError", Msg: "invalid bootstrap subject"}
	}
	profileValue, _ := params.Get("profile_id")
	purposeValue, _ := params.Get("purpose")
	scopeValue, scoped := params.Get("scope")
	if !scoped {
		scopeValue = "profile"
	}
	binding := make([]string, 3)
	for i, value := range []any{profileValue, purposeValue, scopeValue} {
		text, err := controlBindingText(value)
		if err != nil {
			return nil, err
		}
		if text == nil || *text == "" {
			return nil, &PyError{Kind: "PermissionError", Msg: "bootstrap binding rejected"}
		}
		binding[i] = *text
	}
	_, refuseRestartWait := params.Get("restart_wait")
	ticket, err := store.Mint(binding[0], subject, binding[1], binding[2], refuseRestartWait)
	if err != nil {
		return nil, err
	}
	return map[string]any{"ticket": ticket, "expires_in_seconds": int64(30), "instance_id": store.InstanceID,
		"profile_id": profileValue, "runtime_protocol": int64(1)}, nil
}

// String of a ticket binding value, nil for a hashable non string, TypeError for an unhashable one
func controlBindingText(value any) (*string, error) {
	switch v := value.(type) {
	case string:
		return &v, nil
	case *pyjson.Object, map[string]any:
		return nil, &PyError{Kind: "TypeError", Msg: "unhashable type: 'dict'"}
	case []any:
		return nil, &PyError{Kind: "TypeError", Msg: "unhashable type: 'list'"}
	}
	return nil, nil
}

// Supervisor kind of this process from its own launch environment
func detectSupervisor() string {
	if os.Getenv("INVOCATION_ID") != "" {
		return "systemd"
	}
	if runtime.GOOS == "darwin" {
		environ := map[string]string{}
		for _, pair := range os.Environ() {
			if key, value, ok := strings.Cut(pair, "="); ok {
				environ[key] = value
			}
		}
		if LaunchdJobLabel(environ) != "" || environ["LAUNCHD_SOCKET"] != "" {
			return "launchd"
		}
	}
	if os.Getenv("ICEBATH_DESKTOP_MANAGED") != "" {
		return "desktop"
	}
	if slices.Contains(os.Args, "--external-supervisor") {
		return "external"
	}
	return "manual"
}

// Supervisor kind of this process, the launch environment answer
func DetectSupervisor() string { return detectSupervisor() }

// Default identify answer built from the pid record, code identity and served profiles
func BuildIdentifyPayload() *pyjson.Object {
	record := statusBuildPidRecord()
	supervisor := detectSupervisor()
	payload := pyjson.ObjectOf("protocol", int64(ControlProtocolVersion))
	for _, key := range []string{"kind", "pid", "start_time", "hermes_home"} {
		payload.Set(key, recGet(record, key))
	}
	homeValue := recGet(record, "hermes_home")
	label := ""
	if pyrepr.Truthy(homeValue) {
		label = pyrepr.Str(homeValue)
	}
	payload.Set("profile", statusProfileLabelForHome(label))
	payload.Set("supervisor", supervisor)
	payload.Set("launch_supervisor", supervisor)
	for _, pair := range codeIdentityFields() {
		payload.Set(pair[0].(string), pair[1])
	}
	func() {
		defer func() { _ = recover() }()
		if status := ReadRuntimeStatus(""); status != nil {
			if served, ok := recGet(status, "served_profiles").([]any); ok && len(served) > 0 {
				payload.Set("served_profiles", served)
			}
		}
	}()
	return payload
}

// Default status answer, the live runtime status stamped with the answering process
func BuildStatusPayload() *pyjson.Object {
	payload := pyjson.NewObject()
	if status := ReadRuntimeStatus(""); status != nil {
		for _, key := range status.Keys {
			value, _ := status.Get(key)
			payload.Set(key, value)
		}
	}
	payload.Set("protocol", int64(ControlProtocolVersion))
	payload.Set("answered_at", float64(time.Now().UnixNano())/1e9)
	payload.Set("answering_pid", int64(os.Getpid()))
	return payload
}
