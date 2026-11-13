package config

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/nickheyer/icebath/internal/home"
)

// What a loader serves while the file it could not read is broken
type fallbackKind string

// Fallbacks a parse failure warning names
const (
	fallbackDefaults     fallbackKind = "defaults"
	fallbackLastGood     fallbackKind = "last-known-good"
	fallbackLastGoodFile fallbackKind = "last-known-good-backup"
	fallbackRefusedWrite fallbackKind = "refuse-write"
)

// User texts of the config read and write refusals
const (
	fixPermissions               = "Fix the file permissions or move it aside first."
	fixYAMLTemplate              = "Fix it with `icebath config edit` and check with `icebath config check`, or copy the newest good file from %s over config.yaml."
	defaultsFallbackText         = "icebath is running on default settings until it is fixed, so none of your saved settings are applied."
	repairTemplate               = "Open it with `icebath config edit`, fix %s, then run `icebath config check`."
	retryAdvice                  = "Try again; run `icebath config check` if it keeps failing."
	parseWarningPrefix           = "⚠️  icebath config: "
	nonMappingRootProblem        = "top-level YAML must be a mapping, got %s"
	nonMappingRootReasonTemplate = "must start with settings names, but its top level is a %s"
)

// Sentence a parse failure warning gives for what is served meanwhile
func (f fallbackKind) text() string {
	switch f {
	case fallbackLastGood:
		return "icebath is running on the settings it loaded before the edit until it is fixed, so recent changes are not applied."
	case fallbackLastGoodFile:
		return "icebath is running on your last good settings until it is fixed, so recent changes are not applied."
	case fallbackRefusedWrite:
		return "Nothing was written, so the existing file is preserved."
	}
	return defaultsFallbackText
}

// Reports a failure of the filesystem rather than of the YAML the file holds
func isReadError(err error) bool {
	var pe *fs.PathError
	var se *os.SyscallError
	var le *os.LinkError
	return errors.As(err, &pe) || errors.As(err, &se) || errors.As(err, &le)
}

// Where a parse failure happened, such as line 12, empty when the parser names no line
func errorLocation(err error) string {
	var ye *YAMLError
	if errors.As(err, &ye) && ye.Line > 0 {
		return fmt.Sprintf("line %d", ye.Line)
	}
	return ""
}

// Single line details of a failure, the parser problem when there is one
func errorDetails(err error) string {
	text := err.Error()
	var ye *YAMLError
	if errors.As(err, &ye) {
		text = ye.Problem
	}
	return strings.Join(strings.Fields(text), " ")
}

// Advice for a file that could not be read
func readErrorFix(err error) string {
	if errors.Is(err, fs.ErrPermission) {
		return fixPermissions
	}
	return retryAdvice
}

// User text for an unreadable or unparseable config file, naming what is served and how to repair it
func parseFailureMessage(path string, err error, fallback fallbackKind) string {
	if isReadError(err) {
		return fmt.Sprintf("Your settings file (%s) could not be read. %s %s", path, fallback.text(), readErrorFix(err))
	}
	where := errorLocation(err)
	at := ""
	if where != "" {
		at = " at " + where
	} else {
		where = "the problem"
	}
	return fmt.Sprintf("Your settings file (%s) has a formatting error%s. %s %s", path, at, fallback.text(), fmt.Sprintf(repairTemplate, where))
}

// Type name a refusal prints for a top level that is not a mapping, such as list, str or int
func rootKindName(n *YAMLDocument) string {
	switch {
	case n.root == nil || isNull(n.root):
		return "NoneType"
	case n.root.Kind == yaml.SequenceNode:
		return "list"
	case n.root.Kind == yaml.MappingNode:
		return "dict"
	}
	tag, text := scalarKind(n.root)
	switch tag {
	case tagBool:
		return "bool"
	case tagInt:
		return "int"
	case tagFloat:
		return "float"
	case "!!binary":
		return "bytes"
	case "!!timestamp":
		if m := timestampText.FindStringSubmatch(text); m != nil && m[4] == "" {
			return "date"
		}
		return "datetime"
	}
	return "str"
}

// Reports a top level neither a mapping nor a value read as unset, which the readers take as no settings
func badRoot(d *YAMLDocument) bool { return !d.IsMapping() && isSet(d.root) }

// Recorded parse failure of a home config and the version it was taken at
type failureRecord struct {
	version Revision
	message string
}

// Warns once per file signature on log and stderr that a config file is broken, copying bad YAML aside
func (s *Store) warnParseFailure(ctx context.Context, path string, err error, fallback fallbackKind) {
	key := path + "\x00"
	if b, h, berr := s.route(ctx, path); berr == nil {
		if v, verr := b.Version(ctx, h); verr == nil {
			key += fmt.Sprint(v)
			s.recordFailure(path, err)
		}
	}
	s.cacheMu.Lock()
	seen := s.warned[key]
	s.warned[key] = true
	s.cacheMu.Unlock()
	if seen {
		return
	}
	msg := parseFailureMessage(path, err, fallback)
	if !isReadError(err) {
		if backup, ok := s.backupConfig(ctx, path, "corrupt"); ok {
			msg += " A copy of the broken file was saved to " + backup + "."
		}
	}
	details := errorDetails(err)
	slog.Warn(msg + " Details: " + details)
	fmt.Fprintf(os.Stderr, "%s%s\n    Details: %s\n", parseWarningPrefix, msg, details)
}

// Remembers the failure against a digest of the file so a byte identical fix is told apart
func (s *Store) recordFailure(path string, err error) {
	v, verr := fileCacheVersion(path, true)
	if verr != nil {
		v = Revision{Rest: "<" + errorClass(verr) + ">"}
	}
	s.cacheMu.Lock()
	s.failures[path] = failureRecord{version: v, message: err.Error()}
	s.cacheMu.Unlock()
}

// Drops the failure record of a file that reads again
func (s *Store) forgetFailure(path string) {
	s.cacheMu.Lock()
	delete(s.failures, path)
	s.cacheMu.Unlock()
}

// Recorded parse error of the config of ctx while the file is still byte identical to the one that failed
func (s *Store) ActiveParseFailure(ctx context.Context) (string, bool) {
	_, path, err := s.configPath(ctx)
	if err != nil {
		return "", false
	}
	s.cacheMu.Lock()
	rec, ok := s.failures[path]
	s.cacheMu.Unlock()
	if !ok {
		return "", false
	}
	v, verr := fileCacheVersion(path, true)
	if verr != nil {
		v = Revision{Rest: "<" + errorClass(verr) + ">"}
	}
	if v != rec.version {
		return "", false
	}
	return rec.message, true
}

// Write that must not replace an existing config file, with the reason, the fix and the cause
type RefusedWriteError struct {
	Path   string
	Reason string
	Fix    string
	Cause  error
}

// One user sentence ending in the cause details
func (e *RefusedWriteError) Error() string {
	at := ""
	if where := errorLocation(e.Cause); where != "" {
		at = " (" + where + ")"
	}
	return fmt.Sprintf("Your settings file (%s) %s%s, so this change was not saved. %s Details: %s", e.Path, e.Reason, at, e.Fix, errorDetails(e.Cause))
}

// Matches ErrWriteRefused and the cause
func (e *RefusedWriteError) Unwrap() []error { return []error{ErrWriteRefused, e.Cause} }

// Advice naming the directory the good config copies are kept in
func (s *Store) fixYAML(ctx context.Context) string {
	h, err := s.homes.Home(ctx)
	if err != nil {
		return fmt.Sprintf(fixYAMLTemplate, "backups/config/")
	}
	shown, err := home.Display(h)
	if err != nil {
		shown = h
	}
	return fmt.Sprintf(fixYAMLTemplate, shown+"/backups/config/")
}

// Refusal to save a value served in place of an unreadable file, nil for a normal read
func (s *Store) refuseFailedRead(ctx context.Context, path string, readErr error) error {
	switch {
	case readErr == nil:
		return nil
	case isReadError(readErr):
		return &RefusedWriteError{Path: path, Reason: "could not be read", Fix: readErrorFix(readErr), Cause: readErr}
	case errors.Is(readErr, ErrBackendUnavailable):
		return &unservedSaveError{cause: readErr}
	}
	return &RefusedWriteError{Path: path, Reason: "has a formatting error", Fix: s.fixYAML(ctx), Cause: readErr}
}

// Save of a config the selected backend could not serve, refused with the backend failure
type unservedSaveError struct {
	cause error
}

// The refusal followed by the backend failure
func (e *unservedSaveError) Error() string {
	return "The config could not be read, so this change was not saved: " + e.cause.Error()
}

// Matches ErrWriteRefused and the backend failure
func (e *unservedSaveError) Unwrap() []error { return []error{ErrWriteRefused, e.cause} }
