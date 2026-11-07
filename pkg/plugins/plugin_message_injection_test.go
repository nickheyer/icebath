package plugins

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"sync"
	"testing"

	"github.com/nickheyer/icebath/pkg/home"
)

type pmiCLI struct {
	running            bool
	pending, interrupt []string
}

func (c *pmiCLI) AgentRunning() bool        { return c.running }
func (c *pmiCLI) QueueInterrupt(msg string) { c.interrupt = append(c.interrupt, msg) }
func (c *pmiCLI) QueuePending(msg string)   { c.pending = append(c.pending, msg) }
func (c *pmiCLI) Agent() any                { return nil }

type pmiInjector struct {
	mu     sync.Mutex
	calls  []map[string]any
	result bool
	panics bool
}

// Records every kwargs map the host hands the injector and answers the configured result
func (r *pmiInjector) inject(kwargs map[string]any) bool {
	r.mu.Lock()
	r.calls = append(r.calls, kwargs)
	r.mu.Unlock()
	if r.panics {
		panic("gateway unavailable")
	}
	return r.result
}

// Writes a profile config whose notify-plugin entry is the given mapping
func pmiWriteConfig(t *testing.T, dir string, entry map[string]any) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	cfg := map[string]any{"plugins": map[string]any{"entries": map[string]any{"notify-plugin": entry}}}
	if err := os.WriteFile(filepath.Join(dir, "config.yaml"), []byte(dumpYAML(t, cfg)), 0o644); err != nil {
		t.Fatal(err)
	}
}

// Context of a user plugin over a manager of a fresh profile home holding the given entry
func pmiContext(t *testing.T, entry map[string]any) (*Context, *Manager) {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "icebath")
	if entry != nil {
		pmiWriteConfig(t, dir, entry)
	}
	m := NewManager(dir)
	return newContext(&Manifest{Name: "notify-plugin", Key: "notify-plugin", Source: "user"}, m), m
}

func pmiInstallBoundKey(t *testing.T) {
	previous := installedServices()
	InstallServices(&Services{BoundSessionKey: func(ctx context.Context) string {
		key, _ := ctx.Value(pmiKey{}).(string)
		return key
	}})
	t.Cleanup(func() { InstallServices(previous) })
}

type pmiKey struct{}

var pmiOrigin = map[string]any{"platform": "discord", "chat_id": "42", "chat_type": "thread", "thread_id": "42", "user_id": "u1"}

func TestCliIdleInjectionKeepsExistingQueueBehaviour(t *testing.T) {
	c, m := pmiContext(t, nil)
	cli := &pmiCLI{}
	m.SetCLIRef(cli)
	if !c.InjectMessage(context.Background(), "new input", "user", "", nil) || !reflect.DeepEqual(cli.pending, []string{"new input"}) || len(cli.interrupt) != 0 {
		t.Fatalf("idle cli = %v %v", cli.pending, cli.interrupt)
	}
}

func TestCliRunningInjectionKeepsExistingInterruptBehaviour(t *testing.T) {
	c, m := pmiContext(t, nil)
	cli := &pmiCLI{running: true}
	m.SetCLIRef(cli)
	if !c.InjectMessage(context.Background(), "status", "system", "", nil) || !reflect.DeepEqual(cli.interrupt, []string{"[system] status"}) || len(cli.pending) != 0 {
		t.Fatalf("running cli = %v %v", cli.pending, cli.interrupt)
	}
}

// No key and no origin hands the gateway the session bound on the caller context, empty when none is
func TestKeyLessGatewayInjectionLetsTheGatewayPickTheChat(t *testing.T) {
	pmiInstallBoundKey(t)
	c, m := pmiContext(t, map[string]any{"allow_gateway_injection": true})
	inj := &pmiInjector{}
	m.SetGatewayMessageInjector(new(int), inj.inject)
	if c.InjectMessage(context.Background(), "wake up", "user", "", nil) {
		t.Fatal("injector answered false yet inject reported true")
	}
	c.InjectMessage(context.WithValue(context.Background(), pmiKey{}, "agent:main:local:dm:local-1"), "for me", "user", "", nil)
	if len(inj.calls) != 2 {
		t.Fatalf("calls = %v", inj.calls)
	}
	want := [][3]any{{true, "", "wake up"}, {true, "agent:main:local:dm:local-1", "for me"}}
	for i, call := range inj.calls {
		if got := [3]any{call["keyless"], call["calling_session_key"], call["content"]}; got != want[i] {
			t.Fatalf("call %d = %v", i, got)
		}
		if _, ok := call["session_key"]; ok || call["plugin_home"] != m.home {
			t.Fatalf("call %d = %v", i, call)
		}
	}
}

func TestKeyLessInjectionWithoutAGatewayIsRefused(t *testing.T) {
	c, _ := pmiContext(t, map[string]any{"allow_gateway_injection": true})
	if c.InjectMessage(context.Background(), "wake up", "user", "", nil) {
		t.Fatal("key less inject without a gateway accepted")
	}
}

func TestKeyLessGatewayInjectionRequiresExplicitPermission(t *testing.T) {
	c, m := pmiContext(t, map[string]any{})
	inj := &pmiInjector{result: true}
	m.SetGatewayMessageInjector(new(int), inj.inject)
	if c.InjectMessage(context.Background(), "wake up", "user", "", nil) || len(inj.calls) != 0 {
		t.Fatalf("unpermitted key less inject = %v", inj.calls)
	}
}

func TestGatewayInjectionRequiresExplicitPermission(t *testing.T) {
	c, m := pmiContext(t, map[string]any{})
	inj := &pmiInjector{result: true}
	m.SetGatewayMessageInjector(new(int), inj.inject)
	if c.InjectMessage(context.Background(), "wake up", "user", "agent:main:telegram:dm:42", nil) || len(inj.calls) != 0 {
		t.Fatalf("unpermitted inject = %v", inj.calls)
	}
}

func TestGatewayInjectionDoesNotTreatStringAsPermission(t *testing.T) {
	c, m := pmiContext(t, map[string]any{"allow_gateway_injection": "false"})
	inj := &pmiInjector{result: true}
	m.SetGatewayMessageInjector(new(int), inj.inject)
	if c.InjectMessage(context.Background(), "wake up", "user", "agent:main:telegram:dm:42", nil) || len(inj.calls) != 0 {
		t.Fatalf("string permission inject = %v", inj.calls)
	}
}

// An unreadable profile config denies injection instead of granting it
func TestGatewayInjectionFailsClosedWhenConfigCannotBeRead(t *testing.T) {
	c, m := pmiContext(t, nil)
	pmiWriteConfig(t, m.home, map[string]any{"allow_gateway_injection": true})
	previous := injectionConfigLoader
	injectionConfigLoader = func(string) (map[string]any, error) { return nil, errors.New("config unavailable") }
	t.Cleanup(func() { injectionConfigLoader = previous })
	inj := &pmiInjector{result: true}
	m.SetGatewayMessageInjector(new(int), inj.inject)
	if c.InjectMessage(context.Background(), "wake up", "user", "agent:main:telegram:dm:42", nil) || len(inj.calls) != 0 {
		t.Fatalf("unreadable config inject = %v", inj.calls)
	}
}

func TestGatewayInjectionRequiresLiveHost(t *testing.T) {
	c, m := pmiContext(t, map[string]any{"allow_gateway_injection": true})
	if m.HasGatewayMessageInjector() || c.InjectMessage(context.Background(), "wake up", "user", "agent:main:telegram:dm:42", nil) {
		t.Fatal("inject without a live gateway accepted")
	}
}

func TestGatewayInjectionPassesHostOwnedPluginIdentity(t *testing.T) {
	c, m := pmiContext(t, map[string]any{"allow_gateway_injection": true})
	inj := &pmiInjector{result: true}
	m.SetGatewayMessageInjector(new(int), inj.inject)
	if !c.InjectMessage(context.Background(), "wake up", "system", "agent:main:telegram:dm:42", nil) {
		t.Fatal("permitted inject refused")
	}
	want := []map[string]any{{"session_key": "agent:main:telegram:dm:42", "plugin_home": m.home, "content": "[system] wake up", "plugin_id": "notify-plugin"}}
	if !reflect.DeepEqual(inj.calls, want) {
		t.Fatalf("calls = %v", inj.calls)
	}
}

func TestGatewayInjectionFailsClosedOnHostException(t *testing.T) {
	c, m := pmiContext(t, map[string]any{"allow_gateway_injection": true})
	inj := &pmiInjector{result: true, panics: true}
	m.SetGatewayMessageInjector(new(int), inj.inject)
	if c.InjectMessage(context.Background(), "wake up", "user", "agent:main:telegram:dm:42", nil) {
		t.Fatal("raising host reported acceptance")
	}
}

// One published gateway host stamps every existing and later profile manager and its clear unstamps them all
func TestPublishedGatewayHostReachesExistingAndFutureProfileManagers(t *testing.T) {
	root := t.TempDir()
	var homes []string
	for _, name := range []string{"launch", "secondary", "late"} {
		dir := filepath.Join(root, name)
		pmiWriteConfig(t, dir, map[string]any{"allow_gateway_injection": true})
		homes = append(homes, dir)
	}
	t.Setenv("ICEBATH_HOME", homes[0])
	ResetPluginManagersForTests()
	owner := new(int)
	t.Cleanup(func() {
		ClearPublishedGatewayMessageHost(owner)
		ResetPluginManagersForTests()
	})
	managerFor := func(dir string) *Manager { return GetPluginManager(home.WithOverride(context.Background(), dir)) }
	inj := &pmiInjector{result: true}
	launch, secondary := managerFor(homes[0]), managerFor(homes[1])
	if secondary == launch || secondary.HasGatewayMessageInjector() {
		t.Fatal("secondary profile shares the launch manager or starts stamped")
	}
	PublishGatewayMessageHost(owner, inj.inject)
	if !launch.HasGatewayMessageInjector() || !secondary.HasGatewayMessageInjector() {
		t.Fatal("published host missed an existing manager")
	}
	late := managerFor(homes[2])
	if !late.HasGatewayMessageInjector() || managerFor(homes[0]) != launch || !launch.HasGatewayMessageInjector() {
		t.Fatal("late or returning manager not stamped")
	}
	c := newContext(&Manifest{Name: "notify-plugin", Key: "notify-plugin", Source: "user"}, secondary)
	if !c.InjectMessage(home.WithOverride(context.Background(), homes[1]), "continue", "user", "agent:secondary:telegram:dm:42", nil) {
		t.Fatal("secondary profile inject refused")
	}
	want := []map[string]any{{"session_key": "agent:secondary:telegram:dm:42", "plugin_home": secondary.home, "content": "continue", "plugin_id": "notify-plugin"}}
	if !reflect.DeepEqual(inj.calls, want) {
		t.Fatalf("calls = %v", inj.calls)
	}
	ClearPublishedGatewayMessageHost(owner)
	if launch.HasGatewayMessageInjector() || secondary.HasGatewayMessageInjector() || late.HasGatewayMessageInjector() {
		t.Fatal("cleared host still stamped on a manager")
	}
}

// An origin rides with the manager home, never the ambient home of the calling thread
func TestOriginInjectionTargetsThePluginsOwnHome(t *testing.T) {
	c, m := pmiContext(t, map[string]any{"allow_gateway_injection": true})
	inj := &pmiInjector{result: true}
	m.SetGatewayMessageInjector(new(int), inj.inject)
	other := t.TempDir()
	if !c.InjectMessage(home.WithOverride(context.Background(), other), "Kick off", "user", "", pmiOrigin) {
		t.Fatal("origin inject refused")
	}
	want := []map[string]any{{"origin": pmiOrigin, "plugin_home": m.home, "content": "Kick off", "plugin_id": "notify-plugin"}}
	if !reflect.DeepEqual(inj.calls, want) {
		t.Fatalf("calls = %v", inj.calls)
	}
}

// The permission gate reads the plugin profile even when the ambient home grants it
func TestOriginInjectionPermissionComesFromThePluginsOwnProfile(t *testing.T) {
	c, m := pmiContext(t, map[string]any{"allow_gateway_injection": false})
	inj := &pmiInjector{result: true}
	m.SetGatewayMessageInjector(new(int), inj.inject)
	granting := filepath.Join(t.TempDir(), "granting")
	pmiWriteConfig(t, granting, map[string]any{"allow_gateway_injection": true})
	t.Setenv("ICEBATH_HOME", granting)
	if c.InjectMessage(home.WithOverride(context.Background(), granting), "x", "user", "", pmiOrigin) || len(inj.calls) != 0 {
		t.Fatalf("ambient grant leaked = %v", inj.calls)
	}
}

func TestOriginInjectionRejectsAmbiguousOrMalformedTargets(t *testing.T) {
	c, m := pmiContext(t, map[string]any{"allow_gateway_injection": true})
	inj := &pmiInjector{result: true}
	m.SetGatewayMessageInjector(new(int), inj.inject)
	if c.InjectMessage(context.Background(), "x", "user", "agent:main:discord:thread:42", pmiOrigin) || c.InjectMessage(context.Background(), "x", "user", "", "discord:42") || len(inj.calls) != 0 {
		t.Fatalf("ambiguous or malformed target accepted = %v", inj.calls)
	}
}

// An origin names a messaging chat so a CLI hosted plugin gets false and no REPL entry
func TestOriginInjectionNeverLandsInTheAttachedCli(t *testing.T) {
	c, m := pmiContext(t, nil)
	cli := &pmiCLI{}
	m.SetCLIRef(cli)
	if c.InjectMessage(context.Background(), "x", "user", "", pmiOrigin) || len(cli.pending)+len(cli.interrupt) != 0 {
		t.Fatalf("origin landed in the cli = %v %v", cli.pending, cli.interrupt)
	}
}

// The TUI injector occupies its own slot so publishing or clearing it never touches the gateway slot
func TestTuiInjectorSlotDoesNotReplaceTheGatewaySlot(t *testing.T) {
	m := NewManager(t.TempDir())
	gateway, tui := &pmiInjector{result: true}, &pmiInjector{}
	gatewayOwner, tuiOwner := new(int), new(int)
	m.SetGatewayMessageInjector(gatewayOwner, gateway.inject)
	m.SetTUIMessageInjector(tuiOwner, tui.inject)
	if !m.HasGatewayMessageInjector() || !m.HasTUIMessageInjector() {
		t.Fatal("one slot replaced the other")
	}
	if !m.InjectGatewayMessage(map[string]any{"session_key": "agent:main:telegram:dm:1"}) || m.InjectTUIMessage(map[string]any{"session_key": "ses_tui"}) {
		t.Fatal("slot results crossed")
	}
	if !reflect.DeepEqual(gateway.calls, []map[string]any{{"session_key": "agent:main:telegram:dm:1"}}) || !reflect.DeepEqual(tui.calls, []map[string]any{{"session_key": "ses_tui"}}) {
		t.Fatalf("calls = %v %v", gateway.calls, tui.calls)
	}
	m.ClearTUIMessageInjector(tuiOwner)
	if m.HasTUIMessageInjector() || !m.HasGatewayMessageInjector() || !m.InjectGatewayMessage(map[string]any{"session_key": "kept"}) {
		t.Fatal("clearing the TUI slot touched the gateway slot")
	}
}
