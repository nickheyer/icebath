package cron

import (
	"bytes"
	"context"
	"strings"
	"sync"
	"testing"
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
	jobs, err := s.LoadJobs(context.Background())
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
