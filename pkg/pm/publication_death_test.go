package pm

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

const pubHelperEnv = "PUB_PM_HELPER"

// Re-executed test binary entry for publication children the parent test chose
func TestPubHelperProcess(t *testing.T) {
	mode := os.Getenv(pubHelperEnv)
	if mode == "" {
		return
	}
	var args map[string]string
	json.Unmarshal([]byte(os.Getenv(pubHelperEnv+"_ARGS")), &args)
	switch mode {
	case "death-worker":
		uvToolchain = func(context.Context, bool, bool) (string, string, bool, error) {
			return args["uv"], args["python"], true, nil
		}
		wsHost(t, "")
		phase := args["phase"]
		crash := func() { os.Exit(17) }
		write := publicationWrite
		publicationWrite = func(path string, data []byte) error {
			err := write(path, data)
			if phase == "journal" && filepath.Base(path) == "publication.json" ||
				phase == "payload" && path == args["payload"] ||
				phase == "commit" && filepath.Base(path) == "publication.json" && strings.Contains(string(data), "\"committed\": true") {
				crash()
			}
			return err
		}
		rename := publicationRename
		publicationRename = func(source, target string) error {
			err := rename(source, target)
			if phase == "backup" && strings.HasPrefix(filepath.Base(target), ".previous-") || phase == "tree" && target == args["target"] {
				crash()
			}
			return err
		}
		if phase == "commit" && args["rebuild"] == "true" {
			record := recordVenvState
			recordVenvState = func(f *Facts, name, stamp string, extras []string, environment, resolvedLock string) error {
				record(f, name, stamp, extras, environment, resolvedLock)
				crash()
				return nil
			}
		}
		os.Exit(WorkerMain())
	case "fault-worker":
		uvToolchain = func(context.Context, bool, bool) (string, string, bool, error) {
			return args["uv"], args["python"], true, nil
		}
		wsHost(t, "")
		pubSwapVenv(t, &pubVenv{prepare: func(context.Context, []string, VenvApplyOptions) (*VenvGeneration, error) {
			if args["fault"] == "conflict" {
				fmt.Println("engine stdout")
				os.Stdout.Sync()
				os.NewFile(1, "native").Write([]byte("native stdout\n"))
				return nil, &ResolutionConflict{NewInstallError("venv", "impossible union", "change member")}
			}
			return nil, nil
		}})
		if args["fault"] == "facts" {
			recordVenvState = func(*Facts, string, string, []string, string, string) error {
				if data, _ := os.ReadFile(args["config"]); !strings.Contains(string(data), "new") {
					return &PyError{Class: "AssertionError", Message: "selection not published before the facts write"}
				}
				return &PyError{Class: "OSError", Message: "facts disk full"}
			}
		}
		os.Exit(WorkerMain())
	case "activate":
		ctx := context.Background()
		for range 2 {
			if err := ActivateDependencies(ctx, args["project"]); err != nil {
				fmt.Fprintln(os.Stderr, err)
				os.Exit(1)
			}
		}
		os.Exit(0)
	}
}

// Command re-executing this test binary as the publication helper child in a mode with string arguments
func pubHelper(mode string, args map[string]string) *exec.Cmd {
	encoded, _ := json.Marshal(args)
	cmd := exec.Command(os.Args[0], "-test.run=^TestPubHelperProcess$", "-test.count=1")
	cmd.Env = append(os.Environ(), pubHelperEnv+"="+mode, pubHelperEnv+"_ARGS="+string(encoded))
	return cmd
}

func TestPublicationWorkerDeathRecoversAtEachDurablePublicationBoundary(t *testing.T) {
	type row struct {
		kind    string
		rebuild bool
		phase   string
	}
	var rows []row
	for _, kind := range []string{"selection", "tree"} {
		for _, rebuild := range []bool{false, true} {
			for _, phase := range []string{"journal", "payload", "commit"} {
				rows = append(rows, row{kind, rebuild, phase})
			}
		}
	}
	for _, rebuild := range []bool{false, true} {
		for _, phase := range []string{"backup", "tree"} {
			rows = append(rows, row{"tree", rebuild, phase})
		}
	}
	for _, r := range rows {
		t.Run(fmt.Sprintf("%s/rebuild=%v/%s", r.kind, r.rebuild, r.phase), func(t *testing.T) {
			tmp, homeDir := pubClient(t)
			ctx := context.Background()
			project := pubLockedProject(t, tmp, "death-proof")
			target := filepath.Join(homeDir, "plugins", "example")
			peWrite(t, filepath.Join(target, "plugin.yaml"), "name: example\n")
			peWrite(t, filepath.Join(target, "code.py"), "old")
			config := filepath.Join(homeDir, "config.yaml")
			enabled := "[]"
			if r.kind == "tree" {
				enabled = "[example]"
			}
			peWrite(t, config, "# exact bytes\nplugins:\n  enabled: "+enabled+"\n")
			metadata := filepath.Join(homeDir, "plugins", ".install-metadata.json")
			peWrite(t, metadata, "{\"example\":{\"revision\":\"old\"}}\n")
			if err := SyncVenv(ctx, SyncVenvOptions{Explicit: true}); err != nil {
				t.Fatal(err)
			}
			beforeEnv := svSelected(t, project)
			facts := pubState(t, project, "facts.json")
			before := map[string]string{}
			for _, path := range []string{config, metadata, facts} {
				before[path] = peRead(t, path)
			}
			beforeTree, _ := TreeDigest(target)
			var plugins PluginInput
			payload := config
			if r.kind == "selection" {
				if r.rebuild {
					svInstallingMember(t, target, "example", filepath.Join(tmp, "wheels"), "")
				}
				plugins = &Selection{Data: map[string]any{"home": homeDir, "enabled": pubList("example"), "disabled": pubList()}}
			} else {
				staged := filepath.Join(tmp, "staged")
				if err := copyTreeLike(target, staged, false, nil); err != nil {
					t.Fatal(err)
				}
				peWrite(t, filepath.Join(staged, "code.py"), "new")
				if r.rebuild {
					svInstallingMember(t, staged, "example", filepath.Join(tmp, "wheels"), "")
				}
				plugins = &StagedUpdate{Data: map[string]any{
					"target": target, "staged": staged, "target_digest": beforeTree,
					"old_metadata": map[string]any{"example": map[string]any{"revision": "old"}},
					"new_metadata": map[string]any{"example": map[string]any{"revision": "new"}},
				}}
				payload = metadata
			}
			uv, python, _, _ := uvToolchain(ctx, true, true)
			args := map[string]string{"uv": uv, "python": python, "phase": r.phase, "payload": payload, "target": target, "rebuild": fmt.Sprint(r.rebuild)}
			peSwap(t, &repoRoot, func() string { return filepath.Join(tmp, "elsewhere") })
			encoded, _ := json.Marshal(args)
			t.Setenv(pubHelperEnv, "death-worker")
			t.Setenv(pubHelperEnv+"_ARGS", string(encoded))
			peSwap(t, &workerArgv, func() ([]string, error) {
				return []string{os.Args[0], "-test.run=^TestPubHelperProcess$", "-test.count=1"}, nil
			})
			err := SyncVenv(ctx, SyncVenvOptions{ProjectRoot: project, Explicit: true, Plugins: plugins})
			ieWantInstallError(t, err, "worker exited without a result")
			if !pathExists(pubState(t, project, "publication.json")) {
				t.Fatal("the dead worker left no journal")
			}
			if out, err := pubHelper("activate", map[string]string{"project": project}).CombinedOutput(); err != nil {
				t.Fatalf("recovery %s %v", out, err)
			}
			committed := r.phase == "commit"
			if (svSelected(t, project) != beforeEnv) != (committed && r.rebuild) {
				t.Fatalf("selected environment moved %v", svSelected(t, project) != beforeEnv)
			}
			if !committed {
				for path, data := range before {
					if peRead(t, path) != data {
						t.Fatalf("%s changed to %q", path, peRead(t, path))
					}
				}
			}
			if r.kind == "tree" {
				want := "old"
				if committed {
					want = "new"
				}
				if peRead(t, filepath.Join(target, "code.py")) != want {
					t.Fatalf("tree holds %q", peRead(t, filepath.Join(target, "code.py")))
				}
				if now, _ := TreeDigest(target); !committed && now != beforeTree {
					t.Fatal("rolled back tree differs")
				}
			} else {
				want := []string{}
				if committed {
					want = []string{"example"}
				}
				if got := svList(svAt(svConfig(t, config), "plugins", "enabled")); strings.Join(got, ",") != strings.Join(want, ",") {
					t.Fatalf("enabled %v", got)
				}
			}
			if pathExists(pubState(t, project, "publication.json")) {
				t.Fatal("journal survived recovery")
			}
			if left, _ := filepath.Glob(filepath.Join(homeDir, "plugins", ".previous-*")); len(left) != 0 {
				t.Fatalf("backups left %v", left)
			}
		})
	}
}
