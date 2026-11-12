//go:build !windows

package native

import (
	"context"
	"errors"
	"net"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/nickheyer/icebath/pkg/desktop/backend"
	"github.com/nickheyer/icebath/pkg/internal/hermetic"
)

func TestMain(m *testing.M) {
	hermetic.BlockExternalHTTP()
	hermetic.BlockExternalDNS()
	os.Exit(m.Run())
}

func writeScript(t *testing.T, dir, name, body string) string {
	t.Helper()
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, []byte("#!/bin/sh\n"+body), 0o755); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestResolveIcebathBackendPrefersTheOverrideThenTheInstallThenABootstrap(t *testing.T) {
	cli := writeScript(t, t.TempDir(), "icebath", "exit 0\n")
	env := func(v string) func(string) string {
		return func(k string) string {
			if k == "ICEBATH_DESKTOP_ICEBATH" {
				return v
			}
			return ""
		}
	}
	got := ResolveIcebathBackend(context.Background(), []string{"gateway", "ensure", "--json"}, t.TempDir(), env(cli), false, func(string) {})
	if got.Kind != "command" || got.Command != cli || !reflect.DeepEqual(got.Args, []string{"gateway", "ensure", "--json"}) || got.Label != "existing Icebath CLI at "+cli {
		t.Fatalf("%+v", got)
	}
	var logs []string
	wsl := ResolveIcebathBackend(context.Background(), nil, t.TempDir(), env(`C:\icebath\icebath.exe`), true, func(l string) { logs = append(logs, l) })
	if wsl.Kind != "bootstrap-needed" || len(logs) == 0 || !strings.Contains(logs[0], "Ignoring Windows Icebath override under WSL") {
		t.Fatalf("%+v %v", wsl, logs)
	}
	t.Setenv("HOME", t.TempDir())
	if none := ResolveIcebathBackend(context.Background(), nil, t.TempDir(), env(""), false, func(string) {}); none.Kind != "bootstrap-needed" || !none.Bootstrap {
		t.Fatalf("%+v", none)
	}
}

func TestAGatewayTicketRefusalKeepsItsDiscoveryReasonForTheGroupPolicy(t *testing.T) {
	dir, _ := os.MkdirTemp("/tmp", "desktop-native-")
	t.Cleanup(func() { os.RemoveAll(dir) })
	home, _ := filepath.EvalSymlinks(dir)
	ln, err := net.Listen("unix", filepath.Join(home, "gateway.sock"))
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	os.Chmod(filepath.Join(home, "gateway.sock"), 0o666)
	endpoint := backend.GatewayEndpoint{ProfileID: home, InstanceID: "owner", AuthorityEpoch: "1", RuntimeProtocol: 1, APIOrigin: "http://127.0.0.1:1234", Capabilities: []string{"session-authority-v1"}, Supervisor: "none"}
	_, err = MintGatewayTicket(endpoint, "native-http")
	var reason backend.TicketReasonError
	if !errors.As(err, &reason) || reason.TicketReason() != "unsafe_control_permissions" {
		t.Fatalf("%v", err)
	}
}
