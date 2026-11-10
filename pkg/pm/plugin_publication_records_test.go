package pm

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// Runs a publication helper child over a bound home and working directory, returning its status and stderr
func pubRunChild(t *testing.T, mode, tmp, homeDir string, args map[string]string) (int, string) {
	t.Helper()
	cmd := pubHelper(mode, args)
	cmd.Env = append(cmd.Env, "ICEBATH_HOME="+homeDir)
	cmd.Dir = tmp
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	err := cmd.Run()
	if cmd.ProcessState == nil {
		t.Fatal(err)
	}
	return cmd.ProcessState.ExitCode(), stderr.String()
}

// Publishes one staged plugin in its own process the way the Python program does
func pubPublishChild(t *testing.T, tmp, homeDir, project string, plugin map[string]any, extra map[string]string) (int, string) {
	t.Helper()
	encoded, err := json.Marshal(plugin)
	if err != nil {
		t.Fatal(err)
	}
	args := map[string]string{"plugin": string(encoded), "project": project}
	for key, value := range extra {
		args[key] = value
	}
	return pubRunChild(t, "staged-publish", tmp, homeDir, args)
}

// Stages a plugin tree naming itself and publishes it over the home's plugins dir with the given metadata views
func recordsPublish(t *testing.T, tmp, homeDir, name string, old, fresh map[string]any) (int, string) {
	t.Helper()
	staged := filepath.Join(tmp, "staged-"+name)
	writeFile(t, filepath.Join(staged, "plugin.yaml"), []byte("name: "+name+"\n"), 0o644)
	project := filepath.Join(tmp, "project")
	if err := os.MkdirAll(project, 0o777); err != nil {
		t.Fatal(err)
	}
	return pubPublishChild(t, tmp, homeDir, project, map[string]any{
		"staged": staged, "target": filepath.Join(homeDir, "plugins", name), "target_digest": nil,
		"old_metadata": old, "new_metadata": fresh,
	}, nil)
}

// Decoded plugin install metadata sidecar
func recordsRead(t *testing.T, path string) map[string]any {
	t.Helper()
	var records map[string]any
	if err := json.Unmarshal(readFile(t, path), &records); err != nil {
		t.Fatal(err)
	}
	return records
}

func TestSecondInstallKeepsTheFirstInstallRecord(t *testing.T) {
	tmp := isolateHome(t)
	homeDir := filepath.Join(tmp, "home")
	if err := os.MkdirAll(filepath.Join(homeDir, "plugins"), 0o777); err != nil {
		t.Fatal(err)
	}
	metadata := filepath.Join(homeDir, "plugins", ".install-metadata.json")
	first := map[string]any{"source": "https://example.test/nvidia-app.git", "revision": strings.Repeat("a", 40), "pinned": false}
	second := map[string]any{"source": "https://example.test/nvidia-broadcast.git", "revision": strings.Repeat("b", 40), "pinned": false}
	if code, stderr := recordsPublish(t, tmp, homeDir, "nvidia-app", map[string]any{}, map[string]any{"nvidia-app": first}); code != 0 {
		t.Fatalf("first publish exited %d: %s", code, stderr)
	}
	if code, stderr := recordsPublish(t, tmp, homeDir, "nvidia-broadcast", map[string]any{}, map[string]any{"nvidia-broadcast": second}); code != 0 {
		t.Fatalf("second publish exited %d: %s", code, stderr)
	}
	if got := recordsRead(t, metadata); !reflect.DeepEqual(got, map[string]any{"nvidia-app": first, "nvidia-broadcast": second}) {
		t.Fatalf("metadata %v", got)
	}
}

func TestAConcurrentChangeToTheSamePluginRecordIsStillRefused(t *testing.T) {
	tmp := isolateHome(t)
	homeDir := filepath.Join(tmp, "home")
	metadata := filepath.Join(homeDir, "plugins", ".install-metadata.json")
	writeFile(t, metadata, []byte("{\"example\": {\"revision\": \"moved\"}}\n"), 0o644)
	code, stderr := recordsPublish(t, tmp, homeDir, "example",
		map[string]any{"example": map[string]any{"revision": "old"}}, map[string]any{"example": map[string]any{"revision": "new"}})
	if code == 0 || !strings.Contains(stderr, "metadata changed") {
		t.Fatalf("publish exited %d: %s", code, stderr)
	}
	if got := recordsRead(t, metadata); !reflect.DeepEqual(got, map[string]any{"example": map[string]any{"revision": "moved"}}) {
		t.Fatalf("metadata %v", got)
	}
}

func TestBootRecoversPluginPublicationAfterProcessDeath(t *testing.T) {
	for _, committed := range []bool{false, true} {
		t.Run(fmt.Sprint(committed), func(t *testing.T) {
			tmp := isolateHome(t)
			homeDir := filepath.Join(tmp, "home")
			target := filepath.Join(homeDir, "plugins", "example")
			writeFile(t, filepath.Join(target, "__init__.py"), []byte("old code"), 0o644)
			metadata := filepath.Join(homeDir, "plugins", ".install-metadata.json")
			writeFile(t, metadata, []byte("{\"example\":{\"revision\":\"old\"}}\n"), 0o644)
			previous := readFile(t, metadata)
			staged := filepath.Join(tmp, "staged")
			writeFile(t, filepath.Join(staged, "__init__.py"), []byte("new code"), 0o644)
			project := filepath.Join(tmp, "project")
			if err := os.MkdirAll(project, 0o777); err != nil {
				t.Fatal(err)
			}
			digest, err := TreeDigest(target)
			if err != nil {
				t.Fatal(err)
			}
			code, stderr := pubPublishChild(t, tmp, homeDir, project, map[string]any{
				"staged": staged, "target": target, "target_digest": digest,
				"old_metadata": map[string]any{"example": map[string]any{"revision": "old"}},
				"new_metadata": map[string]any{"example": map[string]any{"revision": "new"}},
			}, map[string]string{"committed": fmt.Sprint(committed), "exit": "17"})
			if code != 17 {
				t.Fatalf("publisher exited %d: %s", code, stderr)
			}
			if code, stderr := pubRunChild(t, "recover", tmp, homeDir, map[string]string{"project": project}); code != 0 {
				t.Fatalf("recovery exited %d: %s", code, stderr)
			}
			wantCode, revision := "old code", "old"
			if committed {
				wantCode, revision = "new code", "new"
			}
			if got := string(readFile(t, filepath.Join(target, "__init__.py"))); got != wantCode {
				t.Fatalf("plugin code %q, want %q", got, wantCode)
			}
			records := recordsRead(t, metadata)
			if example, _ := records["example"].(map[string]any); example["revision"] != revision {
				t.Fatalf("metadata %v", records)
			}
			if !committed && !bytes.Equal(readFile(t, metadata), previous) {
				t.Fatal("an uncommitted publication left rewritten metadata bytes")
			}
			if left := ieGlob(t, filepath.Dir(target), ".previous-"); len(left) > 0 {
				t.Fatalf("recovery left %v", left)
			}
		})
	}
}
