package pm

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
)

// Windows build dependency provider a source checkout carries
var windowsDepsProvider = filepath.Join("scripts", "build", "windows-deps.ps1")

// FileNotFoundError text a missing PowerShell raises
var errNoPowerShell = &PyError{Class: "FileNotFoundError", Message: "PowerShell is required to prepare Windows ARM64 build dependencies"}

// Runs the checkout's Windows ARM64 build dependency provider and returns the environment it prepared
func PrepareWindowsEnvironment(source, state string, env map[string]string) (map[string]string, error) {
	var search *string
	if value, ok := env["PATH"]; ok {
		search = &value
	}
	shell := whichOn("powershell", search)
	if shell == "" {
		shell = whichOn("pwsh", search)
	}
	if shell == "" {
		return nil, errNoPowerShell
	}
	if err := os.MkdirAll(state, 0o777); err != nil {
		return nil, err
	}
	scratch, err := os.MkdirTemp(state, "environment-")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(scratch)
	output := filepath.Join(scratch, "environment.json")
	cmd := exec.Command(shell, "-NoProfile", "-NonInteractive", "-ExecutionPolicy", "Bypass", "-File",
		filepath.Join(source, windowsDepsProvider), "-StateRoot", state, "-EnvironmentFile", output)
	cmd.Dir = source
	cmd.Env = EnvList(env)
	if _, err := RunContained(cmd, "Preparing Windows ARM64 build tools", ContainOptions{Indent: "  "}); err != nil {
		return nil, err
	}
	data, err := os.ReadFile(output)
	if err != nil {
		return nil, err
	}
	data = trimBOM(data)
	if err := utf8Strict(data); err != nil {
		return nil, err
	}
	decoded, err := loadJSON(data)
	if err != nil {
		return nil, &PyError{Class: "ValueError", Message: err.Error()}
	}
	invalid := &PyError{Class: "ValueError", Message: "Windows build dependency provider returned an invalid environment"}
	object, ok := decoded.(map[string]any)
	if !ok {
		return nil, invalid
	}
	prepared := map[string]string{}
	for k, v := range object {
		s, ok := v.(string)
		if !ok {
			return nil, invalid
		}
		prepared[k] = s
	}
	return prepared, nil
}

// Environment a dependency build of a source tree needs on this host, nil when the ambient one suffices
func SourceBuildEnvironment(source string) (map[string]string, error) {
	return sourceBuildEnvironment(context.Background(), source)
}

// Environment a dependency build needs, reusing the store parent as the provider's state root
func sourceBuildEnvironment(ctx context.Context, source string) (map[string]string, error) {
	target, err := CurrentTarget()
	if err != nil {
		return nil, err
	}
	if target != "win32-arm64" || !isRegularFile(filepath.Join(source, windowsDepsProvider)) {
		return nil, nil
	}
	store, err := StoreRoot(ctx)
	if err != nil {
		return nil, err
	}
	prepared, err := PrepareWindowsEnvironment(source, filepath.Dir(store), pyEnviron())
	if err != nil {
		return nil, err
	}
	bridged, err := BridgedIndexSettings(pyEnviron(), "")
	if err != nil {
		return nil, err
	}
	for k, v := range bridged {
		prepared[k] = v
	}
	return prepared, nil
}
