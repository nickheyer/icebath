package backup

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/gateway"
)

// RAM backed home the temp home guard accepts, systemctl, loginctl and sudo on PATH and the command boundary recorded
func importChoiceSupervisor(t *testing.T) (string, *[][]string) {
	t.Helper()
	dir, err := os.MkdirTemp("/dev/shm", "icebath-choice-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	profile, runtimeDir, binDir := filepath.Join(dir, "profile"), filepath.Join(dir, "run"), filepath.Join(dir, "bin")
	for _, d := range []string{profile, runtimeDir, binDir} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(runtimeDir, "bus"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"systemctl", "loginctl", "sudo"} {
		if err := os.WriteFile(filepath.Join(binDir, name), []byte("#!/bin/sh\nexit 0\n"), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("HOME", dir)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(dir, ".config"))
	t.Setenv("ICEBATH_HOME", profile)
	t.Setenv("XDG_RUNTIME_DIR", runtimeDir)
	t.Setenv("DBUS_SESSION_BUS_ADDRESS", "")
	os.Unsetenv("DBUS_SESSION_BUS_ADDRESS")
	calls := &[][]string{}
	active := false
	saved := gateway.RunServiceCommand
	t.Cleanup(func() { gateway.RunServiceCommand = saved })
	gateway.RunServiceCommand = func(_ context.Context, _ time.Duration, argv ...string) (gateway.CommandResult, error) {
		args := argv[1:]
		*calls = append(*calls, append([]string{filepath.Base(argv[0])}, args...))
		has := func(a string) bool { return slices.Contains(args, a) }
		if has("start") {
			active = true
		}
		out := ""
		switch {
		case has("is-active"):
			out = "inactive\n"
			if active {
				out = "active\n"
			}
		case has("show-user"):
			out = "yes\n"
		case has("show"):
			out = "LoadState=not-found\n"
		case has("is-system-running"):
			out = "running\n"
		}
		return gateway.CommandResult{Stdout: out}, nil
	}
	return profile, calls
}

func TestImportAndOrdinarySetupNeverAuthorizeInstall(t *testing.T) {
	for _, choice := range []any{nil, "decline", "install"} {
		for _, installed := range []bool{false, true} {
			t.Run(map[bool]string{true: "installed", false: "absent"}[installed]+"-"+map[any]string{nil: "none", "decline": "decline", "install": "install"}[choice], func(t *testing.T) {
				profile, calls := importChoiceSupervisor(t)
				ctx := context.Background()
				if err := config.SaveConfig(ctx, profile, map[string]any{"gateway": map[string]any{"service_install_choice": choice}}, config.SaveOptions{}); err != nil {
					t.Fatal(err)
				}
				unit, err := gateway.GetSystemdUnitPath(ctx, false)
				if err != nil {
					t.Fatal(err)
				}
				if installed {
					text, err := gateway.GenerateSystemdUnit(ctx, false, "")
					if err != nil {
						t.Fatal(err)
					}
					if err := os.MkdirAll(filepath.Dir(unit), 0o755); err != nil {
						t.Fatal(err)
					}
					if err := os.WriteFile(unit, []byte(text), 0o644); err != nil {
						t.Fatal(err)
					}
				}
				before, err := os.ReadFile(filepath.Join(profile, "config.yaml"))
				if err != nil {
					t.Fatal(err)
				}
				var out bytes.Buffer
				for _, o := range []gateway.ServiceSetupOptions{{}, {Interactive: true}, {Context: "import", Install: true}} {
					ok, err := gateway.EnsureGatewayService(ctx, &out, o)
					if err != nil || ok != installed {
						t.Fatalf("ensure %+v %v %v\n%s", o, ok, err, out.String())
					}
				}
				reviveGatewayAfterImport(ctx, &out, profile)
				if _, err := os.Stat(unit); (err == nil) != installed {
					t.Fatalf("unit present %v", err == nil)
				}
				forbidden := []string{"enable", "enable-linger", "daemon-reload"}
				if !installed {
					forbidden = append(forbidden, "start")
				}
				for _, op := range *calls {
					for _, verb := range forbidden {
						if slices.Contains(op, verb) {
							t.Fatalf("forbidden operation %v", op)
						}
					}
				}
				after, err := os.ReadFile(filepath.Join(profile, "config.yaml"))
				if err != nil || !bytes.Equal(before, after) {
					t.Fatalf("config changed %v\n%s\n%s", err, before, after)
				}
			})
		}
	}
}
