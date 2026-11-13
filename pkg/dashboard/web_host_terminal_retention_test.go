package dashboard

import (
	"context"
	"errors"
	"slices"
	"testing"
	"time"

	"github.com/nickheyer/icebath/pkg/tools"
)

// Viewer of a retention case that drops every frame
type hostDiscardViewer struct{}

func (hostDiscardViewer) SendBytes(context.Context, []byte) error  { return nil }
func (hostDiscardViewer) SendText(context.Context, string) error   { return nil }
func (hostDiscardViewer) Close(context.Context, int, string) error { return nil }

func TestAFullHostRegistryRefusesANewShellInsteadOfEvictingAnIdleOne(t *testing.T) {
	t.Setenv("ICEBATH_HOME", t.TempDir())
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	registry := newHostTerminalRegistry()
	var shells []*hostFakeBridge
	spawn := func() (tools.TerminalBridge, error) {
		b := newHostFakeBridge()
		shells = append(shells, b)
		return b, nil
	}
	defer func() {
		for _, shell := range shells {
			_ = shell.Close()
		}
		registry.closeAll(context.Background())
	}()
	var keys []string
	for range hostTerminalMaxShells {
		key := tokenURLSafe(32)
		session, created, err := registry.AttachOrSpawn(ctx, key, spawn, "")
		if err != nil || !created {
			t.Fatalf("spawn = %v, created %v", err, created)
		}
		viewer := &hostDiscardViewer{}
		if _, err := session.Attach(ctx, viewer, tools.PtyAttachOptions{}); err != nil {
			t.Fatal(err)
		}
		registry.Detach(key, viewer)
		keys = append(keys, key)
	}
	_, _, err := registry.AttachOrSpawn(ctx, tokenURLSafe(32), spawn, "")
	var full *tools.RegistryFullError
	if !errors.As(err, &full) {
		t.Fatalf("spawn past capacity = %v", err)
	}
	if !errors.As(registry.reapOneIdleOrRaise(), &full) {
		t.Fatal("the retention override evicted instead of refusing")
	}
	live := registry.Keys()
	for _, key := range keys {
		if !slices.Contains(live, key) {
			t.Fatalf("idle shell %s was evicted", key)
		}
	}
	if len(shells) != hostTerminalMaxShells {
		t.Fatalf("spawned %d shells", len(shells))
	}
}
