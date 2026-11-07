package codeexec

import (
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"

	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
	"github.com/nickheyer/icebath/pkg/tools"
)

const cuaDriverBundleID = "com.trycua.driver"

var (
	cuRuntimePlatforms           = map[string]bool{"darwin": true, "win32": true, "linux": true}
	cuPermissionBools            = []string{"accessibility", "screen_recording", "screen_recording_capturable"}
	cuTCCServices                = []struct{ field, service string }{{"accessibility", "Accessibility"}, {"screen_recording", "ScreenCapture"}}
	tccFields                    = []string{"accessibility", "screen_recording"}
	cuStdout           io.Writer = os.Stdout
	cuStderr           io.Writer = os.Stderr
)

// Reset and regrant recovery text naming the TCC rows of the missing status fields
func staleTCCGrantHint(missing ...string) string {
	var resets []string
	for _, f := range missing {
		for _, s := range cuTCCServices {
			if s.field == f {
				resets = append(resets, "tccutil reset "+s.service+" "+cuaDriverBundleID)
			}
		}
	}
	if len(resets) == 0 {
		return ""
	}
	return "If System Settings already shows CuaDriver ON, the stored grant is stale (it no longer matches the installed " +
		"driver's signature): run `" + strings.Join(resets, " && ") + "`, then `icebath computer-use permissions grant`."
}

// Driver child env with secrets stripped, the process env when building it fails
func permissionsChildEnv(env *tools.Env) (out map[string]string) {
	defer func() {
		if r := recover(); r != nil {
			out = environMap()
		}
	}()
	return sanitizedCuaDriverEnv(env)
}

// Runs one driver verb with captured text output and the sanitized env, swapped by tests
var permissionsRun = func(env *tools.Env, binary string, timeout float64, args ...string) (*completedProcess, error) {
	return runQuiet(append([]string{binary}, args...), timeout, runOpts{Env: permissionsChildEnv(env), HideWindow: true})
}

// Decoded JSON stdout of a driver verb, nil on empty output
func permissionsJSONOut(env *tools.Env, binary string, timeout float64, args ...string) (any, error) {
	proc, err := permissionsRun(env, binary, timeout, args...)
	if err != nil {
		return nil, err
	}
	raw := pystr.Strip(proc.Stdout)
	if raw == "" {
		return nil, nil
	}
	return pyjson.Loads(raw)
}

// Doctor verdict and its label status message checks, nil on any failure
func permissionsDoctor(env *tools.Env, binary string) (ok bool, checks []any, found bool) {
	data, err := permissionsJSONOut(env, binary, 12, "doctor", "--json")
	obj, isObj := data.(*pyjson.Object)
	if err != nil || !isObj {
		return false, nil, false
	}
	probes, _ := obj.Get("probes")
	if probes == nil {
		probes = []any{}
	}
	items, err := pyIterItems(probes)
	if err != nil {
		return false, nil, false
	}
	checks = []any{}
	for _, p := range items {
		probe, ok := p.(*pyjson.Object)
		if !ok {
			continue
		}
		row := pyjson.NewObject()
		for _, k := range []string{"label", "status", "message"} {
			v, has := probe.Get(k)
			if !has {
				v = ""
			}
			row.Set(k, pyrepr.Str(v))
		}
		checks = append(checks, row)
	}
	okRaw, _ := obj.Get("ok")
	return pyrepr.Truthy(okRaw), checks, true
}

// Folds the macOS permissions status booleans and source into out
func macPermissions(env *tools.Env, binary string, out *pyjson.Object) {
	data, err := permissionsJSONOut(env, binary, 10, "permissions", "status", "--json")
	var te *subprocessTimeout
	switch {
	case errors.As(err, &te):
		out.Set("error", "cua-driver permissions status timed out")
	case err != nil:
		out.Set("error", "cua-driver permissions status failed: "+err.Error())
	default:
		obj, ok := data.(*pyjson.Object)
		if !ok {
			return
		}
		for _, k := range cuPermissionBools {
			if v, ok := obj.Vals[k].(bool); ok {
				out.Set(k, v)
			}
		}
		if src, ok := obj.Vals["source"].(*pyjson.Object); ok {
			out.Set("source", src)
		}
	}
}

// OS aware readiness payload for the desktop card, the permissions CLI and the status route in contract key order
func computerUseStatus(env *tools.Env, driverCmd *string) (*pyjson.Object, error) {
	plat, name := cuPlatform, configuredBackendName(env)
	binary := ""
	if name == defaultComputerUseBackend {
		var err error
		if binary, err = resolveCuaDriverCmd(driverCmd); err != nil {
			return nil, err
		}
	}
	out := pyjson.NewObject()
	out.Set("platform", plat)
	out.Set("platform_supported", cuRuntimePlatforms[plat])
	out.Set("installed", binary != "")
	for _, k := range []string{"version", "ready"} {
		out.Set(k, nil)
	}
	out.Set("can_grant", plat == "darwin")
	out.Set("checks", []any{})
	for _, k := range []string{"source", "error"} {
		out.Set(k, nil)
	}
	for _, k := range cuPermissionBools {
		out.Set(k, nil)
	}
	if name != defaultComputerUseBackend {
		out.Set("platform_supported", true)
		out.Set("can_grant", false)
		provider, err := getActiveProvider(env)
		var le *lookupError
		if errors.As(err, &le) {
			out.Set("error", le.Error())
			return out, nil
		} else if err != nil {
			return nil, err
		}
		out.Set("installed", true)
		out.Set("version", fmt.Sprintf("%s (computer_use.backend: %s)", provider.DisplayName(), provider.Name()))
		ready, err := provider.IsAvailable()
		if err != nil {
			return nil, err
		}
		out.Set("ready", ready)
		return out, nil
	}
	if binary == "" {
		return out, nil
	}
	if proc, err := permissionsRun(env, binary, 5, "--version"); err == nil {
		if v := pystr.Strip(proc.Stdout); v != "" {
			out.Set("version", v)
		}
	}
	doctorOK, checks, found := permissionsDoctor(env, binary)
	if found {
		out.Set("checks", checks)
	}
	if plat == "darwin" {
		macPermissions(env, binary, out)
		if errVal, _ := out.Get("error"); errVal == nil {
			a, _ := out.Get("accessibility")
			s, _ := out.Get("screen_recording")
			out.Set("ready", a == true && s == true)
		}
	} else if found {
		out.Set("ready", doctorOK)
	}
	return out, nil
}

// Grant command runner with the terminal attached, swapped by tests
var permissionsGrantRun = func(env *tools.Env, binary string) (int, error) {
	cmd := exec.Command(binary, "permissions", "grant")
	cmd.Env = envList(permissionsChildEnv(env))
	cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
	err := cmd.Run()
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		if code := exitErr.ExitCode(); code >= 0 {
			return code, nil
		}
		return -signalNumber(exitErr), nil
	}
	if err != nil {
		return 0, &spawnError{err}
	}
	return 0, nil
}

// Runs the macOS grant flow, 2 when the driver is missing and 64 off macOS
func requestPermissionsGrant(env *tools.Env, driverCmd *string) (int, error) {
	if cuPlatform != "darwin" {
		fmt.Fprintln(cuStdout, "Computer Use permissions are a macOS concept; nothing to grant here.")
		return 64, nil
	}
	binary, err := resolveCuaDriverCmd(driverCmd)
	if err != nil {
		return 0, err
	}
	if binary == "" {
		fmt.Fprintln(cuStdout, "cua-driver: not installed. Run: icebath computer-use install")
		return 2, nil
	}
	fmt.Fprintln(cuStdout, "Requesting Accessibility + Screen Recording for CuaDriver.\n"+
		"macOS will show a dialog attributed to CuaDriver ("+cuaDriverBundleID+") — approve it, then return here.")
	code, err := permissionsGrantRun(env, binary)
	if err != nil {
		fmt.Fprintf(cuStderr, "cua-driver permissions grant failed: %s\n", err)
		return 2, nil
	}
	return code, nil
}
