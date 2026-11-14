//go:build windows

package codeexec

import (
	"encoding/json"
	"fmt"
	"os/exec"
	"strings"
	"testing"
	"time"
)

func TestChildCanCreateSocketWithScrubbedEnv(t *testing.T) {
	rt := hostRuntime(t)
	env := testEnv(t)
	scrubbed, err := scrubChildEnv(env, environMap(), scrubOptions{isPassthrough: func(string) bool { return false }})
	if err != nil {
		t.Fatal(err)
	}
	script := "import socket, sys\ntry:\n    s = socket.socket(socket.AF_INET, socket.SOCK_STREAM)\n    s.close()\n" +
		"    print(\"OK\")\n    sys.exit(0)\nexcept OSError as exc:\n    print(f\"FAIL: {exc}\")\n    sys.exit(1)"
	cmd := exec.Command(rt.Python, "-c", script)
	cmd.Env = envList(scrubbed)
	out, err := cmd.CombinedOutput()
	if err != nil || !strings.Contains(string(out), "OK") {
		t.Fatalf("child failed to create socket with scrubbed env: %v %s keys=%v", err, out, sortedKeys(scrubbed))
	}
}

func TestWindowsLiveChildOffsetMatchesOSZoneWhenTimezoneIsConfigured(t *testing.T) {
	withTimezone(t, "America/Los_Angeles")
	rt := hostRuntime(t)
	child, err := buildChildEnv(t.Context(), testEnv(t), childEnvSpec{RPCEndpoint: "socket", RPCToken: "token",
		TmpDir: "/tmp/icebath-code-execution-test"})
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := child["TZ"]; ok {
		t.Fatal("TZ handed to a Windows child")
	}
	control := environMap()
	delete(control, "TZ")
	stamp := fmt.Sprint(float64(time.Now().UnixNano()) / 1e9)
	script := "import datetime, json, sys, time; " +
		"instant = datetime.datetime.fromtimestamp(float(sys.argv[1]), datetime.timezone.utc); " +
		"print(json.dumps([time.timezone, instant.astimezone().utcoffset().total_seconds()]))"
	read := func(env map[string]string) []float64 {
		cmd := exec.Command(rt.Python, "-c", script, stamp)
		cmd.Env = envList(env)
		out, err := cmd.Output()
		if err != nil {
			t.Fatal(err)
		}
		var got []float64
		if err := json.Unmarshal(out, &got); err != nil {
			t.Fatal(err)
		}
		return got
	}
	a, b := read(child), read(control)
	if len(a) != 2 || a[0] != b[0] || a[1] != b[1] {
		t.Fatalf("child %v control %v", a, b)
	}
}
