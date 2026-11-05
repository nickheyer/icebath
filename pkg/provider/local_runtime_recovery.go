package provider

import (
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"math/big"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strconv"
	"strings"
	"time"

	"github.com/nickheyer/icebath/pkg/tools"
)

// Live process incarnation with psutil semantics where destructive calls refuse a reused pid
type localRuntimeProc interface {
	Pid() int
	CreateTime() (float64, error)
	IsRunning() bool
	Exe() (string, error)
	PPID() (int, error)
	Parent() (localRuntimeProc, error)
	Cmdline() ([]string, error)
	Children(recursive bool) ([]localRuntimeProc, error)
	Terminate() error
	Kill() error
	Wait(timeout time.Duration) error
}

// Seams the recovery tests replace with fake processes, clocks and paths
var (
	lrOpenProcess   = openPSProcess
	lrPidExists     = psPidExists
	lrStartTime     = tools.ProcessStartTime
	lrStatePath     = func() string { return localRuntimeStatePath() }
	lrModelsDir     = func() string { return localModelsDir() }
	lrTerminateTree = func(proc localRuntimeProc, verifiedRoot bool) error {
		return terminateLlamaServerTree(proc, verifiedRoot)
	}
	lrRecordedProcess       = recordedProcess
	lrOwnerIsDead           = ownerIsDead
	lrLegacyRecordedProcess = legacyRecordedProcess
	lrOSName                = func() string { return pyOSName(runtime.GOOS) }
)

// Python os.name for a Go GOOS
func pyOSName(goos string) string {
	if goos == "windows" {
		return "nt"
	}
	return "posix"
}

// Recorded server state from server.json, empty when unreadable or not an object
func readLocalRuntimeState() map[string]any {
	state, err := lrReadStateFile()
	if err != nil {
		return map[string]any{}
	}
	if m, ok := state.(map[string]any); ok {
		return m
	}
	return map[string]any{}
}

// server.json decoded like Python json.loads of utf-8-sig text, os.ErrNotExist when the file is missing
func lrReadStateFile() (any, error) {
	data, err := os.ReadFile(lrStatePath())
	if err != nil {
		return nil, err
	}
	text := strings.TrimPrefix(string(data), "\xef\xbb\xbf")
	decoded, ok := moaJSONLoads(text)
	if !ok {
		return nil, errors.New("server.json is not valid JSON")
	}
	return decoded, nil
}

var lrModernFields = []string{"create_time", "executable", "owner_pid", "owner_create_time"}

// Whether the record carries any birth identity field of the modern format
func isModernLocalRuntimeState(state map[string]any) bool {
	for _, key := range lrModernFields {
		if _, ok := state[key]; ok {
			return true
		}
	}
	return false
}

// Positive Python int, a decoded integer too large for int counts as valid
func lrValidPid(value any) bool {
	switch v := value.(type) {
	case int:
		return v > 0
	case json.Number:
		return !strings.HasPrefix(v.String(), "-") && strings.Trim(v.String(), "0") != ""
	}
	return false
}

// Positive finite int or float, overflow reports an integer too large for a float like math.isfinite
func lrValidBirth(value any) (valid, overflow bool) {
	switch v := value.(type) {
	case int:
		return v > 0, false
	case float64:
		return !math.IsNaN(v) && !math.IsInf(v, 0) && v > 0, false
	case json.Number:
		f, err := strconv.ParseFloat(v.String(), 64)
		if err != nil || math.IsInf(f, 0) {
			return false, true
		}
		return f > 0, false
	}
	return false, false
}

// Exact numeric value of a decoded JSON number or bool, nil for other types
func lrExact(value any) *big.Float {
	switch v := value.(type) {
	case bool:
		if v {
			return big.NewFloat(1)
		}
		return big.NewFloat(0)
	case int:
		return new(big.Float).SetInt64(int64(v))
	case float64:
		if math.IsNaN(v) {
			return nil
		}
		return big.NewFloat(v)
	case json.Number:
		n, ok := new(big.Int).SetString(v.String(), 10)
		if !ok {
			return nil
		}
		return new(big.Float).SetInt(n)
	}
	return nil
}

// Python a == b for a process float against a recorded JSON value
func lrNumEqual(a float64, b any) bool {
	x, y := lrExact(a), lrExact(b)
	return x != nil && y != nil && x.Cmp(y) == 0
}

// Python a > b for two recorded numbers
func lrNumGreater(a, b any) bool {
	x, y := lrExact(a), lrExact(b)
	return x != nil && y != nil && x.Cmp(y) > 0
}

// Python abs(int(current) - int(recorded)) within the start time drift tolerance
func lrFingerprintsMatch(recorded any, current int64) bool {
	exact := lrExact(recorded)
	if exact == nil {
		return false
	}
	rec, _ := exact.Int(nil)
	diff := new(big.Int).Sub(big.NewInt(current), rec)
	return diff.Abs(diff).Cmp(big.NewInt(200)) <= 0
}

var errLRRecordInvalid = errors.New("recorded identity is missing or malformed")

// Same incarnation by boot relative start time, or create time for records that predate it
func lrSameBirth(proc localRuntimeProc, state map[string]any, prefix string) (bool, error) {
	recorded := state[prefix+"start_time"]
	valid, overflow := lrValidBirth(recorded)
	if overflow {
		return false, errLRRecordInvalid
	}
	if !valid {
		created, err := proc.CreateTime()
		if err != nil {
			return false, err
		}
		want, ok := state[prefix+"create_time"]
		if !ok {
			return false, errLRRecordInvalid
		}
		return lrNumEqual(created, want), nil
	}
	current, ok := lrStartTime(proc.Pid())
	return ok && lrFingerprintsMatch(recorded, current), nil
}

// Live process matching a modern record's full incarnation, never just its reusable pid
func recordedProcess(state map[string]any) localRuntimeProc {
	pid, okPid := state["pid"]
	created, okCreated := state["create_time"]
	ownerPid, okOwner := state["owner_pid"]
	ownerCreated, okOwnerCreated := state["owner_create_time"]
	exe, okExe := state["executable"]
	if !okPid || !okCreated || !okOwner || !okOwnerCreated || !okExe {
		return nil
	}
	createdValid, o1 := lrValidBirth(created)
	ownerValid, o2 := lrValidBirth(ownerCreated)
	if o1 || o2 {
		return nil
	}
	exeText, isStr := exe.(string)
	if !lrValidPid(pid) || !createdValid || !lrValidPid(ownerPid) || !ownerValid ||
		lrNumGreater(ownerCreated, created) || !isStr || exeText == "" {
		return nil
	}
	pidInt, okInt := pid.(int)
	ownerInt, okOwnerInt := ownerPid.(int)
	if !okInt {
		return nil
	}
	proc, err := lrOpenProcess(pidInt)
	if err != nil || !proc.IsRunning() {
		return nil
	}
	same, err := lrSameBirth(proc, state, "")
	if err != nil || !same {
		return nil
	}
	actual, err := proc.Exe()
	if err != nil || !lrPathEqual(actual, exeText) {
		return nil
	}
	if lrOSName() == "nt" {
		ppid, err := proc.PPID()
		if err != nil || !okOwnerInt || ppid != ownerInt {
			return nil
		}
	}
	parent, err := proc.Parent()
	if err != nil {
		return nil
	}
	if parent != nil {
		if okOwnerInt && parent.Pid() == ownerInt {
			same, err := lrSameBirth(parent, state, "owner_")
			if err != nil || !same {
				return nil
			}
		} else if lrOSName() == "nt" || !lrOwnerIsDead(state) {
			return nil
		}
	}
	return proc
}

// Whether the recorded owner provably exited, false whenever the record cannot prove it
func ownerIsDead(state map[string]any) bool {
	pid, okPid := state["owner_pid"]
	created, okCreated := state["owner_create_time"]
	if !okPid || !okCreated {
		return false
	}
	valid, overflow := lrValidBirth(created)
	if overflow || !lrValidPid(pid) || !valid {
		return false
	}
	pidInt, ok := pid.(int)
	if !ok {
		return true
	}
	owner, err := lrOpenProcess(pidInt)
	if err != nil {
		return psIsNoSuchProcess(err)
	}
	if !owner.IsRunning() {
		return true
	}
	startValid, startOverflow := lrValidBirth(state["owner_start_time"])
	if startOverflow {
		return false
	}
	if startValid {
		same, err := lrSameBirth(owner, state, "owner_")
		if err != nil {
			return psIsNoSuchProcess(err)
		}
		return !same
	}
	ownerCreated, err := owner.CreateTime()
	if err != nil {
		return psIsNoSuchProcess(err)
	}
	return lrNumGreater(ownerCreated, created)
}

// Python AttributeError urlsplit raises for a truthy base_url that is not a string
type lrAttributeError struct{ typeName string }

func (e *lrAttributeError) Error() string {
	return fmt.Sprintf("'%s' object has no attribute 'decode'", e.typeName)
}

// Legacy record matched to the managed binary and its exact launch arguments, err where Python propagates
func legacyRecordedProcess(state map[string]any) (localRuntimeProc, error) {
	if isModernLocalRuntimeState(state) {
		return nil, nil
	}
	pid, ok := state["pid"]
	if !ok || !lrValidPid(pid) {
		return nil, nil
	}
	pidInt, ok := pid.(int)
	if !ok {
		return nil, nil
	}
	proc, err := lrOpenProcess(pidInt)
	if err != nil {
		return nil, nil
	}
	statePath := lrStatePath()
	root := filepath.Dir(statePath)
	exe, err := proc.Exe()
	if err != nil {
		return nil, nil
	}
	if _, ok := lrRelativeParts(exe, root); !ok {
		return nil, nil
	}
	if name := strings.ToLower(lrPathName(exe)); name != "llama-server" && name != "llama-server.exe" {
		return nil, nil
	}
	created, err := proc.CreateTime()
	if err != nil {
		return nil, nil
	}
	info, err := os.Stat(statePath)
	if err != nil {
		return nil, nil
	}
	if created > float64(info.ModTime().UnixNano())/1e9 {
		return nil, nil
	}
	rawURL, ok := state["base_url"]
	if !ok {
		return nil, nil
	}
	baseURL, isStr := rawURL.(string)
	if !isStr {
		if pyTruthy(rawURL) {
			return nil, &lrAttributeError{typeName: lrPyTypeName(rawURL)}
		}
		return nil, nil
	}
	parts, err := pyURLParse(baseURL, "")
	if err != nil || parts.scheme != "http" {
		return nil, nil
	}
	if hostname, _, _ := pyNetlocHostPort(parts.netloc); hostname != "127.0.0.1" {
		return nil, nil
	}
	port, hasPort, err := pyURLPort(baseURL)
	if err != nil || !hasPort || port == 0 {
		return nil, nil
	}
	apiKey, ok := state["api_key"]
	if !ok || !pyTruthy(apiKey) {
		return nil, nil
	}
	argv, err := proc.Cmdline()
	if err != nil {
		return nil, nil
	}
	value := func(flag string) (string, bool) {
		index, count := -1, 0
		for i, arg := range argv {
			if arg == flag {
				count++
				if index < 0 {
					index = i
				}
			}
		}
		if count != 1 || index+1 >= len(argv) {
			return "", false
		}
		return argv[index+1], true
	}
	host, hostOK := value("--host")
	portArg, portOK := value("--port")
	key, keyOK := value("--api-key")
	apiKeyText, apiKeyIsStr := apiKey.(string)
	if !hostOK || host != "127.0.0.1" || !portOK || portArg != strconv.Itoa(port) || !keyOK || !apiKeyIsStr || key != apiKeyText {
		return nil, nil
	}
	preset, presetOK := value("--models-preset")
	directory, dirOK := value("--models-dir")
	presetMatch := presetOK && preset != "" && lrPathEqual(preset, filepath.Join(root, "presets.ini")) && !dirOK
	dirMatch := dirOK && directory != "" && lrPathEqual(directory, lrModelsDir()) && !presetOK
	if !presetMatch && !dirMatch {
		return nil, nil
	}
	return proc, nil
}

// Python type name used in the AttributeError text of a decoded JSON value
func lrPyTypeName(v any) string {
	switch v.(type) {
	case bool:
		return "bool"
	case int, json.Number:
		return "int"
	case float64:
		return "float"
	case []any:
		return "list"
	case map[string]any:
		return "dict"
	}
	return "NoneType"
}

// Windows legacy stop recovery of a parentless router three levels under the runtimes root
func legacyOrphanProcess(state map[string]any) (localRuntimeProc, error) {
	if lrOSName() != "nt" {
		return nil, nil
	}
	proc, err := lrLegacyRecordedProcess(state)
	if err != nil || proc == nil {
		return nil, err
	}
	exe, err := proc.Exe()
	if err != nil {
		return nil, nil
	}
	relative, ok := lrRelativeParts(exe, filepath.Dir(lrStatePath()))
	if !ok || len(relative) != 3 || strings.ToLower(relative[2]) != "llama-server.exe" {
		return nil, nil
	}
	parent, err := proc.Parent()
	if err != nil || parent != nil {
		return nil, nil
	}
	ppid, err := proc.PPID()
	if err != nil || ppid <= 0 {
		return nil, nil
	}
	return proc, nil
}

// Explicit user stop of the recorded router, refusing uncertain identity or a living recorded owner
func StopRecordedOrphan() (bool, error) {
	decoded, err := lrReadStateFile()
	if errors.Is(err, os.ErrNotExist) {
		return true, nil
	}
	if err != nil {
		return false, nil
	}
	state, ok := decoded.(map[string]any)
	if !ok || !lrValidPid(state["pid"]) {
		return false, nil
	}
	pid, ok := state["pid"].(int)
	if !ok || pid > math.MaxInt32 {
		return false, errors.New("signed integer is greater than maximum")
	}
	failed := func() (bool, error) {
		slog.Warn("could not verify or stop recorded managed llama-server")
		return false, nil
	}
	if !lrPidExists(pid) {
		return true, nil
	}
	var proc localRuntimeProc
	if isModernLocalRuntimeState(state) {
		proc = lrRecordedProcess(state)
		if proc == nil || !lrOwnerIsDead(state) {
			return false, nil
		}
	} else {
		proc, err = legacyOrphanProcess(state)
		if err != nil {
			return false, err
		}
	}
	if proc == nil || !reflect.DeepEqual(readLocalRuntimeState(), state) || !proc.IsRunning() {
		return false, nil
	}
	if err := lrTerminateTree(proc, true); err != nil {
		return failed()
	}
	if err := proc.Wait(5 * time.Second); err != nil {
		return failed()
	}
	slog.Info(fmt.Sprintf("stopped orphaned managed llama-server pid=%d", proc.Pid()))
	return true, nil
}

// Pathlib comparison key of a path, separators and dot parts collapsed and case folded on Windows
func lrPathKey(p string) (string, []string) {
	windows := runtime.GOOS == "windows"
	if windows {
		p = strings.ReplaceAll(p, "/", `\`)
	}
	sep := string(filepath.Separator)
	anchor := ""
	if vol := filepath.VolumeName(p); vol != "" {
		anchor, p = vol, p[len(vol):]
	}
	if strings.HasPrefix(p, sep) {
		anchor += sep
		if !windows && strings.HasPrefix(p, "//") && !strings.HasPrefix(p, "///") {
			anchor = "//"
		}
	}
	var parts []string
	for _, part := range strings.Split(p, sep) {
		if part != "" && part != "." {
			parts = append(parts, part)
		}
	}
	if windows {
		anchor = strings.ToLower(anchor)
		for i := range parts {
			parts[i] = strings.ToLower(parts[i])
		}
	}
	return anchor, parts
}

// Python Path(a) == Path(b)
func lrPathEqual(a, b string) bool {
	anchorA, partsA := lrPathKey(a)
	anchorB, partsB := lrPathKey(b)
	return anchorA == anchorB && reflect.DeepEqual(partsA, partsB)
}

// Parts of path below root like Path.relative_to, false where Python raises ValueError
func lrRelativeParts(path, root string) ([]string, bool) {
	anchorP, partsP := lrPathKey(path)
	anchorR, partsR := lrPathKey(root)
	if anchorP != anchorR || len(partsP) < len(partsR) || !reflect.DeepEqual(partsP[:len(partsR)], partsR) {
		return nil, false
	}
	_, original := lrPathKeyRaw(path)
	return original[len(partsR):], true
}

// Path parts keeping their original case
func lrPathKeyRaw(p string) (string, []string) {
	if runtime.GOOS == "windows" {
		p = strings.ReplaceAll(p, "/", `\`)
	}
	sep := string(filepath.Separator)
	var parts []string
	for _, part := range strings.Split(p[len(filepath.VolumeName(p)):], sep) {
		if part != "" && part != "." {
			parts = append(parts, part)
		}
	}
	return "", parts
}

// Final component of a path like PurePath.name
func lrPathName(p string) string {
	_, parts := lrPathKeyRaw(p)
	if len(parts) == 0 {
		return ""
	}
	return parts[len(parts)-1]
}
