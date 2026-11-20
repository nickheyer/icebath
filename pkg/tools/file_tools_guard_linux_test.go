//go:build linux

package tools

import "testing"

func TestFileToolsGuardBlockedDeviceDetection(t *testing.T) {
	for _, dev := range []string{"/dev/zero", "/dev/random", "/dev/urandom", "/dev/stdin",
		"/dev/tty", "/dev/console", "/dev/stdout", "/dev/stderr",
		"/dev/fd/0", "/dev/fd/1", "/dev/fd/2"} {
		if !ftgBlocked(t, dev) {
			t.Errorf("%s should be blocked", dev)
		}
	}
}

func TestFileToolsGuardProcFdBlocked(t *testing.T) {
	if !ftgBlocked(t, "/proc/self/fd/0") || !ftgBlocked(t, "/proc/12345/fd/2") {
		t.Fatal("proc fd aliases must be blocked")
	}
}

func TestFileToolsGuardProcSensitivePseudoFilesBlocked(t *testing.T) {
	for _, p := range []string{
		"/proc/self/environ", "/proc/12345/environ",
		"/proc/self/cmdline", "/proc/99/cmdline",
		"/proc/self/maps", "/proc/1/maps",
		"/proc/self/smaps", "/proc/12345/smaps",
		"/proc/self/smaps_rollup", "/proc/99/smaps_rollup",
		"/proc/self/numa_maps", "/proc/1/numa_maps",
		"/proc/self/mem", "/proc/12345/mem",
		"/proc/self/auxv", "/proc/1/auxv",
		"/proc/self/pagemap", "/proc/99/pagemap",
	} {
		if !ftgBlocked(t, p) {
			t.Errorf("%s should be blocked", p)
		}
	}
}

func TestFileToolsGuardProcTaskThreadSensitiveFilesBlocked(t *testing.T) {
	for _, p := range []string{
		"/proc/self/task/1234/maps", "/proc/self/task/1234/smaps", "/proc/self/task/1234/auxv",
		"/proc/self/task/1234/pagemap", "/proc/self/task/1234/environ",
	} {
		if !ftgBlocked(t, p) {
			t.Errorf("%s should be blocked", p)
		}
	}
}

func TestFileToolsGuardNormpathAliasToBlockedDeviceIsBlocked(t *testing.T) {
	if !ftgBlocked(t, "/dev/../dev/zero") || !ftgBlocked(t, "/dev/./urandom") {
		t.Fatal("normpath aliases of blocked devices must be blocked")
	}
}

func TestFileToolsGuardTildeExpandsToProfileHome(t *testing.T) {
	env, profileHome, _ := ftgProfileHomeEnv(t)
	if got := expandTildeFor(env, "~/scratch/file.txt"); got != profileHome+"/scratch/file.txt" {
		t.Fatalf("expanded %q", got)
	}
}
