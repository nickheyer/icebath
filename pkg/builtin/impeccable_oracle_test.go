package builtin

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
	"time"
)

type oracleRule struct {
	re   *regexp.Regexp
	repl string
}

var (
	oracleAdminRules = []oracleRule{
		{regexp.MustCompile(`node '[^']*/hook-admin\.mjs'`), "<HOOK_ADMIN_CMD>"},
		{regexp.MustCompile(`node "[^"]*/hook-admin\.mjs"`), "<HOOK_ADMIN_CMD>"},
		{regexp.MustCompile(`'[^']*/impeccable(?:\.exe)?' hooks`), "<HOOK_ADMIN_CMD>"},
		{regexp.MustCompile(`"[^"]*\\impeccable(?:\.exe)?" hooks`), "<HOOK_ADMIN_CMD>"},
		{regexp.MustCompile(`"chars":\s*\d+`), `"chars": <N>`},
	}
	oracleRepoVerb  = regexp.MustCompile(`node ['"]?<REPO>/skill/scripts/([a-z-]+)\.mjs['"]?`)
	oracleTailRules = []oracleRule{
		{regexp.MustCompile(`IMAGE_TOOLS: available image converters on this machine: [^.]*\. Use the first suitable one; never probe again this session\.`), "IMAGE_TOOLS: <IMAGE_TOOLS_PROBE>"},
		{regexp.MustCompile(`IMAGE_TOOLS: no image converter found \(cwebp, sips, magick, ffmpeg\)\. Ship PNG output unconverted rather than probing per image\.`), "IMAGE_TOOLS: <IMAGE_TOOLS_PROBE>"},
		{regexp.MustCompile(`"devServer": \{\s*"running": (?:true|false),\s*"ports": \[[^\]]*\]\s*\}`), `"devServer": <DEV_SERVER_PROBE>`},
		{regexp.MustCompile(`\d{4}-\d{2}-\d{2}T\d{2}-\d{2}-\d{2}Z`), "<STAMP>"},
		{regexp.MustCompile(`(?:\.\./){2,}\.impeccable/`), "<UP_TO_ROOT>/.impeccable/"},
		{regexp.MustCompile(`"durationMs":\s*\d+(?:\.\d+)?`), `"durationMs": <MS>`},
		{regexp.MustCompile(`\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}(\.\d+)?Z`), "<ISO>"},
		{regexp.MustCompile(`"(updatedAt|createdAt|checkedAt|lastCheck|lastChecked|timestamp|ts|mtimeMs|mtime|startedAt|endedAt)":\s*\d{10,}`), `"$1": <EPOCH>`},
		{regexp.MustCompile(`"([a-z][a-z0-9-]*)":\s*1[6-9]\d{11}([,}\s])`), `"$1": <EPOCH>$2`},
		{regexp.MustCompile(`"pid":(\s*)\d+`), `"pid":$1<PID>`},
		{regexp.MustCompile(`\(pid \d+\)`), "(pid <PID>)"},
		{regexp.MustCompile(`(?i)[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}`), "<UUID>"},
	}
)

// Replaces a path with a tag wherever no name character continues it, as the oracle maskPath does
func oracleMask(text, needle, tag string) string {
	var b strings.Builder
	for {
		j := strings.Index(text, needle)
		if j < 0 {
			break
		}
		b.WriteString(text[:j])
		rest := text[j+len(needle):]
		if rest == "" || !regexp.MustCompile(`^[A-Za-z0-9_-]`).MatchString(rest) {
			b.WriteString(tag)
		} else {
			b.WriteString(needle)
		}
		text = rest
	}
	return b.String() + text
}

// Oracle lib.mjs normalize over engine output, workspace files and hook results
func oracleNormalize(text, ws, userHome string) string {
	for _, r := range oracleAdminRules {
		text = r.re.ReplaceAllString(text, r.repl)
	}
	repo, _ := filepath.Abs(impeccableRepoRoot)
	wsReal, _ := filepath.EvalSymlinks(ws)
	for _, m := range [][2]string{{wsReal, "<WS>"}, {ws, "<WS>"}, {repo, "<REPO>"}, {userHome, "<HOME>"}} {
		if m[0] != "" {
			text = oracleMask(text, m[0], m[1])
		}
	}
	text = oracleRepoVerb.ReplaceAllStringFunc(text, func(m string) string {
		v := oracleRepoVerb.FindStringSubmatch(m)[1]
		switch v {
		case "context-signals":
			v = "signals"
		case "hook-admin":
			v = "hooks"
		}
		return "<IMPECCABLE> " + v
	})
	for _, r := range oracleTailRules {
		text = r.re.ReplaceAllString(text, r.repl)
	}
	return text
}

// Workspace files matching the case globs, the way snapshotFiles walks the staged workspace
func oracleSnapshot(ws string, globs []string) map[string]string {
	var regs []*regexp.Regexp
	for _, g := range globs {
		re := ""
		for i := 0; i < len(g); i++ {
			switch c := g[i]; {
			case c == '*' && i+1 < len(g) && g[i+1] == '*' && i+2 < len(g) && g[i+2] == '/':
				re, i = re+"(?:.*/)?", i+2
			case c == '*' && i+1 < len(g) && g[i+1] == '*':
				re, i = re+".*", i+1
			case c == '*':
				re += "[^/]*"
			case c == '?':
				re += "[^/]"
			default:
				re += regexp.QuoteMeta(string(c))
			}
		}
		regs = append(regs, regexp.MustCompile("^"+re+"$"))
	}
	out := map[string]string{}
	var walk func(dir string)
	walk = func(dir string) {
		entries, _ := os.ReadDir(dir)
		for _, e := range entries {
			full := filepath.Join(dir, e.Name())
			rel, _ := filepath.Rel(ws, full)
			rel = filepath.ToSlash(rel)
			switch {
			case e.IsDir() && e.Name() == ".git":
			case e.IsDir() && e.Name() == "node_modules":
				if preview := filepath.Join(full, ".impeccable-live"); pathExists(preview) {
					walk(preview)
				}
			case e.IsDir():
				walk(full)
			default:
				for _, re := range regs {
					if re.MatchString(rel) {
						out[rel] = string(must(os.ReadFile(full)))
						break
					}
				}
			}
		}
	}
	walk(ws)
	return out
}

// Copies a reference workspace tree into a fresh directory
func stageOracleWorkspace(t *testing.T, name, dst string) {
	src := filepath.Join(impeccableRepoRoot, "tests", "oracle", "workspaces", name)
	err := filepath.WalkDir(src, func(p string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(src, p)
		if d.IsDir() {
			return os.MkdirAll(filepath.Join(dst, rel), 0o755)
		}
		data, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		return os.WriteFile(filepath.Join(dst, rel), data, 0o644)
	})
	if err != nil {
		t.Fatal(err)
	}
}

// Golden steps and workspace files of one oracle case
type oracleGolden struct {
	Steps []struct {
		Stdout, Stderr string
		Exit           int
	}
	Files map[string]string
}

// Normalized outputs of the hermes stop case run through designHook
type oracleHermesRun struct {
	golden   oracleGolden
	result   string
	edit     string
	editWant string
	decision map[string]any
	files    map[string]string
	norm     func(string) string
}

// Test binary standing in as the source built engine, serving its recorded calls and checking both were made in order
func impeccableRecordedEngine(t *testing.T) string {
	t.Helper()
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	recording, err := filepath.Abs(impeccableEngineRecording)
	if err != nil {
		t.Fatal(err)
	}
	served := filepath.Join(t.TempDir(), "served.log")
	t.Setenv(impeccableReplayEnv, recording)
	t.Setenv(impeccableReplayLogEnv, served)
	t.Cleanup(func() {
		if data, _ := os.ReadFile(served); string(data) != "served 0\nserved 1\n" {
			t.Errorf("engine calls %q, the recording holds the edit then the stop", data)
		}
	})
	return self
}

// Runs the hook-session-hermes-stop-decision case with the engine at bin, or the launcher downloaded release when bin is empty
func runOracleHermesStop(t *testing.T, bin string) oracleHermesRun {
	t.Helper()
	var run oracleHermesRun
	raw, err := os.ReadFile(filepath.Join(impeccableRepoRoot, "tests", "oracle", "golden", "hook-session-hermes-stop-decision.json"))
	if err != nil || json.Unmarshal(raw, &run.golden) != nil || len(run.golden.Steps) != 2 {
		t.Fatalf("golden %v", err)
	}
	userHome, _ := os.UserHomeDir()
	tmp, _ := filepath.EvalSymlinks(t.TempDir())
	ws := filepath.Join(tmp, "ws")
	stageOracleWorkspace(t, "hook-project", ws)
	oracleHome := filepath.Join(ws, ".oracle-home")
	os.MkdirAll(oracleHome, 0o755)
	for _, k := range []string{"IMPECCABLE_BIN", "IMPECCABLE_HOME", "IMPECCABLE_DOWNLOAD_BASE", "IMPECCABLE_SKILL_DIR", "IMPECCABLE_SELF", "IMPECCABLE_HOOK_DISABLED", "IMPECCABLE_LAUNCHER_PROBE", "IMPECCABLE_HOOK_HARNESS"} {
		t.Setenv(k, "")
		os.Unsetenv(k)
	}
	if bin != "" {
		t.Setenv("IMPECCABLE_BIN", bin)
	}
	for k, v := range map[string]string{"NO_COLOR": "1", "FORCE_COLOR": "0", "IMPECCABLE_NO_UPDATE_CHECK": "1", "IMPECCABLE_NO_TELEMETRY": "1",
		"DO_NOT_TRACK": "1", "IMPECCABLE_SEED_SALT": "oracle", "HOME": oracleHome, "USERPROFILE": oracleHome, "IMPECCABLE_TELEMETRY_CHANNEL": "test"} {
		t.Setenv(k, v)
	}
	tree, err := materializeAt(filepath.Join(tmp, "icebath-cache"))
	if err != nil {
		t.Fatal(err)
	}
	launcher := tree.Host("impeccable/skills/impeccable/scripts/" + impeccableLauncherName())
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	t.Cleanup(cancel)
	engine, extra, err := newEngineLauncher(filepath.Dir(launcher)).resolveHost(ctx)
	if bin != "" {
		if err != nil || engine != bin || extra != nil {
			t.Fatalf("IMPECCABLE_BIN engine %s %v %v", engine, extra, err)
		}
	} else {
		version := strings.TrimSpace(string(must(os.ReadFile(filepath.Join(filepath.Dir(launcher), "VERSION")))))
		if err != nil || engine != filepath.Join(oracleHome, ".impeccable", "bin", version, filepath.Base(engine)) || extra != nil {
			t.Fatalf("engine download %s %v %v", engine, extra, err)
		}
		if stamp, size := readStamp(engine + ".verified"); stamp != sha256File(engine) || size != fileSize(engine) {
			t.Fatalf("engine stamp %s %s", stamp, size)
		}
	}
	h := newDesignHook(func(context.Context) string { return ws })
	h.homeDir = func(context.Context) string { return filepath.Join(tmp, "icebath-home") }
	h.trusted = func(context.Context, string) bool { return false }
	h.materialized = func() string { return launcher }
	run.norm = func(s string) string { return oracleNormalize(s, ws, userHome) }
	target := filepath.Join(ws, "src", "components", "Card.module.css")
	result := `{"success": true, "files_modified": ["` + target + `"]}`
	run.result = result
	edited, err := h.transformToolResult(ctx, map[string]any{"tool_name": "patch", "args": map[string]any{"path": "src/components/Card.module.css"}, "result": result, "session_id": "hm1"})
	if err != nil {
		t.Fatalf("edit step %v", err)
	}
	var post struct {
		HookSpecificOutput struct{ HookEventName, AdditionalContext string }
	}
	if json.Unmarshal([]byte(run.golden.Steps[0].Stdout), &post) != nil || post.HookSpecificOutput.HookEventName != "PostToolUse" || run.golden.Steps[0].Exit != 0 || run.golden.Steps[0].Stderr != "" {
		t.Fatalf("golden edit step %q", run.golden.Steps[0].Stdout)
	}
	text, _ := edited.(string)
	run.edit, run.editWant = run.norm(text), run.norm(withFindings(result, "impeccable", post.HookSpecificOutput.AdditionalContext))
	stopped, err := h.preVerify(ctx, map[string]any{"session_id": "hm1"})
	if err != nil {
		t.Fatalf("stop step %v", err)
	}
	run.decision, _ = stopped.(map[string]any)
	run.files = map[string]string{}
	for name, body := range oracleSnapshot(ws, []string{".impeccable/**", ".claude/settings.local.json", ".codex/hooks.json", ".cursor/hooks.json", ".github/hooks/impeccable.json"}) {
		run.files[name] = run.norm(body)
	}
	return run
}

// Reason text the golden stop step blocks with
func goldenStopReason(t *testing.T, g oracleGolden) string {
	t.Helper()
	var stop struct{ Decision, Reason string }
	if json.Unmarshal([]byte(g.Steps[1].Stdout), &stop) != nil || stop.Decision != "block" || g.Steps[1].Exit != 0 || g.Steps[1].Stderr != "" {
		t.Fatalf("golden stop step %q", g.Steps[1].Stdout)
	}
	return stop.Reason
}

func TestImpeccableOracleHermesStopDecision(t *testing.T) {
	run := runOracleHermesStop(t, impeccableRecordedEngine(t))
	if run.edit != run.editWant {
		t.Fatalf("edit step\n got %s\nwant %s", run.edit, run.editWant)
	}
	reason, _ := run.decision["reason"].(string)
	if want := goldenStopReason(t, run.golden); run.decision["decision"] != "block" || run.norm(reason) != want || len(run.decision) != 2 {
		t.Errorf("stop step %v\nwant %s", run.decision, want)
	}
	plugin := loadImpeccableRecording(t).Plugin
	if run.norm(run.result) != plugin.Result {
		t.Errorf("tool result %s, the upstream plugin was fed %s", run.norm(run.result), plugin.Result)
	}
	if want := run.norm(plugin.TransformToolResult); run.edit != want {
		t.Errorf("edit step\n got %s\nupstream plugin %s", run.edit, want)
	}
	pyReason, _ := plugin.PreVerify["reason"].(string)
	if plugin.PreVerify["decision"] != run.decision["decision"] || run.norm(reason) != run.norm(pyReason) || len(plugin.PreVerify) != len(run.decision) {
		t.Errorf("stop step %v, upstream plugin %v", run.decision, plugin.PreVerify)
	}
	var names []string
	for name, body := range run.files {
		names = append(names, name)
		if body != run.golden.Files[name] {
			t.Errorf("%s\n got %s\nwant %s", name, body, run.golden.Files[name])
		}
	}
	sort.Strings(names)
	if len(names) != len(run.golden.Files) {
		t.Fatalf("workspace files %v want %d golden files", names, len(run.golden.Files))
	}
}

func TestImpeccableReleasedEngineHermesStop(t *testing.T) {
	run := runOracleHermesStop(t, "")
	if run.edit != run.editWant {
		t.Fatalf("edit step\n got %s\nwant %s", run.edit, run.editWant)
	}
	message, _ := run.decision["message"].(string)
	if want := goldenStopReason(t, run.golden); run.decision["action"] != "continue" || run.norm(message) != want || len(run.decision) != 2 {
		t.Errorf("stop step %v\nwant continue with %s", run.decision, want)
	}
	want := map[string]string{".impeccable/hook.cache.json": `{"version":1,"sessions":{"hm1":{"updatedAt": <EPOCH>,"files":{"<WS>/src/components/Card.module.css":{"editCount":1,"findings":["side-tab:1","gradient-text:2"]}},"footerShown":true}}}`}
	if len(run.files) != len(want) {
		t.Fatalf("workspace files %v want %v", run.files, want)
	}
	for name, body := range want {
		if run.files[name] != body {
			t.Errorf("%s\n got %s\nwant %s", name, run.files[name], body)
		}
	}
}
