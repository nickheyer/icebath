package codeexec

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"testing"

	"golang.org/x/text/encoding/unicode"

	"github.com/nickheyer/icebath/pkg/cliui"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/pm"
	"github.com/nickheyer/icebath/pkg/tools"
)

// Console writing into a buffer with color off
func cuSetupConsole() (*cliui.Console, *bytes.Buffer) {
	buf := &bytes.Buffer{}
	return &cliui.Console{In: strings.NewReader(""), Out: buf, Getenv: func(k string) (string, bool) {
		if k == "NO_COLOR" {
			return "1", true
		}
		return "", false
	}}, buf
}

// Contract status answering a fixed state for any binary
func cuFixedContract(t *testing.T, fields map[string]any) {
	t.Helper()
	cuSwap(t, &cuaDriverContractStatus, func(*tools.Env, string) (*pyjson.Object, error) {
		o := pyjson.NewObject()
		keys := make([]string, 0, len(fields))
		for k := range fields {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			o.Set(k, fields[k])
		}
		return o, nil
	})
}

// Runtime manifest of a driver build answering the 0.20 contract minus the omitted flags
func cuRuntimeManifest(version string, omit ...string) string {
	required := []struct {
		name string
		args []string
	}{
		{"mcp", []string{"--grant", "--socket"}},
		{"serve", []string{"--approve-capability-manifest", "--capability-manifest", "--embedded", "--permission-mode", "--socket"}},
		{"stop", []string{"--socket"}},
	}
	var subs []any
	for _, r := range required {
		var args []any
		for _, a := range r.args {
			if !slices.Contains(omit, a) {
				args = append(args, map[string]any{"name": a})
			}
		}
		subs = append(subs, map[string]any{"name": r.name, "args": args})
	}
	raw, _ := json.Marshal(map[string]any{"binary_version": version,
		"mcp_invocation": map[string]any{"command": "cua-driver", "args": []any{"mcp"}}, "subcommands": subs})
	return string(raw)
}

func TestRuntimeContract(t *testing.T) {
	cases := []struct {
		version string
		omit    []string
		ready   bool
	}{{"0.20.0", nil, true}, {"0.19.4", nil, false}, {"bad-version", nil, false}, {"0.20.0", []string{"--approve-capability-manifest"}, false}}
	for _, c := range cases {
		cuFakeRun(t, func([]string) string { return cuRuntimeManifest(c.version, c.omit...) }, 0)
		binary := filepath.Join(t.TempDir(), "cua-driver")
		state, err := cuaDriverContractStatus(nil, binary)
		if err != nil {
			t.Fatal(err)
		}
		ready, _ := state.Get("ready")
		b, _ := state.Get("binary")
		reason, _ := state.Get("reason")
		if ready != c.ready || b != binary || (reason != nil && reason != "") == c.ready {
			t.Fatalf("%s %v: state %v", c.version, c.omit, state)
		}
		if len(c.omit) > 0 && !strings.Contains(reason.(string), "serve --approve-capability-manifest") {
			t.Fatalf("reason %q", reason)
		}
	}
}

func TestPMFailureIsReportedWithoutVendorFallback(t *testing.T) {
	for range []bool{false, true} {
		t.Setenv(cuaDriverCmdEnv, "")
		cuSwap(t, &cuaResolvedDriverCmd, func() (string, error) { return "", nil })
		var ensured []string
		cuSwap(t, &cuaSetupEnsure, func(_ context.Context, name string, opts pm.EnsureOptions) error {
			ensured = append(ensured, name)
			if !opts.Explicit {
				t.Fatal("ensure must be explicit")
			}
			return errors.New("cua-driver: offline")
		})
		cuSwap(t, &runQuiet, func([]string, float64, runOpts) (*completedProcess, error) {
			t.Fatal("vendor installer")
			return nil, nil
		})
		con, out := cuSetupConsole()
		if ok, err := installCuaDriver(context.Background(), nil, con, true); ok || err != nil {
			t.Fatalf("ok %v err %v", ok, err)
		}
		if !slices.Equal(ensured, []string{"cua-driver"}) || !strings.Contains(out.String(), "offline") {
			t.Fatalf("ensured %v output %q", ensured, out.String())
		}
	}
}

func TestBrokenOverrideNeverAcquiresStandardDriver(t *testing.T) {
	for range []bool{false, true} {
		t.Setenv(cuaDriverCmdEnv, filepath.Join(t.TempDir(), "missing-driver"))
		cuSwap(t, &cuaSetupEnsure, func(context.Context, string, pm.EnsureOptions) error {
			t.Fatal("override was replaced")
			return nil
		})
		con, _ := cuSetupConsole()
		if ok, err := installCuaDriver(context.Background(), nil, con, true); ok || err != nil {
			t.Fatalf("ok %v err %v", ok, err)
		}
	}
}

// Task XML of a cua-driver-serve task as schtasks /XML prints it, UTF-16 with a byte order mark
func cuTaskXML(t *testing.T, command string) []byte {
	t.Helper()
	var esc bytes.Buffer
	if err := xmlEscapeText(&esc, command); err != nil {
		t.Fatal(err)
	}
	text := `<?xml version="1.0" encoding="UTF-16"?>` +
		`<Task xmlns="http://schemas.microsoft.com/windows/2004/02/mit/task">` +
		`<Actions><Exec><Command>` + esc.String() + `</Command></Exec></Actions></Task>`
	raw, err := unicode.UTF16(unicode.LittleEndian, unicode.UseBOM).NewEncoder().Bytes([]byte(text))
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

// Escapes ampersand and angle brackets the way xml.sax.saxutils.escape does
func xmlEscapeText(w *bytes.Buffer, s string) error {
	_, err := w.WriteString(strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;").Replace(s))
	return err
}

func TestAutostartUsesSelectedBinaryAndVerifiesRegistration(t *testing.T) {
	cuSwap(t, &cuPlatform, "win32")
	for _, oldTarget := range []bool{false, true} {
		dir := t.TempDir()
		binary := filepath.Join(dir, "User's driver directory", "cua-driver.exe")
		selected := binary
		if oldTarget {
			selected = filepath.Join(dir, "old-cua-driver.exe")
		}
		var calls [][]string
		cuSwap(t, &cuaAutostartOptIn, func(*tools.Env) bool { return true })
		cuSwap(t, &cuaSchtasksQuery, func(argv []string, _ float64, _ bool) (int, []byte, error) {
			calls = append(calls, slices.Clone(argv))
			return 0, cuTaskXML(t, selected), nil
		})
		cuSwap(t, &cuaSetupRunText, func(argv []string, _ float64, _ map[string]string, _ bool) (*completedProcess, error) {
			calls = append(calls, slices.Clone(argv))
			last := argv[len(argv)-1]
			if !strings.Contains(last, "-FilePath $exe") || !strings.Contains(last, "-ArgumentList @('autostart','enable')") || !strings.Contains(last, psSingleQuote(binary)) {
				t.Fatalf("registration command %q", last)
			}
			selected = binary
			return &completedProcess{Args: argv}, nil
		})
		cuSwap(t, &cuaSetupWhich, func(cmd string) (string, bool) { return cmd, true })
		con, _ := cuSetupConsole()
		if !repairCuaDriverAutostartWindows(nil, con, binary, false) {
			t.Fatalf("old target %v: registration not verified", oldTarget)
		}
		spawned := 0
		for _, c := range calls {
			if c[0] != "schtasks.exe" {
				spawned++
			} else if !slices.Contains(c, "/XML") {
				t.Fatalf("schtasks query without /XML %v", c)
			}
		}
		if (spawned == 1) != oldTarget || spawned > 1 {
			t.Fatalf("old target %v spawned %d registrations", oldTarget, spawned)
		}
	}
}

func TestAutostartDoesNotClaimSuccessWithoutRegisteredTask(t *testing.T) {
	cuSwap(t, &cuPlatform, "win32")
	cuSwap(t, &cuaAutostartOptIn, func(*tools.Env) bool { return true })
	cuSwap(t, &cuaDriverAutostartRegisteredWindows, func(*tools.Env, string) bool { return false })
	cuSwap(t, &cuaSetupWhich, func(cmd string) (string, bool) { return cmd, true })
	cuSwap(t, &cuaSetupRunText, func(argv []string, _ float64, _ map[string]string, _ bool) (*completedProcess, error) {
		return &completedProcess{Args: argv}, nil
	})
	con, _ := cuSetupConsole()
	if repairCuaDriverAutostartWindows(nil, con, "cua-driver.exe", false) {
		t.Fatal("success claimed without a registered task")
	}
}

func TestRepairWithoutOptInSpawnsNoPowershell(t *testing.T) {
	cuSwap(t, &cuPlatform, "win32")
	cuConfig(t, map[string]any{}, false)
	cuSwap(t, &cuaDriverAutostartRegisteredWindows, func(*tools.Env, string) bool { return false })
	cuSwap(t, &cuaSetupWhich, func(string) (string, bool) { t.Fatal("resolved a binary"); return "", false })
	cuSwap(t, &cuaSetupRunText, func([]string, float64, map[string]string, bool) (*completedProcess, error) {
		t.Fatal("spawned PowerShell")
		return nil, nil
	})
	con, _ := cuSetupConsole()
	if !repairCuaDriverAutostartWindows(nil, con, "cua-driver.exe", false) {
		t.Fatal("the on demand default needs no repair")
	}
}

func TestAutostartOptInDefaultsToOnDemand(t *testing.T) {
	for _, tree := range []map[string]any{{}, {"computer_use": map[string]any{}}, {"computer_use": map[string]any{"autostart": false}}} {
		cuConfig(t, tree, false)
		if cuaAutostartOptIn(nil) {
			t.Fatalf("%v opted in", tree)
		}
	}
}

func TestAutostartOptInExplicitTrueRegisters(t *testing.T) {
	cuConfig(t, map[string]any{"computer_use": map[string]any{"autostart": true}}, false)
	if !cuaAutostartOptIn(nil) {
		t.Fatal("explicit true must opt in")
	}
}

func TestAutostartOptInUnreadableConfigFailsClosed(t *testing.T) {
	cuConfig(t, nil, true)
	if cuaAutostartOptIn(nil) {
		t.Fatal("unreadable config must fail closed")
	}
}

func TestAutostartRegistrationPSCommandQuotesPathsWithSpaces(t *testing.T) {
	path := `C:\Program Files\cua driver\cua-driver.exe`
	ps := cuaAutostartRegistrationPSCommand(path)
	for _, want := range []string{"Start-Process -FilePath $exe", "@('autostart','enable')", "-Verb RunAs -Wait -PassThru", "$exe = '" + path + "'"} {
		if !strings.Contains(ps, want) {
			t.Fatalf("%q lacks %q", ps, want)
		}
	}
}

func TestInstallReadyTaskRequirementFollowsTheOptIn(t *testing.T) {
	cuSwap(t, &cuPlatform, "win32")
	for _, c := range []struct{ optIn, expected bool }{{false, true}, {true, false}} {
		cuFixedContract(t, map[string]any{"ready": true, "version": "0.20.0", "binary": "/x/cua-driver", "reason": ""})
		cuSwap(t, &cuaDriverAutostartRegisteredWindows, func(*tools.Env, string) bool { return false })
		cuSwap(t, &cuaAutostartOptIn, func(*tools.Env) bool { return c.optIn })
		if got, err := cuaDriverInstallReady(nil); err != nil || got != c.expected {
			t.Fatalf("opt in %v: ready %v err %v", c.optIn, got, err)
		}
	}
}

func TestSetupPreservesHostRegistrationFailure(t *testing.T) {
	cuSwap(t, &cuPlatform, "win32")
	for _, registered := range []bool{false, true} {
		t.Setenv(cuaDriverCmdEnv, "")
		cuSwap(t, &cuaSetupEnsure, func(context.Context, string, pm.EnsureOptions) error { return nil })
		cuSwap(t, &cuaResolvedDriverCmd, func() (string, error) { return "cua-driver.exe", nil })
		cuFixedContract(t, map[string]any{"ready": true})
		var repairs []string
		cuSwap(t, &repairCuaDriverAutostartWindows, func(_ *tools.Env, _ *cliui.Console, driverCmd string, verbose bool) bool {
			if verbose {
				t.Fatal("repair must run quiet")
			}
			repairs = append(repairs, driverCmd)
			return registered
		})
		con, _ := cuSetupConsole()
		if ok, err := installCuaDriver(context.Background(), nil, con, false); err != nil || ok != registered {
			t.Fatalf("registered %v: ok %v err %v", registered, ok, err)
		}
		if !slices.Equal(repairs, []string{"cua-driver.exe"}) {
			t.Fatalf("repairs %v", repairs)
		}
	}
}

// Executable shell script at path
func cuWriteDriver(t *testing.T, path string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
}

func TestSetupRefusesBareBinaryWithoutRequiredSignedApp(t *testing.T) {
	cuSwap(t, &cuPlatform, "darwin")
	driver := filepath.Join(t.TempDir(), "cua-driver")
	cuWriteDriver(t, driver)
	t.Setenv(cuaDriverCmdEnv, driver)
	cuFixedContract(t, map[string]any{"ready": true})
	con, _ := cuSetupConsole()
	if ok, err := installCuaDriver(context.Background(), nil, con, false); ok || err != nil {
		t.Fatalf("ok %v err %v", ok, err)
	}
}

func TestSetupRegistersOnlyTheValidatedSelectedApp(t *testing.T) {
	cuSwap(t, &cuPlatform, "darwin")
	app := filepath.Join(t.TempDir(), "CuaDriver.app")
	driver := filepath.Join(app, "Contents", "MacOS", "cua-driver")
	cuWriteDriver(t, driver)
	t.Setenv(cuaDriverCmdEnv, driver)
	cuFixedContract(t, map[string]any{"ready": true})
	var validated []string
	cuSwap(t, &cuaValidateAppSignature, func(_ *tools.Env, p string) error { validated = append(validated, p); return nil })
	var registered [][]string
	cuSwap(t, &cuaSetupRunText, func(argv []string, _ float64, _ map[string]string, _ bool) (*completedProcess, error) {
		registered = append(registered, slices.Clone(argv))
		return &completedProcess{Args: argv}, nil
	})
	con, _ := cuSetupConsole()
	if ok, err := installCuaDriver(context.Background(), nil, con, false); !ok || err != nil {
		t.Fatalf("ok %v err %v", ok, err)
	}
	if !slices.Equal(validated, []string{app}) || len(registered) != 1 || !slices.Equal(registered[0][len(registered[0])-2:], []string{"-f", app}) {
		t.Fatalf("validated %v registered %v", validated, registered)
	}
}

func TestSetupKeepsPermissionGuidance(t *testing.T) {
	con, out := cuSetupConsole()
	printCuaPlatformNotes(con, false, false, true)
	if !strings.Contains(out.String(), "Accessibility") || !strings.Contains(out.String(), "Screen Recording") {
		t.Fatalf("output %q", out.String())
	}
}

func TestVersionSummary(t *testing.T) {
	for _, c := range []struct{ raw, expected string }{
		{"cua-driver 0.20.0", "cua-driver 0.20.0"}, {"banner\nsecond line", "banner"},
		{"\n\n  cua-driver 0.20.0  ", "cua-driver 0.20.0"}, {strings.Repeat("x", 500), strings.Repeat("x", 120)},
		{"", ""}, {"   \n \n", ""},
	} {
		if got := cuaVersionSummary(c.raw, 120); got != c.expected {
			t.Fatalf("%q -> %q", c.raw, got)
		}
	}
}
