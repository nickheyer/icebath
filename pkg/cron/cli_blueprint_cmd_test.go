package cron

import (
	"bytes"
	"context"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/nickheyer/icebath/pkg/plugins"
	"github.com/nickheyer/icebath/pkg/tools"
)

const standupPlugin = `def register(ctx):
    ctx.register_automation_blueprint(
        "standup",
        title="Team standup digest",
        description="Weekday summary of what changed in a repo.",
        category="work",
        schedule_template="{minute} {hour} * * 1-5",
        prompt_template="Summarize yesterday's commits for {repo}.",
        slots=[
            {"name": "repo", "type": "text", "label": "Which repo?", "default": "acme/app"},
            {"name": "time", "type": "time", "label": "What time?", "default": "09:15"},
            {"name": "deliver", "type": "enum", "label": "Where?", "default": "origin",
             "options": ["origin", "local"], "strict": False},
        ],
        tags=["work"],
    )
`

// Writes an enabled Python plugin into the home and serves it through the real plugin host child
func writeBlueprintPlugin(t *testing.T, h, name, body string) *lockedLog {
	t.Helper()
	dir := filepath.Join(h, "plugins", name)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	for file, text := range map[string]string{
		filepath.Join(dir, "plugin.yaml"): "name: " + name + "\nversion: 0.1.0\n",
		filepath.Join(dir, "__init__.py"): body,
		filepath.Join(h, "config.yaml"):   `{"model": "test-model", "plugins": {"enabled": ["` + name + `"]}}`,
	} {
		if err := os.WriteFile(file, []byte(text), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	python, err := exec.LookPath("python3")
	if err != nil {
		t.Fatalf("python3 is required to run the plugin host child: %v", err)
	}
	t.Cleanup(plugins.SetHostInterpreterForTests(python))
	previousSecrets := tools.ProfileSecrets
	tools.ProfileSecrets = blueprintHostSecrets{}
	t.Cleanup(func() { tools.ProfileSecrets = previousSecrets })
	plugins.InstallServices(&plugins.Services{BuildPluginBlueprint: BuildPluginBlueprint})
	plugins.ResetPluginManagersForTests()
	pluginLogsOnce.Do(func() {
		plugins.Logger = slog.New(slog.NewTextHandler(pluginLogs, &slog.HandlerOptions{Level: slog.LevelWarn, ReplaceAttr: func(_ []string, a slog.Attr) slog.Attr {
			if a.Key == slog.TimeKey {
				return slog.Attr{}
			}
			return a
		}}))
	})
	pluginLogs.reset()
	t.Cleanup(func() {
		plugins.ResetPluginManagersForTests()
		plugins.InstallServices(nil)
	})
	return pluginLogs
}

// Warning log of the plugin manager, installed once so host goroutines never see the logger change
var (
	pluginLogs     = &lockedLog{}
	pluginLogsOnce sync.Once
)

type lockedLog struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (l *lockedLog) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.buf.Write(p)
}

func (l *lockedLog) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.buf.String()
}

func (l *lockedLog) reset() {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.buf.Reset()
}

// Empty profile secret scope the plugin host child environment is built from
type blueprintHostSecrets struct{}

func (blueprintHostSecrets) ManagedDotenvKeys() []string   { return nil }
func (blueprintHostSecrets) SourceSuppliedNames() []string { return nil }
func (blueprintHostSecrets) BuildProfileSecretScope(string) map[string]string {
	return map[string]string{}
}
func (blueprintHostSecrets) CurrentSecretScope(context.Context) map[string]string {
	return map[string]string{}
}

func blueprintKeys(ctx context.Context) []string {
	var keys []string
	for _, bp := range listBlueprints(ctx) {
		keys = append(keys, bp.Key)
	}
	return keys
}

func countKey(keys []string, key string) int {
	n := 0
	for _, k := range keys {
		if k == key {
			n++
		}
	}
	return n
}

func TestBlueprintCommandFillCreatesJob(t *testing.T) {
	s, _ := newTestStore(t)
	res, err := HandleBlueprintCommand(context.Background(), s.home, "morning-brief time=07:30 deliver=telegram", nil, "cli")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(res.Text, "Scheduled") || res.AgentSeed != nil {
		t.Fatalf("%+v", res)
	}
	jobs, err := s.LoadJobs()
	if err != nil || len(jobs) != 1 {
		t.Fatalf("jobs=%v err=%v", jobs, err)
	}
	shown := jobs[0].ScheduleDisplay
	if shown == "" {
		shown = jobs[0].Schedule.Display
	}
	if shown != "30 7 * * *" || jobs[0].Deliver == nil || *jobs[0].Deliver != "telegram" {
		t.Fatalf("schedule %q deliver %v", shown, jobs[0].Deliver)
	}
}

func TestPluginBlueprintIsListedBySlashCommandAndMatches(t *testing.T) {
	s, _ := newTestStore(t)
	logs := writeBlueprintPlugin(t, s.home, "teamtools", standupPlugin)
	ctx := context.Background()
	catalog, err := HandleBlueprintCommand(ctx, s.home, "", nil, "cli")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(catalog.Text, "teamtools:standup") || !strings.Contains(catalog.Text, "Team standup digest") {
		t.Fatalf("catalog %q logs %q", catalog.Text[len(catalog.Text)-200:], logs.String())
	}
	for _, q := range []string{"teamtools:standup", "standup"} {
		if bp, _ := matchBlueprint(ctx, q); bp == nil || bp.Key != "teamtools:standup" {
			t.Fatalf("match %q -> %+v", q, bp)
		}
	}
}

func TestPluginCannotShadowBuiltinAndBadKeysAreRejected(t *testing.T) {
	s, _ := newTestStore(t)
	logs := writeBlueprintPlugin(t, s.home, "teamtools", `def register(ctx):
    common = dict(title="Mine", description="d", schedule_template="0 9 * * *",
                  prompt_template="hi")
    ctx.register_automation_blueprint("morning-brief", **common)
    ctx.register_automation_blueprint("morning-brief", **common)   # duplicate
    ctx.register_automation_blueprint("other:spoof", **common)      # namespace spoof
`)
	ctx := context.Background()
	keys := blueprintKeys(ctx)
	if countKey(keys, "morning-brief") != 1 || countKey(keys, "teamtools:morning-brief") != 1 {
		t.Fatalf("keys %v", keys)
	}
	builtin, mine := getBlueprint(ctx, "morning-brief"), getBlueprint(ctx, "teamtools:morning-brief")
	if builtin == nil || builtin.Plugin != "" || mine == nil || mine.Title != "Mine" {
		t.Fatalf("builtin %+v mine %+v", builtin, mine)
	}
	for _, k := range keys {
		if strings.HasSuffix(k, "spoof") {
			t.Fatalf("spoofed key listed %v", keys)
		}
	}
	if !strings.Contains(logs.String(), "already registered") || !strings.Contains(logs.String(), "other:spoof") {
		t.Fatalf("logs %q", logs.String())
	}
}

func TestMalformedPluginBlueprintIsWarnedAndIgnored(t *testing.T) {
	base := `'title': 'T', 'description': 'd', 'schedule_template': '0 9 * * *', 'prompt_template': 'hi'`
	for _, tc := range []struct{ fields, reason string }{
		{`'title': 'T', 'description': 'd', 'schedule_template': '0 9 * * *', 'prompt_template': 'hello {nobody}'`, "nobody"},
		{base + `, 'slots': [{'name': 'x', 'type': 'colour', 'label': 'X'}]`, "type"},
		{`'title': 'T', 'description': 'd', 'schedule_template': 'not a schedule', 'prompt_template': 'hi'`, "schedule"},
		{`'title': 'T', 'description': 'd', 'schedule_template': '{minute} {hour} * * *', 'prompt_template': 'hi'`, "time"},
		{`'title': '', 'description': 'd', 'schedule_template': '0 9 * * *', 'prompt_template': 'hi'`, "title"},
	} {
		t.Run(tc.reason, func(t *testing.T) {
			s, _ := newTestStore(t)
			logs := writeBlueprintPlugin(t, s.home, "teamtools", "def register(ctx):\n    ctx.register_automation_blueprint('bad', **{"+tc.fields+"})\n")
			if countKey(blueprintKeys(context.Background()), "teamtools:bad") != 0 {
				t.Fatal("malformed blueprint listed")
			}
			if !strings.Contains(logs.String(), "Plugin 'teamtools' automation blueprint 'bad' rejected") || !strings.Contains(logs.String(), tc.reason) {
				t.Fatalf("logs %q", logs.String())
			}
		})
	}
}
