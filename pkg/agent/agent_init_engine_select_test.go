package agent

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/plugins"
	"github.com/nickheyer/icebath/pkg/pm"
	"github.com/nickheyer/icebath/pkg/provider"
	"github.com/nickheyer/icebath/pkg/tools"
)

// Bare startup agent on a platform with its prints captured and warning presentation on
func startupAgent(t *testing.T, platform string) (*AIAgent, *[]string) {
	t.Helper()
	printed := &[]string{}
	prev := warningNotificationsEnabled
	t.Cleanup(func() { warningNotificationsEnabled = prev })
	warningNotificationsEnabled = func(context.Context, string, map[string]any) bool { return true }
	return &AIAgent{platform: platform, printFn: func(s string) { *printed = append(*printed, s) }}, printed
}

// Agent init over a configured memory provider that resolves nowhere, with lazy installs refused
func startWithMissingMemoryProvider(t *testing.T, platform string) (*AIAgent, *[]string) {
	t.Helper()
	env := promptTestEnv(t)
	t.Setenv(pm.EnvDisableLazyInstalls, "1")
	a, printed := startupAgent(t, platform)
	a.env = env
	initMemory(home.WithOverride(context.Background(), env.Home), a, map[string]any{"memory": map[string]any{"provider": "scout_missing_provider"}}, false, platform, nil)
	if a.memoryManager != nil {
		t.Fatal("a missing provider built a memory manager")
	}
	if _, err := os.Stat(filepath.Join(env.Home, "plugins")); err == nil {
		t.Fatal("startup recovery installed a plugin with lazy installs off")
	}
	return a, printed
}

func TestCLIStartPrintsTheRefusal(t *testing.T) {
	t.Setenv("ICEBATH_HOME", t.TempDir())
	_, printed := startWithMissingMemoryProvider(t, "cli")
	for _, line := range *printed {
		if strings.Contains(line, "icebath plugins install scout_missing_provider") {
			return
		}
	}
	t.Fatalf("printed %q", *printed)
}

func TestGatewayStartReplaysTheRefusalOnceOnFirstTurn(t *testing.T) {
	t.Setenv("ICEBATH_HOME", t.TempDir())
	a, _ := startWithMissingMemoryProvider(t, "telegram")
	var notices []provider.AgentNotice
	a.noticeCallback = func(n provider.AgentNotice) { notices = append(notices, n) }
	a.replayStartupWarnings()
	a.replayStartupWarnings()
	if len(notices) != 1 || notices[0].Level != "warn" || !strings.Contains(notices[0].Text, "icebath plugins install scout_missing_provider") {
		t.Fatalf("notices %+v", notices)
	}
}

func TestAGatewayHostedCLISessionPrintsTheStartupWarningOnItsFirstTurn(t *testing.T) {
	t.Setenv("ICEBATH_HOME", t.TempDir())
	a, printed := startupAgent(t, "cli")
	a.gatewaySessionKey = "agent:main:cli:dm:local"
	a.emitStartupWarning("✓ Something moved out of core — installed its plugin.")
	if len(*printed) != 0 {
		t.Fatalf("printed during init %q", *printed)
	}
	a.replayStartupWarnings()
	a.replayStartupWarnings()
	if len(*printed) != 1 || (*printed)[0] != "✓ Something moved out of core — installed its plugin." {
		t.Fatalf("printed %q", *printed)
	}
}

const ctxDemoEngineSrc = `
from agent.context_engine import ContextEngine
class Demo(ContextEngine):
    @property
    def name(self):
        return "ctx_demo"
    def update_from_response(self, usage):
        pass
    def should_compress(self, prompt_tokens=None):
        return False
    def compress(self, messages, current_tokens=None):
        return messages
def register(ctx):
    ctx.register_context_engine(Demo())
`

type lcmEngine struct{ stubEngine }

func (e *lcmEngine) Name() string { return "lcm" }

func (e *lcmEngine) CloneForAgent() ContextEngine {
	c := *e
	c.ContextEngineBase = e.CloneBase()
	return &c
}

// Engine holding a lock that hands out a fresh engine per agent and counts the clones
type lockedLCMEngine struct {
	lcmEngine
	lock   *sync.Mutex
	clones int
}

func newLockedLCMEngine() *lockedLCMEngine {
	return &lockedLCMEngine{lcmEngine: lcmEngine{stubEngine{ContextEngineBase: NewContextEngineBase()}}, lock: &sync.Mutex{}}
}

func (e *lockedLCMEngine) CloneForAgent() ContextEngine {
	e.clones++
	return newLockedLCMEngine()
}

// Agent init selection over a registered plugin engine with no plugins/context_engine directory hit
func selectRegisteredEngine(t *testing.T, engine ContextEngine) ContextEngine {
	t.Helper()
	prevLoad, prevPlugin := loadContextEngineFn, pluginContextEngineFn
	t.Cleanup(func() { loadContextEngineFn, pluginContextEngineFn = prevLoad, prevPlugin })
	loadContextEngineFn = func(context.Context, string) *plugins.HostedObject { return nil }
	pluginContextEngineFn = func(context.Context) any { return engine }
	return selectContextEngine(tools.WithToolEnv(context.Background(), promptTestEnv(t)), map[string]any{"context": map[string]any{"engine": engine.Name()}})
}

func TestEngineWithUncopyableStateIsSelectedViaCloneForAgent(t *testing.T) {
	t.Setenv("ICEBATH_HOME", t.TempDir())
	singleton := newLockedLCMEngine()
	selected, ok := selectRegisteredEngine(t, singleton).(*lockedLCMEngine)
	if !ok || selected == singleton || singleton.clones != 1 {
		t.Fatalf("selected %v clones %d", selected, singleton.clones)
	}
}

func TestDefaultCloneIsolatesParentFromChildUpdateModel(t *testing.T) {
	t.Setenv("ICEBATH_HOME", t.TempDir())
	singleton := &lcmEngine{stubEngine{ContextEngineBase: NewContextEngineBase()}}
	singleton.UpdateModel("big", 1_000_000, "", "", "", "")
	child := selectRegisteredEngine(t, singleton)
	if child == nil || child == ContextEngine(singleton) {
		t.Fatalf("child %v", child)
	}
	child.UpdateModel("small", 204_800, "", "", "", "")
	if singleton.ContextLength != 1_000_000 || child.Base().ContextLength != 204_800 {
		t.Fatalf("parent %d child %d", singleton.ContextLength, child.Base().ContextLength)
	}
}
