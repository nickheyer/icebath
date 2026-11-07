package tools

import "testing"

func TestDockerExeExposesTheDriverBinary(t *testing.T) {
	d := &dockerEnv{dockerExe: "/usr/bin/podman", containerID: "c1"}
	if d.DockerExe() != "/usr/bin/podman" || d.ContainerID() != "c1" {
		t.Fatalf("%q %q", d.DockerExe(), d.ContainerID())
	}
}
