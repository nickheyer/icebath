package tools

import "testing"

func TestDockerExeExposesTheDriverBinary(t *testing.T) {
	d := &dockerEnv{dockerExe: "/usr/bin/podman", containerID: "c1"}
	if d.DockerExe() != "/usr/bin/podman" || d.ContainerID() != "c1" {
		t.Fatalf("%q %q", d.DockerExe(), d.ContainerID())
	}
}

func TestResetDockerExecutableForgetsTheCachedPath(t *testing.T) {
	dockerExecutable.Lock()
	saved := dockerExecutable.path
	dockerExecutable.path = "/cached/docker"
	dockerExecutable.Unlock()
	t.Cleanup(func() { dockerExecutable.Lock(); dockerExecutable.path = saved; dockerExecutable.Unlock() })
	ResetDockerExecutable()
	dockerExecutable.Lock()
	defer dockerExecutable.Unlock()
	if dockerExecutable.path != "" {
		t.Fatalf("cached path %q survived the reset", dockerExecutable.path)
	}
}
