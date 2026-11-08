package plugins

import (
	"context"
	"testing"
)

func TestNewPluginContextRegistersHookOnManager(t *testing.T) {
	m := NewManager(t.TempDir())
	reg := NewPluginContext(&Manifest{Name: "probe", Key: "probe"}, m).RegisterHook("on_kanban_task_updated",
		func(context.Context, map[string]any) (any, error) { return nil, nil }, false)
	if !m.HasHook("on_kanban_task_updated") {
		t.Fatal("hook not registered on the manager")
	}
	if err := reg.Dispose(); err != nil || m.HasHook("on_kanban_task_updated") {
		t.Fatal("hook not removed on dispose", err)
	}
}

func TestNewContextBindsTheManifestAndManager(t *testing.T) {
	mgr := NewManager(t.TempDir())
	c := NewContext(&Manifest{Name: "probe", Key: "probe"}, mgr)
	if c.PluginID() != "probe" || c.manager != mgr {
		t.Fatalf("context %+v", c)
	}
}
