package tools

import (
	"sync"
	"testing"
)

// Docker host access follows host bind mounts and the cwd workspace mount flag
func TestDockerHasHostAccessExportFollowsTheMountConfig(t *testing.T) {
	cases := []struct {
		cfg  map[string]any
		want bool
	}{
		{map[string]any{"env_type": "local", "docker_volumes": []any{"/host:/c"}}, false},
		{map[string]any{"env_type": "docker", "host_cwd": "/repo", "docker_mount_cwd_to_workspace": true}, true},
		{map[string]any{"env_type": "docker", "host_cwd": "/repo", "docker_mount_cwd_to_workspace": false}, false},
		{map[string]any{"env_type": "docker", "docker_volumes": []any{"/host/data:/data"}}, true},
		{map[string]any{"env_type": "docker", "docker_volumes": []any{"named:/data"}}, false},
	}
	for _, c := range cases {
		if got := DockerHasHostAccess(c.cfg); got != c.want {
			t.Fatalf("DockerHasHostAccess(%v) = %v", c.cfg, got)
		}
	}
}

func TestSessionEnvReadsSessionValuesThenProcessEnv(t *testing.T) {
	t.Setenv("ICEBATH_SESSION_KEY", "from-process")
	if got := SessionEnv(nil, "ICEBATH_SESSION_KEY"); got != "from-process" {
		t.Fatalf("process env fallback got %q", got)
	}
	env := &Env{Values: &sync.Map{}}
	env.Values.Store("session.key", "from-values")
	if got := SessionEnv(env, "ICEBATH_SESSION_KEY"); got != "from-values" || got != sessionContextValue(env, "ICEBATH_SESSION_KEY") {
		t.Fatalf("session value got %q", got)
	}
}

func TestResolveContainerTaskIDExportMatchesTheCacheKey(t *testing.T) {
	env := newTerminalTestEnv(t, "")
	got, err := ResolveContainerTaskID(env, "task-export")
	want, werr := resolveContainerTaskID(env, "task-export")
	if err != nil || werr != nil || got != want || got == "" {
		t.Fatalf("ResolveContainerTaskID = %q %v, want %q %v", got, err, want, werr)
	}
}
