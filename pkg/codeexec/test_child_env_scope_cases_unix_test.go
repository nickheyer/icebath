//go:build !windows

package codeexec

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/tools"
)

type childScopeKey struct{}

// Secret source double whose bound scope is the mapping the case puts on the context
type childScopeSecrets struct{}

func (childScopeSecrets) ManagedDotenvKeys() []string   { return nil }
func (childScopeSecrets) SourceSuppliedNames() []string { return nil }
func (childScopeSecrets) BuildProfileSecretScope(string) map[string]string {
	return map[string]string{}
}
func (childScopeSecrets) CurrentSecretScope(ctx context.Context) map[string]string {
	scope, _ := ctx.Value(childScopeKey{}).(map[string]string)
	return scope
}

// Served profile env with no secrets of its own under multiplexing, SKILL_TOKEN declared and set to the launch value
func servedScopeWorld(t *testing.T) (*tools.Env, context.Context) {
	t.Helper()
	env := testEnv(t)
	tools.RegisterEnvPassthrough(env, []string{"SKILL_TOKEN"})
	t.Setenv("SKILL_TOKEN", "launch-skill")
	prev := tools.ProfileSecrets
	tools.ProfileSecrets = childScopeSecrets{}
	t.Cleanup(func() { tools.ProfileSecrets = prev })
	tools.SetMultiplexActive(true)
	t.Cleanup(func() { tools.SetMultiplexActive(false) })
	return env, context.WithValue(context.Background(), childScopeKey{}, map[string]string{"SKILL_TOKEN": "served-skill"})
}

func TestExecuteCodeChildEnvResolvesPassthroughThroughTheContextScope(t *testing.T) {
	env, ctx := servedScopeWorld(t)
	child, err := buildChildEnv(ctx, env, childEnvSpec{RPCEndpoint: "endpoint", RPCToken: "token", TmpDir: t.TempDir()})
	if err != nil {
		t.Fatalf("execute_code child env under the served scope failed: %v", err)
	}
	if child["SKILL_TOKEN"] != "served-skill" {
		t.Fatalf("execute_code child saw SKILL_TOKEN %q, want the served scope's served-skill", child["SKILL_TOKEN"])
	}
}

func TestComputerUseStatusOfAServedProfileRunsTheDriverWithItsScope(t *testing.T) {
	env, ctx := servedScopeWorld(t)
	driver := filepath.Join(t.TempDir(), "cua-driver")
	if err := os.WriteFile(driver, []byte("#!/bin/sh\n[ \"$1\" = --version ] && printf '%s' \"${SKILL_TOKEN:-none}\"\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv(cuaDriverCmdEnv, driver)
	status, err := ComputerUseStatus(ctx, env)
	if err != nil {
		t.Fatal(err)
	}
	if version, _ := status.Get("version"); version != "served-skill" {
		t.Fatalf("driver probe saw SKILL_TOKEN %v, want the served scope's served-skill", version)
	}
}

func TestCuaDriverCLIFallbackReadsTheBoundCallersScope(t *testing.T) {
	env, ctx := servedScopeWorld(t)
	driver := filepath.Join(t.TempDir(), "cua-driver")
	if err := os.WriteFile(driver, []byte("#!/bin/sh\n[ \"$1\" = call ] && printf '{\"tree_markdown\": \"seen=%s\"}' \"${SKILL_TOKEN:-none}\"\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv(cuaDriverCmdEnv, driver)
	b, err := newCuaDriverBackend(env, "standard")
	if err != nil {
		t.Fatal(err)
	}
	release := b.bindCallContext(ctx)
	defer release()
	out, err := b.session.callToolViaCLI("list_apps", pyjson.NewObject(), 5)
	if err != nil {
		t.Fatalf("driver CLI call under the caller's served scope failed: %v", err)
	}
	if dumped, _ := pyjson.Dumps(out); !strings.Contains(dumped, "seen=served-skill") {
		t.Fatalf("driver CLI call saw %s, want the caller's served-skill", dumped)
	}
}

type callMarkKey struct{}

// Backend recording the call mark bound while each lifecycle step and listing ran
type markRecordingBackend struct {
	noopBackend
	bound context.Context
	marks []any
}

func (b *markRecordingBackend) bindCallContext(ctx context.Context) func() {
	prev := b.bound
	b.bound = ctx
	return func() { b.bound = prev }
}

func (b *markRecordingBackend) mark() {
	var mark any
	if b.bound != nil {
		mark = b.bound.Value(callMarkKey{})
	}
	b.marks = append(b.marks, mark)
}

func (b *markRecordingBackend) Start() error { b.mark(); return nil }
func (b *markRecordingBackend) Stop() error  { b.mark(); return nil }
func (b *markRecordingBackend) ListApps() ([]any, error) {
	b.mark()
	return b.noopBackend.ListApps()
}

func TestComputerUseBindsEachCallersContextOnTheBackend(t *testing.T) {
	resetBackendForTests()
	t.Cleanup(resetBackendForTests)
	var made []*markRecordingBackend
	prev := newComputerUseBackend
	newComputerUseBackend = func(*tools.Env, string) (ComputerUseBackend, error) {
		b := &markRecordingBackend{}
		made = append(made, b)
		return b, nil
	}
	t.Cleanup(func() { newComputerUseBackend = prev })
	mode := "standard"
	prevConfig := loadCUConfig
	loadCUConfig = func(*tools.Env) (config.Tree, error) {
		return config.Tree{"computer_use": map[string]any{"permission_mode": mode}}, nil
	}
	t.Cleanup(func() { loadCUConfig = prevConfig })
	env := cuPluginHome(t, t.TempDir(), nil, "", "")
	marked := func(mark string) context.Context { return context.WithValue(context.Background(), callMarkKey{}, mark) }
	first, err := getBackend(marked("start"), env, "s")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := dispatchComputerUse(marked("dispatch"), env, first, "list_apps", map[string]any{}, func() error { return nil }, nil); err != nil {
		t.Fatal(err)
	}
	mode = "bounded"
	if _, err := getBackend(marked("replace"), env, "s"); err != nil {
		t.Fatal(err)
	}
	ReleaseComputerUseSession(marked("release"), "s")
	if len(made) != 2 || fmt.Sprint(made[0].marks) != "[start dispatch replace]" || fmt.Sprint(made[1].marks) != "[replace release]" {
		var got [][]any
		for _, b := range made {
			got = append(got, b.marks)
		}
		t.Fatalf("backends saw caller marks %v, want [[start dispatch replace] [replace release]]", got)
	}
}
