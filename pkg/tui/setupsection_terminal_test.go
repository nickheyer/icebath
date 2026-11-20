package tui

import (
	"context"
	"os"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/charmbracelet/x/ansi"

	"github.com/nickheyer/icebath/pkg/ui/wm"
)

// Gateway that answers the setup verbs from canned state and records what a walk sends
type sectionGateway struct {
	load    map[string]any
	loaded  map[string]any
	saved   map[string]any
	saves   []map[string]any
	actions []map[string]any
	failing map[string]bool
}

func (g *sectionGateway) canonical() bool { return false }

func (g *sectionGateway) request(_ context.Context, method string, params map[string]any, _ time.Duration) (any, error) {
	switch method {
	case "setup.section.load":
		g.loaded = params
		return g.load, nil
	case "setup.section.save":
		g.saved = params
		g.saves = append(g.saves, params)
		return map[string]any{"config": map[string]any{}, "env": []any{}, "removed": []any{}}, nil
	case "setup.action.start":
		g.actions = append(g.actions, params)
		return map[string]any{"job": params["action"]}, nil
	case "setup.action.poll":
		return map[string]any{"lines": []any{"probe line"}, "done": true, "ok": !g.failing[params["job"].(string)]}, nil
	}
	return map[string]any{}, nil
}

// Load result of a section over the given config values, env values, held secrets and gateway values
func sectionLoad(held, env map[string]any, secrets []any, values map[string]any) map[string]any {
	return map[string]any{"config": held, "env": env, "secrets": secrets, "facts": map[string]any{}, "values": values}
}

// Walks a section, picking the first option whose title holds each answer or typing it into a prompt
func walkSection(t *testing.T, spec sectionSpec, load map[string]any, answers ...string) (*sectionGateway, string) {
	t.Helper()
	return walkSectionWith(t, &sectionGateway{load: load}, spec, answers...)
}

// Walks a section against the given gateway
func walkSectionWith(t *testing.T, g *sectionGateway, spec sectionSpec, answers ...string) (*sectionGateway, string) {
	t.Helper()
	a := testApp(t)
	a.events.gw = g
	drain(a, a.runSection(spec))
	for _, answer := range answers {
		if d, isDialog := a.wm.Overlay().(*dialogCard); isDialog {
			if len(d.fields) != 1 || !d.fields[0].masked {
				t.Fatalf("secret %q asked without a masked field", answer)
			}
			drain(a, d.onAnswer(nil, nil, []string{answer}))
			continue
		}
		p, isPalette := a.wm.Overlay().(*wm.Palette)
		if !isPalette {
			t.Fatalf("no question open for %q, transcript\n%s", answer, screen(a))
		}
		matches := p.Matches()
		if len(matches) == 0 {
			drain(a, p.OnText(answer))
			continue
		}
		picked := false
		for _, item := range matches {
			if strings.Contains(item.Title, answer) {
				a.wm.Dismiss()
				drain(a, item.Run())
				picked = true
				break
			}
		}
		if !picked {
			t.Fatalf("no option %q among %+v", answer, matches)
		}
	}
	if g.saved == nil {
		t.Fatalf("walk did not save, transcript\n%s", screen(a))
	}
	return g, ansi.Strip(screen(a))
}

// Writes of a save as target, key and value triples in order
func savedWrites(g *sectionGateway) [][3]any {
	var out [][3]any
	for _, raw := range g.saved["writes"].([]any) {
		w := raw.(map[string]any)
		value := w["value"]
		if w["kind"] == "held" {
			value = "held"
		}
		out = append(out, [3]any{w["target"], w["key"], value})
	}
	return out
}

// Removals of a save as target and key pairs
func savedRemovals(g *sectionGateway) [][2]any {
	var out [][2]any
	for _, raw := range g.saved["removals"].([]any) {
		r := raw.(map[string]any)
		out = append(out, [2]any{r["target"], r["key"]})
	}
	return out
}

// Gateway values of a machine with docker, a singularity row and the given extras
func terminalValues(extra map[string]any) map[string]any {
	cwd, _ := os.Getwd()
	values := map[string]any{
		"terminal.plugin_backends":      []any{map[string]any{"name": "singularity", "display_name": "Singularity/Apptainer", "description": "HPC-friendly rootless container"}},
		"terminal.builtin_backends":     []any{"local", "docker", "modal", "daytona", "vercel_sandbox", "ssh"},
		"terminal.docker":               map[string]any{"path": "/usr/bin/docker", "runtime": "Docker"},
		"terminal.ssh_defaults":         map[string]any{"user": "nick", "key": "/home/nick/.ssh/id_rsa"},
		"terminal.vercel_linked:" + cwd: map[string]any{},
		"terminal.vercel_default_image": "vercel/sandbox/universal:latest",
	}
	for k, v := range extra {
		values[k] = v
	}
	return values
}

func TestTerminalSectionKeepSavesTheWholeConfigWithoutAnswers(t *testing.T) {
	g, shown := walkSection(t, terminalSection(), sectionLoad(map[string]any{"terminal.backend": "docker"}, map[string]any{}, []any{}, terminalValues(nil)), "Keep current")
	if g.saved["full_save"] != true || len(savedWrites(g)) != 0 || len(savedRemovals(g)) != 0 {
		t.Fatalf("save %+v", g.saved)
	}
	if !strings.Contains(shown, "Keeping current backend docker") {
		t.Fatalf("transcript\n%s", shown)
	}
	keys := map[any]bool{}
	for _, key := range g.loaded["config_keys"].([]any) {
		keys[key] = true
	}
	if len(g.loaded["values"].([]any)) != 6 || !keys["terminal.vercel_runtime"] || !keys["terminal.container_disk"] {
		t.Fatalf("load %+v", g.loaded)
	}
}

func TestTerminalSectionLocalWritesTheBackendThenTheEnvMirror(t *testing.T) {
	g, _ := walkSection(t, terminalSection(), sectionLoad(map[string]any{}, map[string]any{}, []any{}, terminalValues(nil)), "Local")
	want := [][3]any{{"config", "terminal.backend", "local"}, {"env", "TERMINAL_ENV", "local"}}
	if got := savedWrites(g); !reflect.DeepEqual(got, want) {
		t.Fatalf("writes %+v", got)
	}
}

func TestTerminalSectionDockerReportsTheRuntimeAndEnablesTheFirewall(t *testing.T) {
	g, shown := walkSection(t, terminalSection(), sectionLoad(map[string]any{}, map[string]any{}, []any{}, terminalValues(nil)), "Docker", "Enable")
	want := [][3]any{{"config", "proxy.enabled", true}, {"config", "terminal.backend", "docker"}, {"env", "TERMINAL_ENV", "docker"}}
	if got := savedWrites(g); !reflect.DeepEqual(got, want) {
		t.Fatalf("writes %+v", got)
	}
	if !strings.Contains(shown, "Docker found /usr/bin/docker") || !strings.Contains(shown, "Egress firewall enabled") {
		t.Fatalf("transcript\n%s", shown)
	}
	missing := terminalValues(map[string]any{"terminal.docker": map[string]any{"path": "", "runtime": ""}})
	g, shown = walkSection(t, terminalSection(), sectionLoad(map[string]any{}, map[string]any{}, []any{}, missing), "Docker", "Skip")
	if len(savedWrites(g)) != 2 || !strings.Contains(shown, "Docker or Podman not found in PATH") || !strings.Contains(shown, "Skipping egress firewall") {
		t.Fatalf("writes %+v transcript\n%s", savedWrites(g), shown)
	}
}

func TestTerminalSectionModalDirectReplacesTheTokenPair(t *testing.T) {
	load := sectionLoad(map[string]any{}, map[string]any{}, []any{"MODAL_TOKEN_ID", "MODAL_TOKEN_SECRET"}, terminalValues(nil))
	g, shown := walkSection(t, terminalSection(), load, "Modal", "Update", "new-id", "new-secret")
	want := [][3]any{{"config", "terminal.backend", "modal"}, {"env", "MODAL_TOKEN_ID", "new-id"},
		{"env", "MODAL_TOKEN_SECRET", "new-secret"}, {"env", "TERMINAL_ENV", "modal"}}
	if got := savedWrites(g); !reflect.DeepEqual(got, want) {
		t.Fatalf("writes %+v", got)
	}
	if !strings.Contains(shown, "Modal token already configured") {
		t.Fatalf("transcript\n%s", shown)
	}
	g, _ = walkSection(t, terminalSection(), load, "Modal", "Keep")
	if got := savedWrites(g); len(got) != 2 {
		t.Fatalf("kept tokens were written %+v", got)
	}
}

func TestTerminalSectionSSHDropsPort22AndTestsWithTheAnswers(t *testing.T) {
	load := sectionLoad(map[string]any{}, map[string]any{"TERMINAL_SSH_PORT": "2222"}, []any{}, terminalValues(nil))
	g, shown := walkSection(t, terminalSection(), load, "SSH", "box.test", "", "22", "", "Test")
	want := [][3]any{{"config", "terminal.backend", "ssh"}, {"env", "TERMINAL_SSH_HOST", "box.test"}, {"env", "TERMINAL_SSH_USER", "nick"},
		{"env", "TERMINAL_SSH_KEY", "/home/nick/.ssh/id_rsa"}, {"env", "TERMINAL_ENV", "ssh"}}
	if got := savedWrites(g); !reflect.DeepEqual(got, want) {
		t.Fatalf("writes %+v", got)
	}
	if got := savedRemovals(g); !reflect.DeepEqual(got, [][2]any{{"env", "TERMINAL_SSH_PORT"}}) {
		t.Fatalf("removals %+v", got)
	}
	params := map[string]any{"host": "box.test", "user": "nick", "port": "22", "key": "/home/nick/.ssh/id_rsa"}
	if len(g.actions) != 1 || g.actions[0]["action"] != "terminal.ssh.test" || !reflect.DeepEqual(g.actions[0]["params"], params) {
		t.Fatalf("actions %+v", g.actions)
	}
	if !strings.Contains(shown, "probe line") {
		t.Fatalf("transcript\n%s", shown)
	}
}

func TestTerminalSectionDaytonaKeepsAHeldKey(t *testing.T) {
	load := sectionLoad(map[string]any{}, map[string]any{}, []any{"DAYTONA_API_KEY"}, terminalValues(nil))
	g, shown := walkSection(t, terminalSection(), load, "Daytona", "Keep")
	if got := savedWrites(g); len(got) != 2 || !strings.Contains(shown, "Daytona API key already configured") {
		t.Fatalf("writes %+v transcript\n%s", got, shown)
	}
	g, shown = walkSection(t, terminalSection(), sectionLoad(map[string]any{}, map[string]any{}, []any{}, terminalValues(nil)), "Daytona", "dt-key")
	if got := savedWrites(g); !reflect.DeepEqual(got[1], [3]any{"env", "DAYTONA_API_KEY", "dt-key"}) || !strings.Contains(shown, "Daytona API key configured") {
		t.Fatalf("writes %+v transcript\n%s", got, shown)
	}
}

func TestTerminalSectionVercelUsesLinkedIdsAndKeepsTheResolvedToken(t *testing.T) {
	cwd, _ := os.Getwd()
	values := terminalValues(map[string]any{"terminal.vercel_linked:" + cwd: map[string]any{"projectId": "prj_L", "orgId": "team_L"}})
	held := map[string]any{"terminal.vercel_image": "", "terminal.vercel_runtime": "node22", "terminal.container_disk": 100, "terminal.container_cpu": 4,
		"terminal.container_memory": 5120, "terminal.container_persistent": true}
	g, shown := walkSection(t, terminalSection(), sectionLoad(held, map[string]any{}, []any{"VERCEL_TOKEN"}, values),
		"Vercel", "", "Do not persist", "", "", "", "", "")
	want := [][3]any{{"config", "terminal.container_disk", int64(51200)}, {"config", "terminal.backend", "vercel_sandbox"},
		{"config", "terminal.vercel_image", "vercel/sandbox/universal:latest"}, {"env", "TERMINAL_VERCEL_IMAGE", "vercel/sandbox/universal:latest"},
		{"config", "terminal.container_persistent", false}, {"config", "terminal.container_cpu", 4.0}, {"config", "terminal.container_memory", int64(5120)},
		{"env", "VERCEL_TOKEN", "held"}, {"env", "VERCEL_PROJECT_ID", "prj_L"}, {"env", "VERCEL_TEAM_ID", "team_L"}, {"env", "TERMINAL_ENV", "vercel_sandbox"}}
	if got := savedWrites(g); !reflect.DeepEqual(got, want) {
		t.Fatalf("writes %+v", got)
	}
	if got := savedRemovals(g); !reflect.DeepEqual(got, [][2]any{{"env", "VERCEL_OIDC_TOKEN"}}) {
		t.Fatalf("removals %+v", got)
	}
	for _, line := range []string{"terminal.vercel_runtime node22 is deprecated", "does not support custom disk sizing", "Found defaults in the nearest"} {
		if !strings.Contains(shown, line) {
			t.Fatalf("missing %q in\n%s", line, shown)
		}
	}
}

func TestTerminalSectionRegisteredBackendRunsItsSetupHook(t *testing.T) {
	g, _ := walkSection(t, terminalSection(), sectionLoad(map[string]any{}, map[string]any{}, []any{}, terminalValues(nil)), "Singularity")
	cwd, _ := os.Getwd()
	if len(g.actions) != 1 || g.actions[0]["action"] != "terminal.backend.plugin" || !reflect.DeepEqual(g.actions[0]["params"], map[string]any{"backend": "singularity", "cwd": cwd}) {
		t.Fatalf("actions %+v", g.actions)
	}
	if got := savedWrites(g); !reflect.DeepEqual(got, [][3]any{{"config", "terminal.backend", "singularity"}, {"env", "TERMINAL_ENV", "singularity"}}) {
		t.Fatalf("writes %+v", got)
	}
	shadow := terminalValues(map[string]any{"terminal.plugin_backends": []any{map[string]any{"name": "ssh", "display_name": "Plugin SSH", "description": "shadows ssh"}}})
	g, _ = walkSection(t, terminalSection(), sectionLoad(map[string]any{}, map[string]any{}, []any{}, shadow), "○ SSH")
	if len(g.actions) != 1 || g.actions[0]["params"].(map[string]any)["backend"] != "ssh" || len(savedWrites(g)) != 2 {
		t.Fatalf("a registered ssh backend did not take over, actions %+v writes %+v", g.actions, savedWrites(g))
	}
}
