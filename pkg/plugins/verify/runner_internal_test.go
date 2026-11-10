package verify

import (
	"context"
	"errors"
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/nickheyer/icebath/pkg/internal/pyjson"
)

func TestManifest(t *testing.T) {
	t.Run("roundtrip", func(t *testing.T) {
		root := t.TempDir()
		r := &Recipe{Name: "Next.js", Kind: "nextjs", Bootstrap: []string{"npm install"}, Build: []string{"npm run build"},
			Test: []string{"npm test"}, Start: strPtr("npm run dev"), Port: intPtr(3000), ReadinessPath: "/health"}
		path, err := SaveManifest(root, r)
		if err != nil || path != ManifestPath(root) || path != filepath.Join(root, ".icebath", "environment.json") {
			t.Fatalf("path %q err %v", path, err)
		}
		raw, _ := os.ReadFile(path)
		payload, err := pyjson.Loads(string(raw))
		if err != nil {
			t.Fatal(err)
		}
		obj := payload.(*pyjson.Object)
		eq(t, "version", obj.Vals["version"], int64(1))
		if _, ok := obj.Get("updatedAt"); !ok {
			t.Fatal("updatedAt missing")
		}
		loaded, err := LoadManifest(root)
		if err != nil {
			t.Fatal(err)
		}
		recipeEqual(t, loaded, r)
	})
	for name, text := range map[string]string{"missing_file": "", "malformed_json_tolerated": "{oops", "non_dict_tolerated": "[1, 2, 3]"} {
		t.Run(name, func(t *testing.T) {
			root := t.TempDir()
			if text != "" {
				writeFile(t, root, filepath.Join(".icebath", "environment.json"), text)
			}
			if r, err := LoadManifest(root); r != nil || err != nil {
				t.Fatalf("recipe %#v err %v", r, err)
			}
		})
	}
	t.Run("bare_recipe_shape_accepted", func(t *testing.T) {
		root := t.TempDir()
		writeFile(t, root, filepath.Join(".icebath", "environment.json"), `{"name": "Custom", "test": ["true"]}`)
		r, _ := LoadManifest(root)
		eq(t, "name", r.Name, "Custom")
		eq(t, "test", r.Test, []string{"true"})
	})
	t.Run("manifest_wins_over_detection", func(t *testing.T) {
		root := t.TempDir()
		writeFile(t, root, "go.mod", "module x\n")
		if _, err := SaveManifest(root, &Recipe{Name: "Custom", Kind: "custom", Test: []string{"true"}, ReadinessPath: "/"}); err != nil {
			t.Fatal(err)
		}
		r, source, err := LoadOrDetect(root)
		eq(t, "source", source, "manifest")
		eq(t, "name", r.Name, "Custom")
		eq(t, "err", err, nil)
	})
	t.Run("detection_fallback", func(t *testing.T) {
		root := t.TempDir()
		writeFile(t, root, "go.mod", "module x\n")
		r, source, _ := LoadOrDetect(root)
		eq(t, "source", source, "detected")
		eq(t, "kind", r.Kind, "go")
	})
}

func run(t *testing.T, root string, r *Recipe, mutate func(*VerifyOptions)) *VerifyResult {
	t.Helper()
	opts := NewVerifyOptions()
	if mutate != nil {
		mutate(&opts)
	}
	res, err := RunVerify(context.Background(), root, r, opts)
	if err != nil {
		t.Fatal(err)
	}
	return res
}

func skipStart(o *VerifyOptions) { o.SkipStart = true }

func TestRunner(t *testing.T) {
	t.Run("all_phases_pass", func(t *testing.T) {
		res := run(t, t.TempDir(), &Recipe{Name: "x", Bootstrap: []string{"true"}, Build: []string{"true"}, Test: []string{"true"}}, skipStart)
		if !res.OK() || len(res.Phases) != 3 {
			t.Fatalf("result %#v", res)
		}
		for i, p := range res.Phases {
			eq(t, "phase", p.Phase, PhaseOrder[i])
			eq(t, "exit", ptrInt(p.ExitCode), 0)
			if p.Duration < 0 {
				t.Fatal("negative duration")
			}
		}
	})
	t.Run("failure_stops_pipeline", func(t *testing.T) {
		res := run(t, t.TempDir(), &Recipe{Name: "x", Build: []string{"false"}, Test: []string{"true"}}, skipStart)
		if res.OK() || len(res.Phases) != 1 || *res.Phases[0].ExitCode != 1 {
			t.Fatalf("result %#v", res.Phases)
		}
	})
	t.Run("output_captured", func(t *testing.T) {
		var streamed string
		res := run(t, t.TempDir(), &Recipe{Name: "x", Test: []string{"echo hello-verify; echo err-verify 1>&2"}}, func(o *VerifyOptions) {
			o.SkipStart, o.OnOutput = true, func(s string) { streamed = s }
		})
		eq(t, "tail", res.Phases[0].OutputTail, "hello-verify\nerr-verify\n")
		eq(t, "streamed", streamed, "hello-verify\nerr-verify\n")
	})
	t.Run("phase_selection", func(t *testing.T) {
		res := run(t, t.TempDir(), &Recipe{Name: "x", Bootstrap: []string{"true"}, Build: []string{"true"}, Test: []string{"true"}},
			func(o *VerifyOptions) { o.Phases = []string{"test"} })
		if len(res.Phases) != 1 || res.Phases[0].Phase != "test" {
			t.Fatalf("phases %#v", res.Phases)
		}
	})
	t.Run("phase_timeout", func(t *testing.T) {
		started := time.Now()
		res := run(t, t.TempDir(), &Recipe{Name: "x", Test: []string{"echo partial; sleep 5"}}, func(o *VerifyOptions) {
			o.SkipStart, o.PhaseTimeout = true, 0.3
		})
		p := res.Phases[0]
		if res.OK() || !p.TimedOut || p.ExitCode != nil || p.OutputTail != "partial\n" || time.Since(started) > 4*time.Second {
			t.Fatalf("phase %#v after %s", p, time.Since(started))
		}
	})
	t.Run("commands_run_in_project_root", func(t *testing.T) {
		root := t.TempDir()
		writeFile(t, root, "marker.txt", "here")
		if !run(t, root, &Recipe{Name: "x", Test: []string{"cat marker.txt"}}, skipStart).OK() {
			t.Fatal("cat marker.txt failed in the project root")
		}
	})
	t.Run("output_tail_keeps_last_2000_code_points", func(t *testing.T) {
		res := run(t, t.TempDir(), &Recipe{Name: "x", Test: []string{"printf 'a%.0s' $(seq 1 2500); printf '\\303\\251\\r\\n'"}}, skipStart)
		tailText := res.Phases[0].OutputTail
		if !strings.HasSuffix(tailText, "aé\n") || len([]rune(tailText)) != 2000 {
			t.Fatalf("tail of %d code points ends %q", len([]rune(tailText)), tailText[len(tailText)-5:])
		}
	})
}

func composeRecipe() *Recipe {
	return &Recipe{Name: "docker-compose project", Kind: "compose", Build: []string{"docker compose build"},
		Start: strPtr("docker compose up"), ReadinessPath: "/", Evidence: []string{"Detected docker-compose.yml"}}
}

func patchCompose(t *testing.T, probe func(string) (*composeProbeResult, error)) *[]string {
	t.Helper()
	var phases []string
	prevProbe, prevPhase := composeProbe, runPhaseCommand
	composeProbe = probe
	runPhaseCommand = func(phase, command, root string, timeout float64, onOutput func(string)) (*PhaseResult, error) {
		phases = append(phases, phase)
		zero := 0
		return &PhaseResult{Phase: phase, Command: command, ExitCode: &zero}, nil
	}
	t.Cleanup(func() { composeProbe, runPhaseCommand = prevProbe, prevPhase })
	return &phases
}

func TestComposeGuard(t *testing.T) {
	refusals := map[string]struct {
		res *composeProbeResult
		err error
	}{
		"containers-running": {res: &composeProbeResult{Stdout: "myproject-db-1\nmyproject-web-1\n"}},
		"probe-timed-out":    {err: errComposeProbeTimeout},
		"probe-failed":       {res: &composeProbeResult{Returncode: 1, Stderr: "permission denied on docker.sock"}},
	}
	wantReason := map[string]string{
		"containers-running": "this compose project already has running container(s): myproject-db-1, myproject-web-1",
		"probe-timed-out":    "docker compose ps timed out after 15s; live containers cannot be ruled out",
		"probe-failed":       "docker compose ps failed (exit 1): permission denied on docker.sock",
	}
	for id, probe := range refusals {
		t.Run("refuses_"+id, func(t *testing.T) {
			calls := 0
			phases := patchCompose(t, func(string) (*composeProbeResult, error) { calls++; return probe.res, probe.err })
			res := run(t, t.TempDir(), composeRecipe(), nil)
			if res.OK() || *res.Phases[0].ExitCode != 1 || calls != 1 || len(*phases) != 0 || res.Readiness != nil {
				t.Fatalf("result %#v calls %d phases %v", res.Phases, calls, *phases)
			}
			if !strings.HasPrefix(res.Phases[0].OutputTail, "Refusing to run: "+wantReason[id]+". `docker compose build`") {
				t.Fatalf("tail %q", res.Phases[0].OutputTail)
			}
		})
	}
	for id, probe := range map[string]func(string) (*composeProbeResult, error){
		"none-running": func(string) (*composeProbeResult, error) { return &composeProbeResult{}, nil },
		"docker-absent": func(string) (*composeProbeResult, error) {
			return nil, &OSError{Class: "FileNotFoundError", Err: os.ErrNotExist}
		},
	} {
		t.Run("proceeds_"+id, func(t *testing.T) {
			phases := patchCompose(t, probe)
			run(t, t.TempDir(), composeRecipe(), func(o *VerifyOptions) { o.Phases = []string{"build"} })
			eq(t, "phases", *phases, []string{"build"})
		})
	}
	t.Run("guard_skipped_when_no_mutating_phase_selected", func(t *testing.T) {
		phases := patchCompose(t, nil)
		prev := composeLiveStateReason
		composeLiveStateReason = func(string) (string, error) { t.Fatal("compose probe ran"); return "", nil }
		t.Cleanup(func() { composeLiveStateReason = prev })
		run(t, t.TempDir(), &Recipe{Name: "x", Kind: "compose", Test: []string{"true"}}, func(o *VerifyOptions) { o.Phases = []string{"test"} })
		eq(t, "phases", *phases, []string{"test"})
	})
	t.Run("real_probe_without_docker_proceeds", func(t *testing.T) {
		t.Setenv("PATH", t.TempDir())
		reason, err := composeLiveStateReasonReal(t.TempDir())
		eq(t, "reason", reason, "")
		eq(t, "err", err, nil)
	})
	t.Run("result_to_dict", func(t *testing.T) {
		payload := run(t, t.TempDir(), &Recipe{Name: "x", Test: []string{"true"}}, skipStart).ToDict()
		eq(t, "ok", payload.Vals["ok"], true)
		eq(t, "recipe", payload.Vals["recipe"], "x")
		eq(t, "command", payload.Vals["phases"].([]any)[0].(*pyjson.Object).Vals["command"], "true")
		eq(t, "readiness", payload.Vals["readiness"], nil)
	})
}

func TestStartGating(t *testing.T) {
	t.Run("skip_start", func(t *testing.T) {
		res := run(t, t.TempDir(), &Recipe{Name: "x", Test: []string{"true"}, Start: strPtr("sleep 30"), Port: intPtr(1)}, skipStart)
		if res.Readiness != nil || !res.OK() {
			t.Fatalf("result %#v", res)
		}
	})
	t.Run("start_skipped_after_phase_failure", func(t *testing.T) {
		res := run(t, t.TempDir(), &Recipe{Name: "x", Test: []string{"false"}, Start: strPtr("sleep 30"), Port: intPtr(1)},
			func(o *VerifyOptions) { o.StopOnFailure = false })
		if res.Readiness != nil || res.OK() {
			t.Fatalf("result %#v", res)
		}
	})
	t.Run("readiness_url_refusals", func(t *testing.T) {
		for url, want := range map[string]string{
			"http://127.0.0.1:True/":  "nonnumeric port: 'True'",
			"http://127.0.0.1:80/a b": "URL can't contain control characters. '/a b' (found at least ' ')",
			"http://127.0.0.1:80/hé":  "'ascii' codec can't encode character '\\xe9' in position 6: ordinal not in range(128)",
		} {
			_, _, _, err := pollReadiness(url, 1, time.Millisecond)
			var re *RequestError
			if !errors.As(err, &re) || re.Msg != want {
				t.Errorf("%s gave %v, want %s", url, err, want)
			}
		}
	})
}

type fakeProcess struct {
	terminated, killed int
}

func (f *fakeProcess) pid() int                { return 999 }
func (f *fakeProcess) poll() bool              { return false }
func (f *fakeProcess) terminate() error        { f.terminated++; return nil }
func (f *fakeProcess) kill() error             { f.killed++; return nil }
func (f *fakeProcess) wait(time.Duration) bool { return true }

func TestTerminateSharedGroupChildGetsTerminateOnly(t *testing.T) {
	prevGet, prevKill := getpgid, killpg
	t.Cleanup(func() { getpgid, killpg = prevGet, prevKill })
	var calls [][2]any
	getpgid = func(int) (int, error) { return 555, nil }
	killpg = func(pgid int, force bool) error { calls = append(calls, [2]any{pgid, force}); return nil }
	proc := &fakeProcess{}
	if err := terminateProcessGroup(proc); err != nil {
		t.Fatal(err)
	}
	if len(calls) != 0 || proc.terminated != 1 {
		t.Fatalf("killpg %v terminate %d", calls, proc.terminated)
	}
}

func TestHooks(t *testing.T) {
	nudges := func(cfg map[string]any) int {
		n, err := MaxVerifyNudges(cfg)
		if err != nil {
			t.Fatal(err)
		}
		return n
	}
	agent := func(k string, v any) map[string]any { return map[string]any{"agent": map[string]any{k: v}} }
	t.Run("default_when_unset", func(t *testing.T) {
		eq(t, "empty", nudges(map[string]any{}), DefaultMaxVerifyNudges)
		eq(t, "agent", nudges(map[string]any{"agent": map[string]any{}}), DefaultMaxVerifyNudges)
	})
	t.Run("reads_and_coerces", func(t *testing.T) {
		eq(t, "5", nudges(agent("max_verify_nudges", 5)), 5)
		eq(t, "'2'", nudges(agent("max_verify_nudges", "2")), 2)
		eq(t, "-1", nudges(agent("max_verify_nudges", -1)), 0)
		eq(t, "2.9", nudges(agent("max_verify_nudges", 2.9)), 2)
	})
	t.Run("bad_value_falls_back", func(t *testing.T) {
		eq(t, "x", nudges(agent("max_verify_nudges", "x")), DefaultMaxVerifyNudges)
	})
	t.Run("infinity_raises_overflow", func(t *testing.T) {
		inf := math.Inf(1)
		if _, err := MaxVerifyNudges(agent("max_verify_nudges", inf)); err == nil || err.Error() != "cannot convert float infinity to integer" {
			t.Fatalf("err %v", err)
		}
	})
	t.Run("loads_config_when_none_given", func(t *testing.T) {
		prev := loadConfig
		t.Cleanup(func() { loadConfig = prev })
		loadConfig = func() (map[string]any, error) { return agent("max_verify_nudges", 7), nil }
		eq(t, "loaded", nudges(nil), 7)
		loadConfig = func() (map[string]any, error) { return nil, errors.New("broken") }
		eq(t, "broken", nudges(nil), DefaultMaxVerifyNudges)
	})
	t.Run("guidance_enabled_by_default", func(t *testing.T) {
		eq(t, "empty", CodingVerifyGuidance(map[string]any{}), CodingVerifyGuidanceText)
		eq(t, "agent", CodingVerifyGuidance(map[string]any{"agent": map[string]any{}}), CodingVerifyGuidanceText)
	})
	t.Run("guidance_reads_truthy_config", func(t *testing.T) {
		eq(t, "yes", CodingVerifyGuidance(agent("verify_guidance", "yes")), CodingVerifyGuidanceText)
	})
	t.Run("guidance_opt_out_via_config", func(t *testing.T) {
		eq(t, "false", CodingVerifyGuidance(agent("verify_guidance", false)), "")
		eq(t, "null", CodingVerifyGuidance(agent("verify_guidance", nil)), CodingVerifyGuidanceText)
	})
}

func TestBuildVerifyParser(t *testing.T) {
	p := BuildVerifyParser()
	eq(t, "name", p.Name, "verify")
	eq(t, "timeout help", p.Arguments[6].Help, "Per-phase timeout in seconds (default: 600)")
	eq(t, "ready help", p.Arguments[7].Help, "Readiness poll timeout in seconds (default: 60)")
	eq(t, "choices", p.Arguments[4].Choices, []string{"bootstrap", "build", "test", "start"})
}

func TestRunnableRecipeInputsOfTheVerifyNudge(t *testing.T) {
	workspace := func(scripts *pyjson.Object, manifest string) string {
		root := t.TempDir()
		pkg := pyjson.NewObject()
		if scripts != nil {
			pkg = pyjson.ObjectOf("scripts", scripts)
		}
		writePkg(t, root, pkg)
		if manifest != "" {
			writeFile(t, root, filepath.Join(".icebath", "environment.json"), manifest)
		}
		return root
	}
	withStart := detect(t, workspace(pyjson.ObjectOf("test", "vitest", "dev", "vite"), ""))
	eq(t, "start", ptrStr(withStart.Start), "npm run dev")
	eq(t, "test", withStart.Test, []string{"npm run test"})
	eq(t, "no start", ptrStr(detect(t, workspace(pyjson.ObjectOf("test", "vitest"), "")).Start), nil)
	info, err := os.Stat(ManifestPath(workspace(pyjson.ObjectOf("test", "vitest"), `{"version": 1, "recipe": {"name": "Fake", "test": ["echo ok"]}}`)))
	if err != nil || !info.Mode().IsRegular() {
		t.Fatalf("manifest stat %v %v", info, err)
	}
}
