package tools

import (
	"context"
	"errors"
	"strings"
	"testing"
)

type probeFakeEnv struct {
	Environment
	output  string
	execErr error
	ran     []string
	cleaned []bool
}

func (e *probeFakeEnv) Execute(ctx context.Context, command string, opts ExecOptions) (*ExecResult, error) {
	e.ran = append(e.ran, command)
	if e.execErr != nil {
		return nil, e.execErr
	}
	return &ExecResult{Output: e.output, ReturnCode: 0}, nil
}

func (e *probeFakeEnv) Cleanup(forceRemove bool) error {
	e.cleaned = append(e.cleaned, forceRemove)
	return nil
}

type probeCreateCall struct {
	envType   string
	probeOnly bool
}

func fakeProbeSandbox(t *testing.T, backend string, fake *probeFakeEnv) *[]probeCreateCall {
	t.Helper()
	h := t.TempDir()
	t.Setenv("ICEBATH_HOME", h)
	t.Setenv("TERMINAL_ENV", backend)
	ClearBackendProbeCache()
	t.Cleanup(ClearBackendProbeCache)
	calls := &[]probeCreateCall{}
	prev := createEnvironmentFn
	createEnvironmentFn = func(ctx context.Context, env *Env, envType, image, cwd string, timeout int, sshConfig, containerConfig, localConfig map[string]any, taskID, hostCwd string, probeOnly bool) (Environment, error) {
		*calls = append(*calls, probeCreateCall{envType, probeOnly})
		return fake, nil
	}
	t.Cleanup(func() { createEnvironmentFn = prev })
	return calls
}

func TestRemoteBackendProbeCarriesNoUserHomeCwd(t *testing.T) {
	fake := &probeFakeEnv{output: "os=Linux\nkernel=6.8.0\nhome=/home/alice\ncwd=/srv/secret\nuser=alice\n"}
	fakeProbeSandbox(t, "docker", fake)
	hint, ok, err := ProbeRemoteBackend(context.Background(), &Env{Home: t.TempDir()}, "docker")
	if err != nil || !ok || !strings.Contains(hint, "OS: Linux 6.8.0") {
		t.Fatalf("probe %q %v %v", hint, ok, err)
	}
	for _, token := range []string{"whoami", "id -un", "$HOME", "pwd"} {
		if strings.Contains(fake.ran[0], token) {
			t.Fatalf("probe command asks for %s", token)
		}
	}
	for _, leaked := range []string{"User:", "Home:", "Working directory:", "alice", "/srv/secret"} {
		if strings.Contains(hint, leaked) {
			t.Fatalf("probe leaked %s in %q", leaked, hint)
		}
	}
}

func TestProbeRemoteBackendTearsDownItsSandbox(t *testing.T) {
	fake := &probeFakeEnv{output: "os=Linux\nkernel=6.8.0\nhome=/root\ncwd=/workspace\nuser=root\n"}
	fakeProbeSandbox(t, "docker", fake)
	if _, ok, err := ProbeRemoteBackend(context.Background(), nil, "docker"); err != nil || !ok {
		t.Fatalf("probe %v %v", ok, err)
	}
	if len(fake.cleaned) != 1 || !fake.cleaned[0] {
		t.Fatalf("cleanup calls %v", fake.cleaned)
	}
}

func TestProbeRemoteBackendTearsDownSandboxOnFailure(t *testing.T) {
	fake := &probeFakeEnv{execErr: errors.New("backend went away")}
	fakeProbeSandbox(t, "docker", fake)
	if _, ok, err := ProbeRemoteBackend(context.Background(), nil, "docker"); err != nil || ok {
		t.Fatalf("failed probe %v %v", ok, err)
	}
	if len(fake.cleaned) != 1 || !fake.cleaned[0] {
		t.Fatalf("cleanup calls %v", fake.cleaned)
	}
}

func TestProbeRemoteBackendCleansUpDaytonaOnce(t *testing.T) {
	fake := &probeFakeEnv{output: "os=Linux\nkernel=6.8.0\nhome=/home/u\ncwd=/home/u\nuser=u\n"}
	fakeProbeSandbox(t, "daytona", fake)
	if _, ok, err := ProbeRemoteBackend(context.Background(), nil, "daytona"); err != nil || !ok {
		t.Fatalf("probe %v %v", ok, err)
	}
	if len(fake.cleaned) != 1 {
		t.Fatalf("cleanup calls %v", fake.cleaned)
	}
}

func TestProbeRemoteBackendSSHIsProbeOnlyAndTornDown(t *testing.T) {
	fake := &probeFakeEnv{output: "os=Linux\nkernel=6.8.0\nhome=/home/u\ncwd=/home/u\nuser=u\n"}
	calls := fakeProbeSandbox(t, "ssh", fake)
	if _, ok, err := ProbeRemoteBackend(context.Background(), nil, "ssh"); err != nil || !ok {
		t.Fatalf("probe %v %v", ok, err)
	}
	if len(*calls) != 1 || !(*calls)[0].probeOnly || (*calls)[0].envType != "ssh" {
		t.Fatalf("create calls %+v", *calls)
	}
	if len(fake.cleaned) != 1 {
		t.Fatalf("cleanup calls %v", fake.cleaned)
	}
}
