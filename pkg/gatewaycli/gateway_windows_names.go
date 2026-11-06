package gatewaycli

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"regexp"

	"github.com/nickheyer/icebath/pkg/internal/pystr"
)

const taskNameDefault = "Icebath_Gateway"

var (
	errWindowsOnly       = errors.New("gateway_windows is Windows-only")
	windowsFilenameBadRE = regexp.MustCompile("[<>:\"/\\\\|?*\x00-\x1f]")
)

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
	suffix, err := profileSuffix(ctx)
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
