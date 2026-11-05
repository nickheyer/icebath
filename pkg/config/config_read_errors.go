package config

import (
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"os"
	"regexp"
	"strings"
	"sync"
	"syscall"

	"github.com/nickheyer/icebath/pkg/home"
)

var parseFailureFallbackMsg = map[string]string{
	"last-known-good":        "Icebath is running on the settings it loaded before the edit until it is fixed, so recent changes are not applied.",
	"last-known-good-backup": "Icebath is running on your last good settings until it is fixed, so recent changes are not applied.",
	"refuse-write":           "Nothing was written, so the existing file is preserved.",
}

const (
	parseFailureDefaultsMsg = "Icebath is running on default settings until it is fixed, so none of your saved settings are applied."
	fixPerms                = "Fix the file permissions or move it aside first."
	fixYAML                 = "Fix it with `icebath config edit` and check with `icebath config check`, or copy the newest good " +
		"file from %s over config.yaml."
)

// Stream the parse failure warning is written to
var ParseFailureOut io.Writer = os.Stderr

type parseFailure struct {
	version string
	message string
}

var (
	parseFailureMu     sync.Mutex
	configParseWarned  = map[string]bool{}
	configParseFailure = map[string]parseFailure{}
)

var yamlLineRe = regexp.MustCompile(`(?s)^yaml: line (\d+): (.*)$`)

// Underlying YAML error text and its one based line when the error carries one
func yamlProblem(err error) (line string, problem string) {
	var inv *InvalidUserConfigError
	if errors.As(err, &inv) && inv.Err != nil {
		err = inv.Err
	}
	text := err.Error()
	if m := yamlLineRe.FindStringSubmatch(text); m != nil {
		return "line " + m[1], m[2]
	}
	return "", ""
}

// Line location of a YAML error such as line 12, empty without one
func YAMLErrorLocation(err error) string {
	line, _ := yamlProblem(err)
	return line
}

// Single line details text of the YAML problem or the error text
func YAMLErrorDetails(err error) string {
	_, problem := yamlProblem(err)
	text := problem
	if text == "" {
		text = strings.TrimSpace(err.Error())
	}
	return strings.Join(strings.Fields(text), " ")
}

// Reports whether an error is an operating system error rather than a parse failure
func IsOSError(err error) bool {
	var pathErr *fs.PathError
	var linkErr *os.LinkError
	var sysErr *os.SyscallError
	var errno syscall.Errno
	return errors.As(err, &pathErr) || errors.As(err, &linkErr) || errors.As(err, &sysErr) || errors.As(err, &errno)
}

// User copy for an unreadable or unparseable config file naming what happens and how to fix it
func FormatConfigParseFailure(configPath string, err error, fallback string) string {
	where := YAMLErrorLocation(err)
	fallbackMsg, ok := parseFailureFallbackMsg[fallback]
	if !ok {
		fallbackMsg = parseFailureDefaultsMsg
	}
	if IsOSError(err) {
		return fmt.Sprintf("Your settings file (%s) could not be read. %s %s", configPath, fallbackMsg, ReadErrorFix(err))
	}
	at, fixWhere := "", "the problem"
	if where != "" {
		at, fixWhere = " at "+where, where
	}
	repair := fmt.Sprintf("Open it with `icebath config edit`, fix %s, then run `icebath config check`.", fixWhere)
	return fmt.Sprintf("Your settings file (%s) has a formatting error%s. %s %s", configPath, at, fallbackMsg, repair)
}

// Stat signature of a config file as the file backend versions it
func configStatVersion(path string) (string, error) {
	st, err := os.Stat(path)
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("%d:%d", st.ModTime().UnixNano(), st.Size()), nil
}

// Reader the failure version digests through, swapped by tests to inject a transient read fault
var readFailureBytes = os.ReadFile

// Reads the bytes of a YAML file every config parse reads, the seam transient read faults replace
var readYAMLFile = os.ReadFile

// Version a parse failure record matches on, content digested so a same tick fix clears it
func failureVersion(path string) (string, error) {
	sig, err := configStatVersion(path)
	if err != nil {
		return "", err
	}
	data, rerr := readFailureBytes(path)
	if rerr != nil {
		return sig + ":none", nil
	}
	return fmt.Sprintf("%s:%x", sig, sha256.Sum256(data)), nil
}

// Records a config parse failure and warns once per signature, errors when the corrupt backup has no backend
func WarnConfigParseFailure(configPath string, err error, fallback string) error {
	parseFailureMu.Lock()
	key := configPath + "|0:0"
	if sig, serr := configStatVersion(configPath); serr == nil {
		key = configPath + "|" + sig
		if version, verr := failureVersion(configPath); verr == nil {
			configParseFailure[configPath] = parseFailure{version: version, message: err.Error()}
		}
	}
	if configParseWarned[key] {
		parseFailureMu.Unlock()
		return nil
	}
	configParseWarned[key] = true
	parseFailureMu.Unlock()
	backup := ""
	if !IsOSError(err) {
		var berr error
		if backup, berr = BackupConfig(configPath, "corrupt", DefaultBackupKeep); berr != nil {
			return berr
		}
	}
	msg := FormatConfigParseFailure(configPath, err, fallback)
	if backup != "" {
		msg += " A copy of the broken file was saved to " + backup + "."
	}
	details := YAMLErrorDetails(err)
	slog.Warn(msg+" Details: "+details, "path", configPath)
	fmt.Fprintf(ParseFailureOut, "⚠️  icebath config: %s\n    Details: %s\n", msg, details)
	return nil
}

// Recorded parse error while the active config file is byte identical to the one that failed
func ActiveConfigParseFailure(h string) (string, bool) {
	path := home.ConfigPath(h)
	parseFailureMu.Lock()
	record, ok := configParseFailure[path]
	parseFailureMu.Unlock()
	if !ok {
		return "", false
	}
	version, err := failureVersion(path)
	if err != nil || version != record.version {
		return "", false
	}
	return record.message, true
}

// Clears the warned signatures and recorded parse failures
func ResetConfigParseFailures() {
	parseFailureMu.Lock()
	defer parseFailureMu.Unlock()
	configParseWarned = map[string]bool{}
	configParseFailure = map[string]parseFailure{}
}

// Fail open fallback a reader serves for an unreadable config that writers refuse to persist
type FailedConfigRead struct {
	Data      map[string]any
	ReadError error
}

// Error a config backend raises when it cannot serve the config, carrying its exit message
type SystemExitError interface {
	error
	SystemExitCode() string
}

// Fix advice for a config read error
func ReadErrorFix(err error) string {
	if errors.Is(err, fs.ErrPermission) {
		return fixPerms
	}
	return "Try again; run `icebath config check` if it keeps failing."
}

// Refusal to save a fail open fallback, nil for ordinary data
func RefuseFailedRead(h, configPath string, data any) error {
	failed, ok := data.(*FailedConfigRead)
	if !ok {
		var m map[string]any
		switch d := data.(type) {
		case map[string]any:
			m = d
		case Tree:
			m = d
		}
		readErr := FailedReadError(m)
		if readErr == nil {
			return nil
		}
		failed = &FailedConfigRead{Data: m, ReadError: readErr}
	}
	err := failed.ReadError
	if IsOSError(err) {
		return RefuseOverwrite(configPath, "could not be read", err, ReadErrorFix(err))
	}
	var exit SystemExitError
	if errors.As(err, &exit) {
		return fmt.Errorf("The config could not be read, so this change was not saved: %s", exit.SystemExitCode())
	}
	return RefuseOverwrite(configPath, "has a formatting error", err, fmt.Sprintf(fixYAML, BackupsDirDisplay(h)))
}

// Error for a write that must not replace an existing config file
func RefuseOverwrite(configPath, reason string, err error, fix string) error {
	at := ""
	if where := YAMLErrorLocation(err); where != "" {
		at = " (" + where + ")"
	}
	return fmt.Errorf("Your settings file (%s) %s%s, so this change was not saved. %s Details: %s",
		configPath, reason, at, fix, YAMLErrorDetails(err))
}

// Display form of the config backups dir of a home
func BackupsDirDisplay(h string) string {
	return home.Display(h) + "/backups/config/"
}
