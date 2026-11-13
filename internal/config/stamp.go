package config

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"os"
	"strconv"
	"strings"

	"gopkg.in/yaml.v3"
)

// Run that cannot repair config refused because the user file is not valid YAML or not a mapping
type InvalidUserConfigError struct {
	Message string
	Cause   error
}

// The user message
func (e *InvalidUserConfigError) Error() string { return e.Message }

// The parse failure behind it
func (e *InvalidUserConfigError) Unwrap() error { return e.Cause }

// Schema version a stamp value stands for, a boolean or unreadable value reading as the oldest schema
func coerceConfigVersion(n *yaml.Node) int {
	n = resolveAlias(n)
	if n == nil || n.Kind != yaml.ScalarNode {
		return 0
	}
	tag, text := scalarKind(n)
	var v int64
	switch tag {
	case tagInt:
		i, ok := constructInt(text)
		if !ok || !i.IsInt64() {
			return 0
		}
		v = i.Int64()
	case tagFloat:
		f, ok := constructFloat(text)
		if !ok || math.IsNaN(f) || math.IsInf(f, 0) {
			return 0
		}
		v = int64(f)
	case tagStr:
		i, err := strconv.ParseInt(strings.TrimSpace(strings.ReplaceAll(text, "_", "")), 10, 64)
		if err != nil {
			return 0
		}
		v = i
	default:
		return 0
	}
	return int(max(v, 0))
}

// Stamp the raw config of ctx carries and the current schema version, stamped false for a file without the key
func (s *Store) ReadConfigVersionStamp(ctx context.Context, strict bool) (stamp int, stamped bool, latest int, err error) {
	latest = max(Version, 1)
	_, path, err := s.configPath(ctx)
	if err != nil {
		return 0, false, latest, err
	}
	b, h, err := s.route(ctx, path)
	if err != nil {
		return 0, false, latest, err
	}
	if exists, err := b.Exists(ctx, h); err != nil || !exists {
		return latest, true, latest, err
	}
	doc, err := b.Read(ctx, h)
	if err != nil {
		if errors.Is(err, ErrBackendUnavailable) {
			return 0, false, latest, err
		}
		s.warnParseFailure(ctx, path, err, fallbackDefaults)
		if strict {
			return 0, false, latest, &InvalidUserConfigError{Message: fmt.Sprintf("Cannot inspect %s: config.yaml is not valid YAML (%v)", path, err), Cause: err}
		}
		return latest, true, latest, nil
	}
	root := emptyMapping()
	switch {
	case doc.root == nil || isNull(doc.root):
	case !doc.IsMapping():
		if strict {
			return 0, false, latest, &InvalidUserConfigError{Message: fmt.Sprintf("Cannot inspect %s: config.yaml top-level value must be a mapping, got %s", path, rootKindName(doc))}
		}
	default:
		root = doc.root
	}
	at := mappingIndex(root, "_config_version")
	if at < 0 {
		return 0, false, latest, nil
	}
	return coerceConfigVersion(root.Content[at]), true, latest, nil
}

// Schema version on disk and the current one, a file without the key reading as zero
func (s *Store) CheckConfigVersion(ctx context.Context, strict bool) (current, latest int, err error) {
	stamp, stamped, latest, err := s.ReadConfigVersionStamp(ctx, strict)
	if !stamped {
		stamp = 0
	}
	return stamp, latest, err
}

// Refuses an existing invalid config before a run that cannot repair it, a missing or empty file being valid
func (s *Store) RequireParseableUserConfig(ctx context.Context, ignoreUserConfig bool) error {
	if ignoreUserConfig || os.Getenv("ICEBATH_IGNORE_USER_CONFIG") == "1" {
		return nil
	}
	_, path, err := s.configPath(ctx)
	if err != nil {
		return err
	}
	b, h, err := s.route(ctx, path)
	if err != nil {
		return err
	}
	doc, perr := b.Read(ctx, h)
	switch {
	case errors.Is(perr, os.ErrNotExist):
		return nil
	case errors.Is(perr, ErrBackendUnavailable):
		return perr
	case perr == nil && doc.IsMapping():
		return nil
	case perr == nil:
		perr = &YAMLError{Problem: fmt.Sprintf("top-level YAML value must be a mapping, got %s", rootKindName(doc))}
	}
	at := ""
	if where := errorLocation(perr); where != "" {
		at = " at " + where
	}
	msg := fmt.Sprintf("icebath stopped because your settings file (%s) has a formatting error%s. Fix it with `icebath config edit` and check with "+
		"`icebath config check`, or add --ignore-user-config to run once with default settings.", path, at)
	if backup, ok := s.backupConfig(ctx, path, "corrupt"); ok {
		msg += " A copy of the broken file is at " + backup + "."
	}
	msg += " Details: " + errorDetails(perr)
	slog.Error(msg)
	return &InvalidUserConfigError{Message: msg, Cause: perr}
}
