package plugins

import (
	"context"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

const holdStreamRedactor = "def register(ctx):\n" +
	"    ctx.register_hook('transform_llm_output', lambda response_text, **_: response_text, hold_stream=True)\n"

func noopHook(context.Context, map[string]any) (any, error) { return nil, nil }

func TestTheHoldLastsAsLongAsTheRegistrationThatDeclaredIt(t *testing.T) {
	m := NewManager("")
	c := newContext(&Manifest{Name: "redactor", Source: "user"}, m)
	c.RegisterHook("transform_llm_output", noopHook, false)
	if m.HoldsStream() {
		t.Fatal("a rewrite that did not ask holds the stream")
	}
	held := c.RegisterHook("transform_llm_output", noopHook, true)
	if !m.HoldsStream() {
		t.Fatal("the declared hold is not in force")
	}
	if err := held.Dispose(); err != nil {
		t.Fatal(err)
	}
	if m.HoldsStream() || !m.HasHook("transform_llm_output") {
		t.Fatalf("after dispose holds=%v has_hook=%v", m.HoldsStream(), m.HasHook("transform_llm_output"))
	}
}

func TestAHoldOnAHookThatDoesNotRewriteTheReplyHoldsNothing(t *testing.T) {
	for _, hook := range []string{"post_llm_call", "transform_tool_result"} {
		t.Run(hook, func(t *testing.T) {
			logs := captureLogs(t)
			m := NewManager("")
			newContext(&Manifest{Name: "observer", Source: "user"}, m).RegisterHook(hook, noopHook, true)
			if m.HoldsStream() {
				t.Fatal("a hook that does not rewrite the reply holds the stream")
			}
			if !strings.Contains(logs.String(), "hold_stream") {
				t.Fatalf("the ignored declaration was not reported: %s", logs.String())
			}
		})
	}
}

func TestATurnStartingDuringAPluginReloadSeesTheSettledHold(t *testing.T) {
	var loading atomic.Bool
	entered, release := make(chan struct{}), make(chan struct{})
	released := false
	free := func() {
		if !released {
			released = true
			close(release)
		}
	}
	ledgerSwapBundled(t, ledgerBundled{m: Manifest{Name: "redactor", Version: "0.1.0"}, register: func(ctx *Context) error {
		if loading.Load() {
			close(entered)
			select {
			case <-release:
			case <-time.After(10 * time.Second):
			}
		}
		ctx.RegisterHook("transform_llm_output", func(_ context.Context, kwargs map[string]any) (any, error) {
			return kwargs["response_text"], nil
		}, true)
		return nil
	}})
	h := casesHome(t, "  enabled:\n  - redactor\n")
	t.Setenv(EnvEnableProjectPlugins, "0")
	ResetPluginManagersForTests()
	t.Cleanup(ResetPluginManagersForTests)
	t.Cleanup(free)
	ctx := homeCtx(h)
	m := GetPluginManager(ctx)
	m.DiscoverAndLoad(ctx, false)
	if !m.HoldsStream() {
		t.Fatalf("the loaded redactor does not hold the stream (load error %q)", loadedError(m, "redactor"))
	}
	loading.Store(true)
	reloaded := make(chan struct{})
	go func() { m.DiscoverAndLoad(ctx, true); close(reloaded) }()
	select {
	case <-entered:
	case <-time.After(10 * time.Second):
		t.Fatal("the reload never re-registered the plugin")
	}
	reading, answers := make(chan struct{}), make(chan bool, 1)
	go func() {
		close(reading)
		answers <- m.HoldsStream()
	}()
	select {
	case <-reading:
	case <-time.After(10 * time.Second):
		t.Fatal("the turn never started reading")
	}
	select {
	case got := <-answers:
		t.Fatalf("the turn read %v while the plugin was mid-reload", got)
	case <-time.After(2 * time.Second):
	}
	free()
	select {
	case <-reloaded:
	case <-time.After(10 * time.Second):
		t.Fatal("the reload did not finish")
	}
	select {
	case got := <-answers:
		if !got {
			t.Fatal("the turn read an unsettled hold")
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the turn never got its answer")
	}
}
