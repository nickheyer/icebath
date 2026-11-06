package hostinfo

import (
	"os"
	"path/filepath"
	"sync"
	"testing"
)

// Clears the cached detection and restores the existence seam after the test
func resetContainerCache(t *testing.T, exists func(string) bool) {
	t.Helper()
	saved := pathExists
	containerOnce, pathExists = sync.Once{}, exists
	t.Cleanup(func() { containerOnce, pathExists = sync.Once{}, saved })
}

func TestIsContainer(t *testing.T) {
	t.Run("detects_dockerenv", func(t *testing.T) {
		resetContainerCache(t, func(p string) bool { return p == "/.dockerenv" })
		if !IsContainer() {
			t.Fatal("/.dockerenv not detected")
		}
	})
	t.Run("detects_kubernetes_env", func(t *testing.T) {
		resetContainerCache(t, func(string) bool { return false })
		t.Setenv("KUBERNETES_SERVICE_HOST", "10.43.0.1")
		if !IsContainer() {
			t.Fatal("kubernetes pod not detected")
		}
	})
	t.Run("cgroup_v2_fallback_inspects_only_the_root_mount", func(t *testing.T) {
		markers := []string{"kubepods", "containerd", "crio"}
		tmp := t.TempDir()
		host := filepath.Join(tmp, "host")
		container := filepath.Join(tmp, "container")
		files := map[string]string{
			host: "25 1 259:2 / / rw,relatime shared:1 - ext4 /dev/nvme0n1p2 rw\n" +
				"469 554 0:94 / /var/lib/docker/rootfs/overlayfs/7dda83 rw,relatime shared:247 - overlay overlay " +
				"rw,lowerdir=/var/lib/containerd/io.containerd.snapshotter.v1.overlayfs/snapshots/33509/fs\n",
			container: "1 0 0:50 / / rw,relatime - overlay overlay " +
				"rw,lowerdir=/var/lib/containerd/io.containerd.snapshotter.v1.overlayfs/snapshots/9/fs\n" +
				"2 1 0:51 / /proc rw,nosuid - proc proc rw\n",
		}
		for p, text := range files {
			if err := os.WriteFile(p, []byte(text), 0o644); err != nil {
				t.Fatal(err)
			}
		}
		if rootMountHasMarker(host, markers) || !rootMountHasMarker(container, markers) || rootMountHasMarker(filepath.Join(tmp, "missing"), markers) {
			t.Fatal("root mount marker check")
		}
	})
}
