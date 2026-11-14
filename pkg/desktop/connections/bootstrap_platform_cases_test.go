package connections

import (
	"strings"
	"testing"
)

func envOf(values map[string]string) func(string) string {
	return func(key string) string { return values[key] }
}

func TestDetectRemoteDisplayKeepsGPUOnForLocalSessions(t *testing.T) {
	cases := []struct {
		env      map[string]string
		platform string
	}{
		{map[string]string{"DISPLAY": ":0"}, "linux"},
		{map[string]string{"WAYLAND_DISPLAY": "wayland-0"}, "linux"},
		{map[string]string{"SESSIONNAME": "Console"}, "win32"},
		{map[string]string{}, "darwin"},
	}
	for _, c := range cases {
		if got := DetectRemoteDisplay(envOf(c.env), c.platform); got != "" {
			t.Fatalf("%v on %s: got %q", c.env, c.platform, got)
		}
	}
}

func TestDetectRemoteDisplayDoesNotTreatWSLgAsRemote(t *testing.T) {
	for _, env := range []map[string]string{
		{"WSL_DISTRO_NAME": "Ubuntu", "DISPLAY": ":0"},
		{"WSL_INTEROP": "/run/WSL/1_interop", "DISPLAY": ":0"},
	} {
		if got := DetectRemoteDisplay(envOf(env), "linux"); got != "" {
			t.Fatalf("%v: got %q", env, got)
		}
	}
}

func TestDetectRemoteDisplayFlagsSSHSessionsOnAnyPlatform(t *testing.T) {
	cases := []struct {
		env      map[string]string
		platform string
	}{
		{map[string]string{"SSH_CONNECTION": "1.2.3.4 5 6.7.8.9 22"}, "linux"},
		{map[string]string{"SSH_CLIENT": "1.2.3.4 5 22"}, "darwin"},
		{map[string]string{"SSH_TTY": "/dev/pts/0"}, "win32"},
	}
	for _, c := range cases {
		if got := DetectRemoteDisplay(envOf(c.env), c.platform); got != "ssh-session" {
			t.Fatalf("%v on %s: got %q", c.env, c.platform, got)
		}
	}
}

func TestDetectRemoteDisplayFlagsForwardedX11DisplaysButNotLocalOnes(t *testing.T) {
	for _, display := range []string{"localhost:10.0", "192.168.1.5:0"} {
		if got := DetectRemoteDisplay(envOf(map[string]string{"DISPLAY": display}), "linux"); !strings.Contains(got, "x11-forwarding") {
			t.Fatalf("%s: got %q", display, got)
		}
	}
	if got := DetectRemoteDisplay(envOf(map[string]string{"DISPLAY": ":1"}), "linux"); got != "" {
		t.Fatalf("local display: got %q", got)
	}
}

func TestDetectRemoteDisplayFlagsRDPSessions(t *testing.T) {
	if got := DetectRemoteDisplay(envOf(map[string]string{"SESSIONNAME": "RDP-Tcp#7"}), "win32"); !strings.HasPrefix(got, "rdp") {
		t.Fatalf("got %q", got)
	}
}

func TestDetectRemoteDisplayHonorsTheICEBATHDESKTOPDISABLEGPUOverrideBothWays(t *testing.T) {
	if got := DetectRemoteDisplay(envOf(map[string]string{"ICEBATH_DESKTOP_DISABLE_GPU": "1", "DISPLAY": ":0"}), "linux"); !strings.Contains(got, "override") {
		t.Fatalf("force on: got %q", got)
	}
	env := map[string]string{"ICEBATH_DESKTOP_DISABLE_GPU": "false", "SSH_CONNECTION": "1.2.3.4 5 6.7.8.9 22"}
	if got := DetectRemoteDisplay(envOf(env), "linux"); got != "" {
		t.Fatalf("force off: got %q", got)
	}
}
