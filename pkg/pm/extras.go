package pm

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
)

// Anchors maps each extra to the imports that prove it installed
var Anchors = map[string][]string{
	"anthropic": {"anthropic"}, "bedrock": {"boto3"}, "vertex": {"google.auth"}, "azure-identity": {"azure.identity"},
	"exa": {"exa_py"}, "firecrawl": {"firecrawl"}, "parallel-web": {"parallel"}, "ddgs": {"ddgs"},
	"otlp": {"opentelemetry.sdk"}, "mistral": {"mistralai"}, "edge-tts": {"edge_tts"}, "neutts": {"neutts"},
	"kittentts": {"kittentts", "soundfile"}, "piper": {"piper"}, "tts-premium": {"elevenlabs"},
	"voice": {"faster_whisper"}, "stt-whisper": {"faster_whisper"}, "audio-io": {"sounddevice", "numpy"},
	"silk": {"pilk"}, "wake": {"pyopen_wakeword"}, "wake-openwakeword": {"pyopen_wakeword"},
	"wake-sherpa": {"sherpa_onnx"}, "wake-porcupine": {"pvporcupine"}, "fal": {"fal_client"},
	"messaging": {"telegram"}, "telegram": {"telegram"}, "discord": {"discord"}, "slack": {"slack_bolt"},
	"matrix": {"mautrix", "asyncpg", "aiosqlite", "markdown", "aiohttp_socks"}, "dingtalk": {"dingtalk_stream"},
	"feishu": {"lark_oapi"}, "wecom": {"defusedxml"}, "keychain": {"keyring"}, "teams": {"microsoft_teams.apps"},
	"modal": {"modal"}, "daytona": {"daytona"}, "vercel": {"vercel"},
	"google":      {"googleapiclient", "google.auth", "google_auth_oauthlib.flow", "google_auth_httplib2"},
	"google-chat": {"google.cloud.pubsub_v1"}, "google-meet": {"playwright.sync_api", "websockets"},
	"youtube": {"youtube_transcript_api"}, "acp": {"acp"}, "web": {"fastapi"}, "doc-extract": {"anydoc"},
	"documents":     {"docx", "openpyxl", "pptx", "reportlab", "pypdf"},
	"documents-pdf": {"pdfplumber", "pypdfium2"}, "computer-use": {"mcp"}, "trace-upload": {"huggingface_hub"},
	"vision": {"PIL"},
}

// Imports proving an extra, the extra's own name with underscores when the table lacks it
func anchors(extra string) []string {
	if got, ok := Anchors[extra]; ok {
		return got
	}
	return []string{strings.ReplaceAll(extra, "-", "_")}
}

const importableProbe = "import importlib.util, sys\ntry:\n    found = importlib.util.find_spec(sys.argv[1]) is not None\nexcept (ImportError, ValueError):\n    found = False\nprint('1' if found else '0')\n"

// Reports whether the selected environment's interpreter finds an anchor, the import this process's children make
var anchorImportable = func(ctx context.Context, anchor string) bool {
	root := RepoRoot()
	venv, err := SelectedVenv(ctx, root)
	if err != nil {
		return false
	}
	command, err := VenvCommand(ctx, root, venv, []string{"-I"})
	if err != nil || !isRegularFile(command[0]) {
		return false
	}
	result, err := runCaptured(ctx, append(command, "-c", importableProbe, anchor), root, pyEnviron(), 60*time.Second)
	return err == nil && result.Code == 0 && pystr.Strip(result.Stdout) == "1"
}

// AnchorImportable reports whether the selected environment's interpreter finds the module through importlib find_spec
func AnchorImportable(ctx context.Context, anchor string) bool { return anchorImportable(ctx, anchor) }

// Reports every anchor of an extra importable and its platform gate admitting this machine
func Available(ctx context.Context, extra string) (bool, error) {
	supported, err := extraSupported(ctx, extra, nil, nil)
	if err != nil || !supported {
		return false, err
	}
	for _, anchor := range anchors(extra) {
		if !anchorImportable(ctx, anchor) {
			return false, nil
		}
	}
	return true, nil
}

var platformGatesCache struct {
	sync.Mutex
	gates map[string]string
}

// Extra to marker table of the checkout's pyproject, read once per process and empty when unreadable
func platformGates() (map[string]string, error) {
	platformGatesCache.Lock()
	defer platformGatesCache.Unlock()
	if platformGatesCache.gates != nil {
		return platformGatesCache.gates, nil
	}
	gates := map[string]string{}
	data, err := loadPyproject(RepoRoot())
	if err == nil {
		tool, err := tomlGet(data, "tool", NewOrderedObject())
		if err != nil {
			return nil, err
		}
		own, err := tomlGet(tool, "icebath", NewOrderedObject())
		if err != nil {
			return nil, err
		}
		table, err := tomlGet(own, "extras-platforms", NewOrderedObject())
		if err != nil {
			return nil, err
		}
		if obj, ok := table.(*OrderedObject); ok {
			for _, key := range obj.Keys {
				gates[key] = tomlText(obj.Values[key])
			}
		}
	}
	platformGatesCache.gates = gates
	return gates, nil
}

// Platform marker environment of this machine as Python's platform module reports it
func markerEnvironment() map[string]string {
	system := map[string]string{"linux": "Linux", "darwin": "Darwin", "windows": "Windows", "freebsd": "FreeBSD",
		"openbsd": "OpenBSD", "netbsd": "NetBSD", "android": "Android", "dragonfly": "DragonFly"}[runtime.GOOS]
	osName := "posix"
	if runtime.GOOS == "windows" {
		osName = "nt"
	}
	return map[string]string{"sys_platform": sysPlatform(), "platform_system": system, "platform_machine": platformMachine(), "os_name": osName}
}

// Marker evaluation through the locked PM runtime, which ships packaging
var evaluateGate func(ctx context.Context, marker string, environment map[string]string) bool

func init() {
	evaluateGate = func(ctx context.Context, marker string, environment map[string]string) bool {
		return EvaluateMarkerInRuntime(ctx, marker, environment, RuntimeOptions{Project: RepoRoot(), Imports: PMRuntimeImports})
	}
}

// Reports an extra installable here, an installed anchor set beating its platform gate
func extraSupported(ctx context.Context, extra string, environment map[string]string, importable func(string) bool) (bool, error) {
	all := true
	for _, anchor := range anchors(extra) {
		if importable != nil {
			all = all && importable(anchor)
		} else {
			all = all && anchorImportable(ctx, anchor)
		}
	}
	if all {
		return true, nil
	}
	gates, err := platformGates()
	if err != nil {
		return false, err
	}
	marker, gated := gates[extra]
	if !gated {
		return true, nil
	}
	if environment == nil {
		environment = markerEnvironment()
	}
	return evaluateGate(ctx, marker, environment), nil
}

// ExtraSupported reports an extra installable on this machine, an installed anchor set beating its platform gate
func ExtraSupported(ctx context.Context, extra string) (bool, error) {
	return extraSupported(ctx, extra, nil, nil)
}

// The one command users run for a missing extra
func InstallHint(extra string) string { return "icebath pm install --extra " + extra }

// Reports a terminal UI owning stdin, set by the TUI so a prompt never reads under it
var InteractiveAppRunning func() bool

// Client sync a lazy extra install goes through
var clientSyncVenv = SyncVenv

// Reports stdin and stdout both terminals, the only case a lazy install asks first
var promptTerminals = func() bool { return isTerminalStream(os.Stdin) && isTerminalStream(os.Stdout) }

// Writes a prompt to stdout and reads one line from stdin, failing at end of input with nothing read
var promptInput = func(prompt string) (string, error) {
	fmt.Fprint(os.Stdout, prompt)
	line, err := bufio.NewReader(os.Stdin).ReadString('\n')
	if err != nil && line == "" {
		return "", err
	}
	return strings.TrimSuffix(line, "\n"), nil
}

// Makes an extra available, asking first on a terminal, syncing it, then adopting or demanding a restart
func EnsureImport(ctx context.Context, extra string) error {
	available, err := Available(ctx, extra)
	if err != nil || available {
		return err
	}
	supported, err := extraSupported(ctx, extra, nil, nil)
	if err != nil {
		return err
	}
	if !supported {
		gates, err := platformGates()
		if err != nil {
			return err
		}
		return NewInstallError("venv", fmt.Sprintf("extra %s is not supported on this platform (gate: %s); the adapter degrades without it",
			pyrepr.StrRepr(extra), pyrepr.StrRepr(gates[extra])), "")
	}
	appRunning := InteractiveAppRunning != nil && InteractiveAppRunning()
	if !appRunning && promptTerminals() {
		answer, err := promptInput(fmt.Sprintf("\nThis needs icebath's optional %s feature, which isn't installed yet.\nInstall it now? [Y/n] ", pyrepr.StrRepr(extra)))
		if err != nil {
			answer = "n"
		}
		answer = pystr.Lower(pystr.Strip(answer))
		if answer != "" && answer != "y" && answer != "yes" {
			return NewInstallError("venv", fmt.Sprintf("installation of extra %s declined", pyrepr.StrRepr(extra)), "")
		}
	}
	if err := clientSyncVenv(ctx, SyncVenvOptions{Extras: []string{extra}}); err != nil {
		return err
	}
	if !isRegularFile(RuntimeFactsPath(ctx)) {
		return nil
	}
	root := RepoRoot()
	if _, err := AdoptSelected(ctx, root); err != nil {
		return err
	}
	venv, err := SelectedVenv(ctx, root)
	if err != nil {
		return err
	}
	site, err := SitePackages(venv)
	if err != nil {
		return err
	}
	selected := resolvePath(site)
	for _, entry := range importPathEntries() {
		if resolvePath(entry) == selected {
			return nil
		}
	}
	reason, err := RestartNeeded(ctx, root)
	if err != nil {
		return err
	}
	if reason == "" {
		reason = "this process does not run from the install's dependency environment"
	}
	NoteRestartPending(extra + " installed (" + reason + ")")
	return NewInstallError("venv", fmt.Sprintf("%s installed; restart icebath to activate it (%s)", extra, reason), "")
}

// Ensures an extra then merges the bindings importer returns into target, false when either step fails
func EnsureAndBind(ctx context.Context, extra string, importer func() (map[string]any, error), target map[string]any) (bool, error) {
	if err := EnsureImport(ctx, extra); err != nil {
		slog.Warn(fmt.Sprintf("extra %s unavailable: %s", pyrepr.StrRepr(extra), err))
		return false, nil
	}
	bindings, err := importer()
	if err != nil {
		if errorClass(err) != "ImportError" && errorClass(err) != "ModuleNotFoundError" {
			return false, err
		}
		slog.Warn(fmt.Sprintf("import after installing %s failed: %s", pyrepr.StrRepr(extra), err))
		return false, nil
	}
	for k, v := range bindings {
		target[k] = v
	}
	return true, nil
}

// Anchors of an extra the selected environment cannot import
func Missing(ctx context.Context, extra string) []string {
	var out []string
	for _, anchor := range anchors(extra) {
		if !anchorImportable(ctx, anchor) {
			out = append(out, anchor)
		}
	}
	return out
}

// Reports an anchor installed as a package dir, module file or compiled extension under a site packages tree
func installedIn(sitePackages, anchor string) bool {
	parts := strings.Split(anchor, ".")
	leaf := parts[len(parts)-1]
	parent := filepath.Join(append([]string{sitePackages}, parts[:len(parts)-1]...)...)
	if isDirectory(filepath.Join(parent, leaf)) {
		return true
	}
	matches, _ := filepath.Glob(filepath.Join(globEscape(parent), globEscape(leaf)+".*"))
	for _, match := range matches {
		name := filepath.Base(match)
		switch name[strings.LastIndex(name, ".")+1:] {
		case "py", "so", "pyd":
			return true
		}
	}
	return false
}

// DeclinedExtrasFilename is the record of extras dropped with install without
const DeclinedExtrasFilename = "declined-extras.json"

// Declined extras record of an install, this binary's own tree when projectRoot is empty
func DeclinedExtrasPath(ctx context.Context, projectRoot string) string {
	if projectRoot == "" {
		projectRoot = RepoRoot()
	}
	return filepath.Join(InstallStateDir(ctx, projectRoot), DeclinedExtrasFilename)
}

// Extras the user dropped by their PEP 685 names, refusing a damaged record rather than re adding them
func declinedExtras(ctx context.Context, projectRoot string) (map[string]bool, error) {
	path := DeclinedExtrasPath(ctx, projectRoot)
	data, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return map[string]bool{}, nil
	}
	fail := func(detail string) error {
		return NewInstallError("venv", fmt.Sprintf("cannot read %s: %s", path, detail), "fix or delete the file, then retry")
	}
	if err != nil {
		return nil, fail(pyExceptionText(err))
	}
	data = trimBOM(data)
	if err := utf8Strict(data); err != nil {
		return nil, fail(err.Error())
	}
	doc, err := loadJSON(data)
	if err != nil {
		return nil, fail(err.Error())
	}
	out := map[string]bool{}
	record, _ := doc.(map[string]any)
	if names, ok := record["declined"].([]any); ok {
		for _, name := range names {
			if s, isStr := name.(string); isStr {
				out[ExtraKey(s)] = true
			}
		}
	}
	return out, nil
}

// Why an update left each declined extra out, empty for a user's own without and for any unreadable record
func declinedReasons(ctx context.Context, projectRoot string) map[string]string {
	out := map[string]string{}
	data, err := os.ReadFile(DeclinedExtrasPath(ctx, projectRoot))
	if err != nil {
		return out
	}
	data = trimBOM(data)
	if utf8Strict(data) != nil {
		return out
	}
	doc, err := loadJSON(data)
	if err != nil {
		return out
	}
	record, _ := doc.(map[string]any)
	notes, _ := record["left_out_by_update"].(map[string]any)
	for name, text := range notes {
		if s, ok := text.(string); ok {
			out[name] = s
		}
	}
	return out
}

// Adds and removes declined extras with update reasons, writing only on change and returning the new set
func recordDeclinedExtras(ctx context.Context, add, remove []string, reasons map[string]string, projectRoot string) (map[string]bool, error) {
	current, err := declinedExtras(ctx, projectRoot)
	if err != nil {
		return nil, err
	}
	updated := map[string]bool{}
	for name := range current {
		updated[name] = true
	}
	for _, name := range add {
		updated[ExtraKey(name)] = true
	}
	for _, name := range remove {
		delete(updated, ExtraKey(name))
	}
	known := declinedReasons(ctx, projectRoot)
	merged := map[string]string{}
	for name, text := range known {
		merged[name] = text
	}
	for name, text := range reasons {
		merged[ExtraKey(name)] = pyPrefix(strings.Join(strings.FieldsFunc(text, pystr.IsSpace), " "), 300)
	}
	noted := map[string]string{}
	for name, text := range merged {
		if updated[name] {
			noted[name] = text
		}
	}
	if !sameKeys(updated, current) || !sameNotes(noted, known) {
		record := NewOrderedObject()
		record.Set("schema", 1)
		record.Set("declined", sortedKeys(updated))
		if len(noted) > 0 {
			notes := NewOrderedObject()
			for _, name := range sortedKeys(noted) {
				notes.Set(name, noted[name])
			}
			record.Set("left_out_by_update", notes)
		}
		data, err := dumpsJSON(record, true)
		if err != nil {
			return nil, err
		}
		if err := DurableWriteBytes(DeclinedExtrasPath(ctx, projectRoot), append(data, '\n')); err != nil {
			return nil, err
		}
	}
	return updated, nil
}

// Reports two sets equal
func sameKeys(a, b map[string]bool) bool {
	if len(a) != len(b) {
		return false
	}
	for k := range a {
		if !b[k] {
			return false
		}
	}
	return true
}

// Reports two string maps equal
func sameNotes(a, b map[string]string) bool {
	if len(a) != len(b) {
		return false
	}
	for k, v := range a {
		if w, ok := b[k]; !ok || w != v {
			return false
		}
	}
	return true
}

// Declared extras install without may drop, neither all nor anything all pulls in
func DroppableExtras(projectRoot string) ([]string, error) {
	data, err := loadPyproject(projectRoot)
	if err != nil {
		return nil, err
	}
	project, err := tomlGet(data, "project", NewOrderedObject())
	if err != nil {
		return nil, err
	}
	table, err := tomlGet(project, "optional-dependencies", NewOrderedObject())
	if err != nil {
		return nil, err
	}
	nameValue, err := tomlGet(project, "name", "")
	if err != nil {
		return nil, err
	}
	selfRef := regexp.MustCompile(`(?i)^` + regexp.QuoteMeta(tomlText(nameValue)) + `\[([^\]]+)\]`)
	kept, pending := map[string]bool{"all": true}, []string{"all"}
	for len(pending) > 0 {
		next := pending[len(pending)-1]
		pending = pending[:len(pending)-1]
		requirements, err := tomlGet(table, next, []any{})
		if err != nil {
			return nil, err
		}
		items, err := tomlIter(requirements)
		if err != nil {
			return nil, err
		}
		for _, item := range items {
			text, ok := item.(string)
			if !ok {
				return nil, &PyError{Class: "AttributeError", Message: fmt.Sprintf("'%s' object has no attribute 'replace'", tomlTypeName(item))}
			}
			match := selfRef.FindStringSubmatch(strings.ReplaceAll(text, " ", ""))
			if match == nil {
				continue
			}
			for _, member := range strings.Split(match[1], ",") {
				if !kept[member] {
					kept[member] = true
					pending = append(pending, member)
				}
			}
		}
	}
	names, err := tomlIter(table)
	if err != nil {
		return nil, err
	}
	declared, err := tomlStrings(names)
	if err != nil {
		return nil, err
	}
	out := []string{}
	for _, extra := range declared {
		if !kept[extra] {
			out = append(out, extra)
		}
	}
	sort.Strings(out)
	return out, nil
}

// Extras PM's first generation selects when replacing a pre PM venv, all plus every supported opt in it held
func LegacySelection(ctx context.Context, projectRoot string) ([]string, error) {
	var trees []string
	for _, venv := range []string{filepath.Join(projectRoot, "venv"), filepath.Join(projectRoot, ".venv")} {
		candidates := globUnder(filepath.Join(venv, "lib"), filepath.Join("python*", "site-packages"))
		candidates = append(candidates, filepath.Join(venv, "Lib", "site-packages"))
		for _, tree := range candidates {
			if isDirectory(tree) {
				trees = append(trees, tree)
			}
		}
	}
	refused, err := declinedExtras(ctx, projectRoot)
	if err != nil {
		return nil, err
	}
	carried := []string{}
	for _, extra := range sortedKeys(Anchors) {
		if extra == "messaging" || extra == "voice" || extra == "wake" || refused[extra] {
			continue
		}
		held := false
		for _, tree := range trees {
			all := true
			for _, anchor := range anchors(extra) {
				all = all && installedIn(tree, anchor)
			}
			held = held || all
		}
		if !held {
			continue
		}
		supported, err := extraSupported(ctx, extra, nil, func(string) bool { return false })
		if err != nil {
			return nil, err
		}
		if supported {
			carried = append(carried, extra)
		}
	}
	return append([]string{"all"}, carried...), nil
}
