package gatewaycli

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"regexp"
	"runtime"

	"github.com/nickheyer/icebath/pkg/gateway"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
	"github.com/nickheyer/icebath/pkg/profiles"
)

const taskNameDefault = "Icebath_Gateway"

var (
	serviceGOOS          = runtime.GOOS
	errWindowsOnly       = errors.New("gateway_windows is Windows-only")
	windowsFilenameBadRE = regexp.MustCompile("[<>:\"/\\\\|?*\x00-\x1f]")
)

// Installs the Windows task facts pkg/profiles/manage reaches through profiles.GatewayServiceHooks
func init() {
	h := &profiles.GatewayServiceHooks
	h.WindowsTaskName = func(ctx context.Context) string { return mustService(GetTaskName(ctx)) }
	h.WindowsTaskRegistered = func(ctx context.Context) bool {
		ok, err := IsTaskRegistered(ctx)
		mustService("", err)
		return ok
	}
	h.ExecSchtasks = func(args []string) (int, string, string) {
		code, out, errText, err := ExecSchtasks(args)
		mustService("", err)
		return code, out, errText
	}
	h.WindowsStartupEntryPath = func(ctx context.Context) string { return mustService(GetStartupEntryPath(ctx)) }
	h.WindowsLegacyStartupEntryPath = func(ctx context.Context) string { return mustService(LegacyStartupEntryPath(ctx)) }
}

// Value of a naming call, raising its error the way the uncaught Python exception does
func mustService(v string, err error) string {
	if err != nil {
		panic(err)
	}
	return v
}

// RuntimeError of every gateway_windows entry point off Windows
func assertWindows() error {
	if serviceGOOS != "windows" {
		return errWindowsOnly
	}
	return nil
}

// Scheduled Task name scoped per profile
func GetTaskName(ctx context.Context) (string, error) {
	if err := assertWindows(); err != nil {
		return "", err
	}
	suffix, err := gateway.ProfileSuffix(ctx)
	if err != nil {
		return "", err
	}
	if suffix != "" {
		return taskNameDefault + "_" + suffix, nil
	}
	return taskNameDefault, nil
}

// Characters illegal in Windows filenames replaced by underscores
func sanitizeFilename(value string) string {
	return windowsFilenameBadRE.ReplaceAllString(value, "_")
}

// Startup folder under APPDATA, else under USERPROFILE or HOME AppData Roaming
func startupDir() (string, error) {
	tail := []string{"Microsoft", "Windows", "Start Menu", "Programs", "Startup"}
	if appdata := pystr.Strip(os.Getenv("APPDATA")); appdata != "" {
		return filepath.Join(append([]string{appdata}, tail...)...), nil
	}
	profile := pystr.Strip(os.Getenv("USERPROFILE"))
	if profile == "" {
		profile = pystr.Strip(os.Getenv("HOME"))
	}
	if profile == "" {
		return "", errors.New("neither APPDATA nor USERPROFILE is set \u2014 cannot resolve Startup folder")
	}
	return filepath.Join(append([]string{profile, "AppData", "Roaming"}, tail...)...), nil
}

// Startup folder entry named for the task with the given extension
func startupEntry(ctx context.Context, ext string) (string, error) {
	if err := assertWindows(); err != nil {
		return "", err
	}
	dir, err := startupDir()
	if err != nil {
		return "", err
	}
	name, err := GetTaskName(ctx)
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, sanitizeFilename(name)+ext), nil
}

// Startup folder VBS launcher of the task
func GetStartupEntryPath(ctx context.Context) (string, error) { return startupEntry(ctx, ".vbs") }

// Startup folder cmd entry older installs wrote
func LegacyStartupEntryPath(ctx context.Context) (string, error) { return startupEntry(ctx, ".cmd") }

// Whether schtasks answers the task query with exit code zero
func IsTaskRegistered(ctx context.Context) (bool, error) {
	name, err := GetTaskName(ctx)
	if err != nil {
		return false, err
	}
	code, _, _, err := ExecSchtasks([]string{"/Query", "/TN", name})
	return err == nil && code == 0, err
}
