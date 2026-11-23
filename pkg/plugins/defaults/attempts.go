package defaults

import (
	"fmt"
	"math"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
	"unicode"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/home"
)

const (
	// Wait after a first failed install before the next automatic attempt, doubled per further failure
	retrySeconds = 3600.0
	// Longest wait between automatic attempts
	retryMaxSeconds = 7 * 24 * 3600.0
)

// Stamp a failed automatic install leaves in a home, its text the count of consecutive failures
func failureStamp(h, name string) string {
	return filepath.Join(home.CacheDir(h), "default-plugins.failed-"+name)
}

// Consecutive failures a stamp records, an empty or non numeric stamp counting once
func failureCount(stamp string) int {
	data, err := os.ReadFile(stamp)
	if err != nil {
		return 0
	}
	text := strings.TrimSpace(strings.TrimPrefix(string(data), "\ufeff"))
	if text == "" || strings.TrimFunc(text, unicode.IsDigit) != "" {
		return 1
	}
	n, err := strconv.Atoi(text)
	if err != nil {
		return math.MaxInt32
	}
	return n
}

// Counts one more failed install in the stamp whose age and count gate the next automatic attempt
func noteFailure(h, name string) {
	stamp := failureStamp(h, name)
	count := failureCount(stamp)
	if err := os.MkdirAll(filepath.Dir(stamp), 0o700); err != nil {
		pkgLog().Warn(fmt.Sprintf("default plugins: retry stamp not written for %s: %s", h, err))
		return
	}
	if err := os.WriteFile(stamp, []byte(strconv.Itoa(count+1)), 0o600); err != nil {
		pkgLog().Warn(fmt.Sprintf("default plugins: retry stamp not written for %s: %s", h, err))
	}
}

// Whether the last failure is younger than an hour doubled per failure, capped at a week
func failedRecently(h, name string) bool {
	stamp := failureStamp(h, name)
	info, err := os.Stat(stamp)
	if err != nil {
		return false
	}
	age := time.Since(info.ModTime()).Seconds()
	failures := min(failureCount(stamp), 32)
	return age < math.Min(retrySeconds*math.Pow(2, float64(failures-1)), retryMaxSeconds)
}

// One line cause of an install error, git's own fatal or error line when present
func firstCause(errText string) string {
	var lines []string
	for _, line := range strings.Split(strings.ReplaceAll(errText, "\r\n", "\n"), "\n") {
		if line = strings.TrimSpace(line); line != "" {
			lines = append(lines, line)
		}
	}
	cause := "unknown error"
	if len(lines) > 0 {
		cause = lines[0]
	}
	for _, line := range lines {
		if strings.HasPrefix(line, "fatal:") || strings.HasPrefix(line, "error:") {
			cause = strings.TrimSpace(strings.SplitN(line, ":", 2)[1])
			break
		}
	}
	return strings.TrimRight(cause, ". ")
}

// Names a home by its profile, else by its path
func homeLabel(h string) string {
	if profile := home.ProfileNameFor(h); profile != "" {
		return fmt.Sprintf("profile '%s'", profile)
	}
	return h
}

// User config.yaml of a home with env references expanded and the administrator's pins applied, empty when it has none
func yamlLayers(h string) (map[string]any, error) {
	doc, err := config.ReadDocument(home.ConfigPath(h))
	if err != nil {
		return nil, err
	}
	expanded, _ := config.Expand(doc, os.LookupEnv).(map[string]any)
	if expanded == nil {
		expanded = map[string]any{}
	}
	return config.ApplyManagedOverlay(expanded), nil
}
