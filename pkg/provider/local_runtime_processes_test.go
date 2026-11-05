package provider

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/nickheyer/icebath/pkg/tools"
)

const lr3HelperRoleEnv = "ICEBATH_LRT_HELPER_ROLE"

type lr3Identity struct {
	PID     int     `json:"pid"`
	Created float64 `json:"created"`
}

// Starts this test binary as a disposable helper process playing role under root
func lr3HelperCmd(root, role string) *exec.Cmd {
	cmd := exec.Command(os.Args[0], "-test.run=^TestLocalRuntimeProcessHelper$", "-test.v=false")
	cmd.Env = append(os.Environ(), lr3HelperRoleEnv+"="+role, "ICEBATH_LRT_HELPER_ROOT="+root,
		"ICEBATH_HOME="+filepath.Join(root, "home"))
	return cmd
}

// Atomically writes the pid and birth time of this process as root/role.json
func lr3Record(root, role string) {
	created, _ := tools.ProcessBirth(os.Getpid())
	raw, _ := json.Marshal(lr3Identity{PID: os.Getpid(), Created: created})
	tmp := filepath.Join(root, role+".tmp")
	_ = os.WriteFile(tmp, raw, 0o644)
	_ = os.Rename(tmp, filepath.Join(root, role+".json"))
}

func lr3Wait(predicate func() bool, timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if predicate() {
			return true
		}
		time.Sleep(30 * time.Millisecond)
	}
	return predicate()
}

func lr3Exists(path string) func() bool {
	return func() bool { _, err := os.Stat(path); return err == nil }
}

func lr3Read(t *testing.T, path string) lr3Identity {
	t.Helper()
	if !lr3Wait(lr3Exists(path), 15*time.Second) {
		t.Fatalf("missing child receipt: %s", path)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var id lr3Identity
	if err := json.Unmarshal(raw, &id); err != nil {
		t.Fatal(err)
	}
	return id
}

// Reports whether the recorded incarnation still runs and is not a zombie awaiting its reaper
func lr3Alive(id lr3Identity) bool {
	created, err := tools.ProcessBirth(id.PID)
	return err == nil && created == id.Created && !tools.ProcessZombie(id.PID)
}

func lr3Kill(id lr3Identity) {
	if lr3Alive(id) {
		if p, err := os.FindProcess(id.PID); err == nil {
			_ = p.Kill()
		}
	}
}

// Runs a helper role when launched by lr3HelperCmd and does nothing in a normal test run
func TestLocalRuntimeProcessHelper(t *testing.T) {
	role := os.Getenv(lr3HelperRoleEnv)
	if role == "" {
		return
	}
	root := os.Getenv("ICEBATH_LRT_HELPER_ROOT")
	lr3Record(root, role)
	switch role {
	case "owner":
		lr3OwnerRole(root)
	case "router":
		_ = lr3HelperCmd(root, "grandchild").Start()
		time.Sleep(90 * time.Second)
	case "lr7owner":
		lr7OwnerRole()
	case "lr7router":
		_ = lr3HelperCmd(root, "lr7child").Start()
		time.Sleep(90 * time.Second)
	default:
		time.Sleep(90 * time.Second)
	}
	os.Exit(0)
}

func TestServerChildEnvIsCaseInsensitiveAndPure(t *testing.T) {
	base := map[string]string{"openai_api_key": "k", "Path": "C:\\x", "GITHUB_TOKEN": "g", "HSA_OVERRIDE_GFX_VERSION": "11"}
	got := ServerChildEnv(base)
	if want := map[string]string{"Path": "C:\\x", "HSA_OVERRIDE_GFX_VERSION": "11"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("ServerChildEnv = %v, want %v", got, want)
	}
	if base["openai_api_key"] != "k" {
		t.Fatal("input mutated")
	}
}

func TestSpawnServerKeepsTheCallersEnvironment(t *testing.T) {
	cmd := lr3EnvEchoCmd("GITHUB_TOKEN")
	cmd.Env = []string{"GITHUB_TOKEN=g", "PATH=" + os.Getenv("PATH"), "SystemRoot=" + os.Getenv("SystemRoot")}
	var out strings.Builder
	cmd.Stdout = &out
	job, err := SpawnServer(cmd, false)
	if err != nil {
		t.Fatal(err)
	}
	defer job.Close()
	if err := cmd.Wait(); err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(out.String()) != "g" {
		t.Fatalf("child saw GITHUB_TOKEN=%q", out.String())
	}
}
