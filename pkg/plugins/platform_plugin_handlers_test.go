package plugins

import (
	"reflect"
	"strings"
	"testing"
)

// Fresh manager with the context of one plugin, the hermes _make_ctx twin
func handlerFactoryContext(name string) (*Manager, *Context) {
	m := NewManager("")
	return m, newContext(&Manifest{Name: name, Version: "0.1.0", Description: "test"}, m)
}

func noopHandlerFactory(native, adapter any) {}

func sameFunc(a, b any) bool { return reflect.ValueOf(a).Pointer() == reflect.ValueOf(b).Pointer() }

func TestRegisterPlatformHandlerAPIFactoryIsQueuedWithPluginName(t *testing.T) {
	m, c := handlerFactoryContext("test_plugin")
	factory := func(native, adapter any) {}
	if err := c.RegisterPlatformHandler("discord", factory); err != nil {
		t.Fatal(err)
	}
	factories := m.GetPlatformHandlerFactories("discord")
	if len(factories) != 1 || !sameFunc(factories[0].Factory, factory) || factories[0].Plugin != "test_plugin" {
		t.Fatalf("factories %+v", factories)
	}
}

func TestRegisterPlatformHandlerAPIPlatformKeyIsNormalized(t *testing.T) {
	m, c := handlerFactoryContext("test_plugin")
	if err := c.RegisterPlatformHandler("  Slack ", noopHandlerFactory); err != nil {
		t.Fatal(err)
	}
	if n := len(m.GetPlatformHandlerFactories("slack")); n != 1 {
		t.Fatalf("slack factories = %d", n)
	}
}

func TestRegisterPlatformHandlerAPINonCallableFactoryRaises(t *testing.T) {
	_, c := handlerFactoryContext("test_plugin")
	if err := c.RegisterPlatformHandler("discord", "nope"); err == nil || !strings.Contains(err.Error(), "non-callable") {
		t.Fatalf("non callable factory error = %v", err)
	}
}

func TestRegisterPlatformHandlerAPIEmptyPlatformRaises(t *testing.T) {
	_, c := handlerFactoryContext("test_plugin")
	if err := c.RegisterPlatformHandler("  ", noopHandlerFactory); err == nil || !strings.Contains(err.Error(), "empty platform") {
		t.Fatalf("empty platform error = %v", err)
	}
}

func TestRegisterPlatformHandlerAPIPlatformScoping(t *testing.T) {
	m, c := handlerFactoryContext("test_plugin")
	for _, platform := range []string{"discord", "matrix"} {
		if err := c.RegisterPlatformHandler(platform, noopHandlerFactory); err != nil {
			t.Fatal(err)
		}
	}
	if len(m.GetPlatformHandlerFactories("discord")) != 1 || len(m.GetPlatformHandlerFactories("matrix")) != 1 || len(m.GetPlatformHandlerFactories("slack")) != 0 {
		t.Fatalf("discord %d matrix %d slack %d", len(m.GetPlatformHandlerFactories("discord")), len(m.GetPlatformHandlerFactories("matrix")), len(m.GetPlatformHandlerFactories("slack")))
	}
}

func TestRegisterPlatformHandlerAPIMultiplePluginsEachRecorded(t *testing.T) {
	m := NewManager("")
	for _, name := range []string{"plugin_a", "plugin_b"} {
		c := newContext(&Manifest{Name: name, Version: "0.1.0", Description: "t"}, m)
		if err := c.RegisterPlatformHandler("telegram", noopHandlerFactory); err != nil {
			t.Fatal(err)
		}
	}
	var names []string
	for _, f := range m.GetPlatformHandlerFactories("telegram") {
		names = append(names, f.Plugin)
	}
	if !reflect.DeepEqual(names, []string{"plugin_a", "plugin_b"}) {
		t.Fatalf("names %v", names)
	}
}

func TestRegisterPlatformHandlerAPIForceRediscoveryClearsFactories(t *testing.T) {
	h := casesHome(t, "")
	m := casesManager(t, h)
	c := newContext(&Manifest{Name: "test_plugin", Version: "0.1.0", Description: "test"}, m)
	if err := c.RegisterPlatformHandler("telegram", noopHandlerFactory); err != nil {
		t.Fatal(err)
	}
	if n := len(m.GetPlatformHandlerFactories("telegram")); n != 1 {
		t.Fatalf("telegram factories before rediscovery = %d", n)
	}
	m.DiscoverAndLoad(homeCtx(h), true)
	if got := m.GetPlatformHandlerFactories("telegram"); len(got) != 0 {
		t.Fatalf("telegram factories after forced rediscovery = %+v", got)
	}
}

func TestTelegramAliasRegisterTelegramHandlerRoutesToTelegramBucket(t *testing.T) {
	m, c := handlerFactoryContext("test_plugin")
	factory := func(application, adapter any) {}
	if err := c.RegisterTelegramHandler(factory); err != nil {
		t.Fatal(err)
	}
	got := m.GetPlatformHandlerFactories("telegram")
	if len(got) != 1 || !sameFunc(got[0].Factory, factory) || got[0].Plugin != "test_plugin" {
		t.Fatalf("telegram factories %+v", got)
	}
}
