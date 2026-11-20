package config

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func stubContainerSeams(t *testing.T, which func(string) (string, error), probe func([]string) (int, bool, error), tty bool) (*[][]string, *bytes.Buffer) {
	t.Helper()
	prevWhich, prevProbe, prevTTY, prevExec, prevErr := containerWhich, containerProbeRun, containerStdinIsTTY, containerExecvp, containerStderr
	t.Cleanup(func() {
		containerWhich, containerProbeRun, containerStdinIsTTY, containerExecvp, containerStderr = prevWhich, prevProbe, prevTTY, prevExec, prevErr
	})
	execs := &[][]string{}
	errBuf := &bytes.Buffer{}
	containerWhich, containerProbeRun, containerStderr = which, probe, errBuf
	containerStdinIsTTY = func() bool { return tty }
	containerExecvp = func(argv []string) (int, error) { *execs = append(*execs, argv); return 0, nil }
	return execs, errBuf
}

func dockerContainerInfo() *ContainerExecInfo {
	return &ContainerExecInfo{Backend: "docker", ContainerName: "icebath-agent", ExecUser: "icebath", IcebathBin: "/data/current-package/bin/icebath"}
}

func TestGetContainerExecInfoReturnsMetadata(t *testing.T) {
	h := filepath.Join(t.TempDir(), ".icebath")
	if err := os.MkdirAll(h, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("ICEBATH_DEV", "")
	if err := os.WriteFile(filepath.Join(h, ".container-mode"), []byte("# Written by NixOS activation script. Do not edit manually.\nbackend=podman\ncontainer_name=icebath-agent\nexec_user=icebath\nicebath_bin=/data/current-package/bin/icebath\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	info, err := GetContainerExecInfo(h)
	if err != nil || info == nil {
		t.Fatalf("info %v err %v", info, err)
	}
	if info.Backend != "podman" || info.ContainerName != "icebath-agent" || info.ExecUser != "icebath" || info.IcebathBin != "/data/current-package/bin/icebath" {
		t.Fatalf("info %+v", *info)
	}
}

func TestExecInContainerCallsExecvp(t *testing.T) {
	execs, _ := stubContainerSeams(t, func(string) (string, error) { return "/usr/bin/docker", nil },
		func([]string) (int, bool, error) { return 0, false, nil }, true)
	t.Setenv("TERM", "xterm-256color")
	t.Setenv("LANG", "en_US.UTF-8")
	code, err := ExecInContainer(dockerContainerInfo(), []string{"chat", "-m", "opus"})
	if err != nil || code != 0 || len(*execs) != 1 {
		t.Fatalf("code %d err %v execs %v", code, err, *execs)
	}
	cmd := (*execs)[0]
	if cmd[0] != "/usr/bin/docker" || cmd[1] != "exec" || !slices.Contains(cmd, "-it") {
		t.Fatalf("cmd %v", cmd)
	}
	if idx := slices.Index(cmd, "-u"); idx < 0 || cmd[idx+1] != "icebath" {
		t.Fatalf("user flag in %v", cmd)
	}
	var envValues []string
	for i, v := range cmd {
		if v == "-e" {
			envValues = append(envValues, cmd[i+1])
		}
	}
	if !slices.Contains(envValues, "TERM=xterm-256color") || !slices.Contains(envValues, "LANG=en_US.UTF-8") {
		t.Fatalf("env flags %v", envValues)
	}
	for _, want := range []string{"icebath-agent", "/data/current-package/bin/icebath", "chat"} {
		if !slices.Contains(cmd, want) {
			t.Fatalf("%s missing from %v", want, cmd)
		}
	}
}

func TestExecInContainerPipedStdinUsesInteractiveOnly(t *testing.T) {
	execs, _ := stubContainerSeams(t, func(string) (string, error) { return "/usr/bin/docker", nil },
		func([]string) (int, bool, error) { return 0, false, nil }, false)
	if _, err := ExecInContainer(dockerContainerInfo(), nil); err != nil {
		t.Fatal(err)
	}
	cmd := (*execs)[0]
	if cmd[2] != "-i" || slices.Contains(cmd, "-it") {
		t.Fatalf("tty flag in %v", cmd)
	}
}

func TestExecInContainerMissingRuntimeExitsOne(t *testing.T) {
	execs, errBuf := stubContainerSeams(t, func(string) (string, error) { return "", errors.New("not found") },
		func([]string) (int, bool, error) { return 0, false, nil }, true)
	code, err := ExecInContainer(dockerContainerInfo(), []string{"chat"})
	if err != nil || code != 1 || len(*execs) != 0 {
		t.Fatalf("code %d err %v execs %v", code, err, *execs)
	}
	if errBuf.String() != "Error: docker not found on PATH. Cannot route to container.\n" {
		t.Fatalf("stderr %q", errBuf.String())
	}
}

func TestExecInContainerProbeTimeoutExitsOne(t *testing.T) {
	_, errBuf := stubContainerSeams(t, func(string) (string, error) { return "/usr/bin/podman", nil },
		func([]string) (int, bool, error) { return 0, true, nil }, true)
	code, err := ExecInContainer(&ContainerExecInfo{Backend: "podman", ContainerName: "c", ExecUser: "u", IcebathBin: "b"}, nil)
	if err != nil || code != 1 {
		t.Fatalf("code %d err %v", code, err)
	}
	if errBuf.String() != "Error: timed out waiting for podman to respond.\nThe podman daemon may be unresponsive or starting up.\n" {
		t.Fatalf("stderr %q", errBuf.String())
	}
}

func TestExecInContainerRootfulContainerRoutesThroughSudo(t *testing.T) {
	var probes [][]string
	execs, _ := stubContainerSeams(t, func(name string) (string, error) { return "/usr/bin/" + name, nil },
		func(argv []string) (int, bool, error) {
			probes = append(probes, argv)
			if argv[0] == "/usr/bin/sudo" {
				return 0, false, nil
			}
			return 1, false, nil
		}, true)
	code, err := ExecInContainer(dockerContainerInfo(), []string{"chat"})
	if err != nil || code != 0 || len(*execs) != 1 {
		t.Fatalf("code %d err %v execs %v", code, err, *execs)
	}
	if len(probes) != 2 || probes[1][0] != "/usr/bin/sudo" || probes[1][1] != "-n" || probes[1][2] != "/usr/bin/docker" {
		t.Fatalf("probes %v", probes)
	}
	cmd := (*execs)[0]
	if cmd[0] != "/usr/bin/sudo" || cmd[1] != "-n" || cmd[2] != "/usr/bin/docker" || cmd[3] != "exec" {
		t.Fatalf("cmd %v", cmd)
	}
}

func TestExecInContainerRootfulWithoutSudoExplains(t *testing.T) {
	_, errBuf := stubContainerSeams(t, func(name string) (string, error) {
		if name == "sudo" {
			return "", errors.New("not found")
		}
		return "/usr/bin/docker", nil
	}, func([]string) (int, bool, error) { return 1, false, nil }, true)
	code, _ := ExecInContainer(dockerContainerInfo(), []string{"chat", "-q", "hi"})
	if code != 1 || !strings.Contains(errBuf.String(), "Try: sudo icebath chat -q hi") {
		t.Fatalf("code %d stderr %q", code, errBuf.String())
	}
}

func TestExecInContainerSudoProbeFailureExplainsNixOSRule(t *testing.T) {
	_, errBuf := stubContainerSeams(t, func(name string) (string, error) { return "/usr/bin/" + name, nil },
		func([]string) (int, bool, error) { return 1, false, nil }, true)
	t.Setenv("USER", "nick")
	code, _ := ExecInContainer(dockerContainerInfo(), []string{"chat"})
	out := errBuf.String()
	if code != 1 || !strings.Contains(out, `users = [ "nick" ];`) || !strings.Contains(out, `command = "/usr/bin/docker"; options = [ "NOPASSWD" ];`) || !strings.HasSuffix(out, "Or run: sudo icebath chat\n") {
		t.Fatalf("code %d stderr %q", code, out)
	}
}
