package codeexec

import (
	"bytes"
	"context"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/pm"
	"github.com/nickheyer/icebath/pkg/tools"
)

func TestMacosRefreshRegistersPostEnsureSignedApp(t *testing.T) {
	for _, registrationExit := range []int{0, 1} {
		cuSwap(t, &cuPlatform, "darwin")
		t.Setenv(cuaDriverCmdEnv, "")
		t.Setenv("ICEBATH_HOME", t.TempDir())
		dir := t.TempDir()
		old := filepath.Join(dir, "old-cua-driver")
		app := filepath.Join(dir, "new-pin", "CuaDriver.app")
		binary := filepath.Join(app, "Contents", "MacOS", "cua-driver")
		cuWriteDriver(t, binary)
		selected := old
		cuSwap(t, &cuaResolvedDriverCmd, func() (string, error) { return selected, nil })
		var ensured []string
		cuSwap(t, &cuaSetupEnsure, func(_ context.Context, name string, opts pm.EnsureOptions) error {
			if !opts.Explicit {
				t.Fatalf("ensure %s without explicit", name)
			}
			ensured = append(ensured, name)
			selected = binary
			return nil
		})
		var contracted []string
		cuSwap(t, &cuaDriverContractStatus, func(_ *tools.Env, b string) (*pyjson.Object, error) {
			contracted = append(contracted, b)
			state := pyjson.NewObject()
			state.Set("ready", true)
			return state, nil
		})
		var validated []string
		cuSwap(t, &cuaValidateAppSignature, func(_ *tools.Env, p string) error { validated = append(validated, p); return nil })
		var registered [][]string
		cuSwap(t, &cuaSetupRunText, func(argv []string, _ float64, _ map[string]string, _ bool) (*completedProcess, error) {
			registered = append(registered, slices.Clone(argv))
			return &completedProcess{Args: argv, Returncode: registrationExit}, nil
		})
		var out bytes.Buffer
		if _, err := InstallCuaDriver(context.Background(), &out, false); err != nil {
			t.Fatal(err)
		}
		if !slices.Equal(ensured, []string{"cua-driver"}) || !slices.Equal(contracted, []string{binary}) || !slices.Equal(validated, []string{app}) {
			t.Fatalf("ensured %v contracted %v validated %v", ensured, contracted, validated)
		}
		if len(registered) != 1 || !slices.Equal(registered[0][len(registered[0])-2:], []string{"-f", app}) {
			t.Fatalf("registered %v", registered)
		}
		if strings.Contains(out.String(), "app registration failed") != (registrationExit != 0) {
			t.Fatalf("exit %d output %q", registrationExit, out.String())
		}
	}
}
