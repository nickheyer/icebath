package selfupdate

import (
	"context"
	"fmt"
	"io"
	"os"
	"strconv"

	"github.com/nickheyer/icebath/pkg/gateway"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
	"github.com/nickheyer/icebath/pkg/pm"
)

// Hidden entry relaunching the manual backends a historical updater paused
const UpdateServeResumeEntry = "update-serve-resume"

// Respawn seams of the serve resume child, the host filter and detached respawn
var (
	serveResumeFilter   = gateway.FilterDashboardRespawnCandidates
	serveResumeRespawn  = gateway.RespawnDashboardProcesses
	serveResumeActivate = pm.ActivateDependencies
)

// Command line running the serve resume child of a checkout binary on a request and result file pair
func updateServeResumeArgv(binary, contextPath, resultPath string) []string {
	return []string{binary, UpdateServeResumeEntry, contextPath, resultPath}
}

// Python KeyError text of a missing mapping key
func pyKeyErrorText(key string) string { return pyrepr.Repr(key) }

// Value of key in a JSON object, the AttributeError text when the holder is not an object
func pyDictGet(holder any, key string) (any, error) {
	obj, ok := holder.(*pyjson.Object)
	if !ok {
		return nil, fmt.Errorf("'%s' object has no attribute 'get'", pyrepr.TypeName(holder))
	}
	return obj.GetOr(key, nil), nil
}

// Value of key in a JSON object, the KeyError or TypeError text when it is absent or not subscriptable
func pyDictItem(holder any, key string) (any, error) {
	obj, ok := holder.(*pyjson.Object)
	if !ok {
		if _, isList := holder.([]any); isList {
			return nil, fmt.Errorf("list indices must be integers or slices, not str")
		}
		if _, isText := holder.(string); isText {
			return nil, fmt.Errorf("string indices must be integers, not 'str'")
		}
		return nil, fmt.Errorf("'%s' object is not subscriptable", pyrepr.TypeName(holder))
	}
	value, present := obj.Get(key)
	if !present {
		return nil, fmt.Errorf("%s", pyKeyErrorText(key))
	}
	return value, nil
}

// Reads a request file as utf-8-sig JSON
func readRequestFile(path string) (any, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	text, err := pystr.DecodeUTF8Sig(raw)
	if err != nil {
		return nil, err
	}
	return pyjson.Loads(text)
}

// Relaunches the paused manual serves of a request and acknowledges the token, the child exit status
func UpdateServeResumeMain(ctx context.Context, contextPath, resultPath string, stdout, stderr io.Writer) int {
	handled := false
	defer func() {
		body := pyjson.ObjectOf("serves_handled", handled)
		text, _ := pyjson.Dumps(body)
		_ = os.WriteFile(resultPath, []byte(text), 0o666)
	}()
	code, err := serveResume(ctx, contextPath, stdout, stderr, &handled)
	if err != nil {
		fmt.Fprintf(stderr, "Stopped serve recovery failed: %s\n", launchErrorText(err))
		return 1
	}
	return code
}

// Rebuilds and respawns the serve and dashboard commands of a pending stopped serve token
func serveResume(ctx context.Context, contextPath string, stdout, stderr io.Writer, handled *bool) (int, error) {
	request, err := readRequestFile(contextPath)
	if err != nil {
		return 0, err
	}
	rootValue, err := pyDictItem(request, "root")
	if err != nil {
		return 0, err
	}
	root := pyrepr.Str(rootValue)
	token, err := pyDictItem(request, "stopped_serves")
	if err != nil {
		return 0, err
	}
	pending, err := pyDictGet(token, "pending")
	if err != nil {
		return 0, err
	}
	if !pyrepr.Truthy(pending) {
		*handled = true
		return 0, nil
	}
	if err := serveResumeActivate(ctx, root); err != nil {
		return 0, err
	}
	executable, err := runningExecutable()
	if err != nil {
		return 0, err
	}
	var candidates []gateway.RespawnCandidate
	skipped := 0
	rawEntries, err := pyDictGet(token, "entries")
	if err != nil {
		return 0, err
	}
	if !pyrepr.Truthy(rawEntries) {
		rawEntries = nil
	}
	entries, err := pyIterable(rawEntries)
	if err != nil {
		return 0, err
	}
	for _, entry := range entries {
		port, err := pyDictGet(entry, "port")
		if err != nil {
			return 0, err
		}
		purpose, _ := pyDictGet(entry, "purpose")
		number, isInt := pyIntValue(port)
		if (purpose != "serve" && purpose != "dashboard") || !isInt || number <= 0 || number > 65535 {
			skipped++
			continue
		}
		command := []string{executable}
		profile, _ := pyDictGet(entry, "profile")
		if pyrepr.Truthy(profile) && profile != "default" {
			command = append(command, "--profile", pyrepr.Str(profile))
		}
		command = append(command, purpose.(string))
		if host, _ := pyDictGet(entry, "host"); pyrepr.Truthy(host) {
			command = append(command, "--host", pyrepr.Str(host))
		}
		command = append(command, "--port", strconv.FormatInt(number, 10))
		pidValue, _ := pyDictGet(entry, "pid")
		pid, _ := pyIntValue(pidValue)
		owner := ""
		if homeValue, _ := pyDictGet(entry, "hermes_home"); pyrepr.Truthy(homeValue) {
			owner = pyrepr.Str(homeValue)
		}
		candidates = append(candidates, gateway.RespawnCandidate{Pid: int(pid), Argv: command, Home: owner})
	}
	var ownHome *string
	if homeValue, _ := pyDictGet(request, "home"); homeValue != nil {
		text := pyrepr.Str(homeValue)
		ownHome = &text
	}
	commands := serveResumeFilter(ctx, candidates, ownHome)
	skipped += len(candidates) - len(commands)
	var failed [][]string
	if len(commands) > 0 {
		failed = serveResumeRespawn(ctx, stdout, commands)
	}
	*handled = true
	if skipped > 0 || len(failed) > 0 {
		fmt.Fprintln(stderr, "Some stopped backends could not be relaunched; restart them manually.")
		return 1, nil
	}
	return 0, nil
}

// Items of a JSON value iterated the way a Python for loop does, a TypeError for a non iterable
func pyIterable(value any) ([]any, error) {
	switch v := value.(type) {
	case nil:
		return nil, nil
	case []any:
		return v, nil
	case string:
		out := make([]any, 0, len(v))
		for _, r := range v {
			out = append(out, string(r))
		}
		return out, nil
	case *pyjson.Object:
		out := make([]any, 0, v.Len())
		for _, key := range v.Keys {
			out = append(out, key)
		}
		return out, nil
	}
	return nil, fmt.Errorf("'%s' object is not iterable", pyrepr.TypeName(value))
}
