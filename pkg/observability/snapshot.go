package observability

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sync"

	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/i18n"
	"github.com/nickheyer/icebath/pkg/internal/pyint"
	"github.com/nickheyer/icebath/pkg/state"
)

const snapshotCountCap = 251

// Hooks the skills, profiles and gateway packages install from an init function
var (
	// SkillIndexFilesHook lists every skill index file under a skills directory, set by pkg/skills
	SkillIndexFilesHook func(skillsDir, filename string) []string
	// ProfileNamesHook lists every profile name, set by pkg/profiles
	ProfileNamesHook func(ctx context.Context) []string
	// ConnectedPlatformsHook counts the messaging platforms the gateway config connects, set by pkg/gateway
	ConnectedPlatformsHook func(ctx context.Context) (int, error)
)

// SQLiteReadError is a state.db that exists but cannot be read, the sqlite3.Error the readers catch
type SQLiteReadError struct{ Err error }

func (e *SQLiteReadError) Error() string { return e.Err.Error() }
func (e *SQLiteReadError) Unwrap() error { return e.Err }

var (
	firstSessionMu sync.Mutex
	firstSessionAt = map[string]float64{}
)

func mcpServerCount(cfg map[string]any) int {
	servers, ok := asDict(installSub(cfg, "mcp_servers"))
	if !ok {
		return 0
	}
	count := 0
	for _, spec := range servers {
		if d, isDict := asDict(spec); isDict {
			if enabled, set := d["enabled"].(bool); set && !enabled {
				continue
			}
		}
		count++
	}
	return count
}

func pluginCount(cfg map[string]any) int {
	if enabled, ok := installSub(installSub(cfg, "plugins"), "enabled").([]any); ok {
		return len(enabled)
	}
	return 0
}

func cronJobCount(ctx context.Context) (int, error) {
	record, ok := readRecord(filepath.Join(home.Dir(ctx), "cron", "jobs.json"))
	if !ok {
		return 0, nil
	}
	jobs, present := record.Get("jobs")
	if !present {
		return 0, nil
	}
	var entries []any
	switch v := jobs.(type) {
	case []any:
		entries = v
	case string, map[string]any:
		return 0, nil
	default:
		if _, isDict := asDict(v); isDict {
			return 0, nil
		}
		return 0, fmt.Errorf("TypeError: %T object is not iterable", jobs)
	}
	count := 0
	for _, job := range entries {
		if d, isDict := asDict(job); isDict {
			if enabled, set := d["enabled"]; !set || pyTruthy(enabled) {
				count++
			}
		}
	}
	return count, nil
}

func skillCount(ctx context.Context) (int, error) {
	if SkillIndexFilesHook == nil {
		return 0, errHookUnavailable
	}
	dir := filepath.Join(home.Dir(ctx), "skills")
	if info, err := os.Stat(dir); err != nil || !info.IsDir() {
		return 0, nil
	}
	return min(len(SkillIndexFilesHook(dir, "SKILL.md")), snapshotCountCap), nil
}

func profileCount(ctx context.Context) (int, error) {
	if ProfileNamesHook == nil {
		return 0, errHookUnavailable
	}
	return len(ProfileNamesHook(ctx)), nil
}

func messagingPlatformCount(ctx context.Context) (int, error) {
	if ConnectedPlatformsHook == nil {
		return 0, errHookUnavailable
	}
	return ConnectedPlatformsHook(ctx)
}

func safeCount(reader func() (int, error)) (count int) {
	defer func() {
		if recover() != nil {
			count = 0
		}
	}()
	count, err := reader()
	if err != nil {
		return 0
	}
	return count
}

// Epoch of the profile's first session, now for a profile without one, cached once it exists
func firstSessionStartedAt(ctx context.Context, homeDir string) (float64, error) {
	if homeDir == "" {
		homeDir = home.Dir(ctx)
	}
	firstSessionMu.Lock()
	cached, ok := firstSessionAt[homeDir]
	firstSessionMu.Unlock()
	if ok {
		return cached, nil
	}
	database := filepath.Join(homeDir, "state.db")
	if _, err := os.Stat(database); err != nil {
		return pyTime(), nil
	}
	conn, err := state.ConnectSQLite("file:"+database+"?mode=ro", state.SQLiteConfig{Timeout: 1, URI: true})
	if err != nil {
		return 0, &SQLiteReadError{err}
	}
	var first any
	err = conn.QueryRowContext(ctx, "SELECT MIN(started_at) FROM sessions").Scan(&first)
	conn.Close()
	if err != nil {
		return 0, &SQLiteReadError{err}
	}
	if first == nil {
		return pyTime(), nil
	}
	value, err := pyint.Float(first)
	if err != nil {
		return 0, err
	}
	firstSessionMu.Lock()
	firstSessionAt[homeDir] = value
	firstSessionMu.Unlock()
	return value, nil
}

// Bucketed age of the profile's first session, unknown when state.db cannot be read
func InstallAgeBucketAt(ctx context.Context, homeDir string) (string, error) {
	first, err := firstSessionStartedAt(ctx, homeDir)
	var readErr *SQLiteReadError
	if errors.As(err, &readErr) {
		return "unknown", nil
	}
	if err != nil {
		return "", err
	}
	return InstallAgeBucket(pyTime() - first), nil
}

// Bounded daily snapshot fields of the active profile's config
func CollectInstallSnapshot(ctx context.Context, cfg map[string]any) (map[string]string, error) {
	model := installSub(cfg, "model")
	language := func() (value any) {
		defer func() {
			if r := recover(); r != nil {
				slog.Debug("Shared-metrics display language unavailable", "error", fmt.Sprint(r))
				value = "other"
			}
		}()
		return i18n.GetLanguage(ctx)
	}()
	skills, err := skillCount(ctx)
	if err != nil {
		return nil, err
	}
	cronJobs, err := cronJobCount(ctx)
	if err != nil {
		return nil, err
	}
	profiles, err := profileCount(ctx)
	if err != nil {
		return nil, err
	}
	age, err := InstallAgeBucketAt(ctx, "")
	if err != nil {
		return nil, err
	}
	var mainProvider any
	if _, ok := asDict(model); ok {
		mainProvider = installSub(model, "provider")
	}
	fields := InstallSnapshotFields(InstallSnapshotInput{
		MemoryProvider:     installSub(installSub(cfg, "memory"), "provider"),
		MCPServers:         mcpServerCount(cfg),
		Plugins:            pluginCount(cfg),
		Skills:             skills,
		CronJobs:           cronJobs,
		Profiles:           profiles,
		MessagingPlatforms: safeCount(func() (int, error) { return messagingPlatformCount(ctx) }),
		InstallAgeBucket:   age,
		MainProvider:       mainProvider,
		TerminalBackend:    installSub(installSub(cfg, "terminal"), "backend"),
		DisplayLanguage:    language,
	})
	for key, value := range InstallV4SnapshotFields(ctx, cfg) {
		fields[key] = value
	}
	fields["compression_prepare_ahead"] = PrepareAheadState(cfg)
	return fields, nil
}
