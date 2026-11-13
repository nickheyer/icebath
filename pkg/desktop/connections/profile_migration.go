package connections

import (
	"bytes"
	"encoding/json"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// Floor in bytes of the size axis of the state.db score
const ProfileScoreMinSizeBytes = 1024

// MigrationDecision is the active-profile.json seed, Migrated set when the state.db heuristic chose it
type MigrationDecision struct {
	Profile  *string `json:"profile"`
	Migrated bool    `json:"_migrated,omitempty"`
}

// MigrationDeps are the homes, clock, process check and writer the first boot seed reads and writes through
type MigrationDeps struct {
	LegacyActivePath   string
	HermesHome         string
	ProfilesRoot       string
	IsHermesProcess    func(pid int) bool
	Now                func() float64
	WriteJSON          func(path string, decision MigrationDecision) error
	IsValidProfileName func(name string) bool
}

// ProfileStateDBPath is state.db of a profile, the default living at the home root
func ProfileStateDBPath(name, hermesHome, profilesRoot string) string {
	if name == "default" {
		return hermesHome + "/state.db"
	}
	return profilesRoot + "/" + name + "/state.db"
}

// ProfileGatewayPidPath is gateway.pid of a profile, the default living at the home root
func ProfileGatewayPidPath(name, hermesHome, profilesRoot string) string {
	if name == "default" {
		return hermesHome + "/gateway.pid"
	}
	return profilesRoot + "/" + name + "/gateway.pid"
}

// ReadLegacyActiveProfile is the trimmed CLI sticky profile, present false when unreadable or empty, empty name when invalid or default
func ReadLegacyActiveProfile(legacyActivePath string, isValid func(string) bool) (name string, present bool) {
	raw, err := os.ReadFile(legacyActivePath)
	if err != nil {
		return "", false
	}
	trimmed := jsTrim(nodeUTF8(raw))
	if trimmed == "" {
		return "", false
	}
	if trimmed == "default" || !isValid(trimmed) {
		return "", true
	}
	return trimmed, true
}

// Decodes bytes as Node utf8 does, invalid sequences becoming U+FFFD
func nodeUTF8(raw []byte) string { return strings.ToValidUTF8(string(raw), "�") }

// FindRunningGatewayProfiles lists the profiles whose gateway.pid names a live icebath process
func FindRunningGatewayProfiles(allProfiles []string, deps MigrationDeps) []string {
	running := []string{}
	for _, name := range allProfiles {
		pidFile := ProfileGatewayPidPath(name, deps.HermesHome, deps.ProfilesRoot)
		if _, err := os.Stat(pidFile); err != nil {
			continue
		}
		raw, err := os.ReadFile(pidFile)
		if err != nil {
			continue
		}
		var parsed any
		if json.Unmarshal([]byte(nodeUTF8(raw)), &parsed) != nil {
			continue
		}
		var pidValue any
		if obj, ok := parsed.(map[string]any); ok {
			pidValue = obj["pid"]
		}
		pid := ecmaToNumber(pidValue)
		if math.IsNaN(pid) || math.IsInf(pid, 0) || pid != math.Trunc(pid) || pid < 1 || pid > math.MaxInt32 {
			continue
		}
		if deps.IsHermesProcess(int(pid)) {
			running = append(running, name)
		}
	}
	return running
}

// ScoreStateDB is the recency times size score of a state.db, false when the file cannot be stated
func ScoreStateDB(dbPath string, now float64) (float64, bool) {
	info, err := os.Stat(dbPath)
	if err != nil {
		return 0, false
	}
	mtimeMs := float64(info.ModTime().UnixNano()) / 1e6
	days := math.Max(0, (now-mtimeMs)/(1000*60*60*24))
	recency := math.Max(0.1, 30-days)
	size := math.Log10(math.Max(ProfileScoreMinSizeBytes, float64(info.Size())))
	return recency * size, true
}

// DecideMigration prefers the legacy choice, then a lone running gateway, then the best scored state.db
func DecideMigration(legacyActive string, running, candidates []string, deps MigrationDeps, score func(string) (float64, bool)) *MigrationDecision {
	if legacyActive != "" {
		return &MigrationDecision{Profile: &legacyActive}
	}
	if len(running) == 1 {
		name := running[0]
		return &MigrationDecision{Profile: &name}
	}
	best := ""
	maxScore := math.Inf(-1)
	for _, name := range candidates {
		s, ok := score(ProfileStateDBPath(name, deps.HermesHome, deps.ProfilesRoot))
		if !ok {
			continue
		}
		if s > maxScore {
			maxScore = s
			best = name
		}
	}
	if best == "" || best == "default" {
		return nil
	}
	return &MigrationDecision{Profile: &best, Migrated: true}
}

// ListProfileDirs names the profile directories under the profiles root, a default directory included
func ListProfileDirs(deps MigrationDeps) []string {
	entries, err := os.ReadDir(deps.ProfilesRoot)
	if err != nil {
		return []string{}
	}
	names := []string{}
	for _, e := range entries {
		if e.IsDir() && (e.Name() == "default" || deps.IsValidProfileName(e.Name())) {
			names = append(names, e.Name())
		}
	}
	return names
}

// WithDefaultCandidate puts default first ahead of the named profiles
func WithDefaultCandidate(named []string) []string {
	out := []string{"default"}
	for _, name := range named {
		if name != "default" {
			out = append(out, name)
		}
	}
	return out
}

// ExistingPreference is the profile and heuristic flag of an existing active-profile.json
type ExistingPreference struct {
	Profile  string
	Migrated bool
}

// ReadExistingPreference parses active-profile.json, nil when missing, malformed or not an object
func ReadExistingPreference(path string) *ExistingPreference {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	var parsed any
	if json.Unmarshal([]byte(nodeUTF8(raw)), &parsed) != nil {
		return nil
	}
	if parsed == nil {
		return nil
	}
	rec, ok := parsed.(map[string]any)
	if !ok {
		if _, isArray := parsed.([]any); !isArray {
			return nil
		}
		rec = map[string]any{}
	}
	profile := ""
	if p, ok := rec["profile"].(string); ok {
		profile = jsTrim(p)
	}
	return &ExistingPreference{Profile: profile, Migrated: rec["_migrated"] == true}
}

// MigrateActiveProfileIfMissing seeds active-profile.json on first boot and repairs a heuristic one, true when it wrote
func MigrateActiveProfileIfMissing(desktopProfileConfigPath string, deps MigrationDeps) (bool, error) {
	_, statErr := os.Stat(desktopProfileConfigPath)
	exists := statErr == nil
	var existing *ExistingPreference
	if exists {
		existing = ReadExistingPreference(desktopProfileConfigPath)
	}
	if exists && (existing == nil || !existing.Migrated) {
		return false, nil
	}
	legacyActive, _ := ReadLegacyActiveProfile(deps.LegacyActivePath, deps.IsValidProfileName)
	allProfiles := WithDefaultCandidate(ListProfileDirs(deps))
	running := FindRunningGatewayProfiles(allProfiles, deps)
	candidates := allProfiles
	if len(running) > 1 {
		candidates = running
	}
	decision := DecideMigration(legacyActive, running, candidates, deps, func(dbPath string) (float64, bool) {
		return ScoreStateDB(dbPath, deps.Now())
	})
	if decision == nil || *decision.Profile == "default" {
		if existing != nil && existing.Migrated {
			return true, deps.WriteJSON(desktopProfileConfigPath, MigrationDecision{})
		}
		return false, nil
	}
	if existing != nil && existing.Migrated && existing.Profile == *decision.Profile {
		return false, nil
	}
	return true, deps.WriteJSON(desktopProfileConfigPath, *decision)
}

// Whether pid is a live process whose command line names icebath, guarding recycled pids of stale pid files
func isHermesProcess(pid int) bool {
	if !signalZero(pid) {
		return false
	}
	if cmdline, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/cmdline"); err == nil {
		return strings.Contains(nodeUTF8(cmdline), "icebath")
	}
	cmd := exec.Command("ps", "-p", strconv.Itoa(pid), "-o", "args=")
	var out bytes.Buffer
	cmd.Stdout = &out
	if cmd.Start() != nil {
		return false
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	select {
	case err := <-done:
		return err == nil && strings.Contains(nodeUTF8(out.Bytes()), "icebath")
	case <-time.After(2 * time.Second):
		_ = cmd.Process.Kill()
		<-done
		return false
	}
}

// MigrateActiveProfileIfMissing seeds the desktop profile from the legacy file, a running gateway or state.db once per launch
func (s *Service) MigrateActiveProfileIfMissing() error {
	if s.local.migrationAttempted.Swap(true) {
		return nil
	}
	deps := MigrationDeps{
		LegacyActivePath:   filepath.Join(s.icebathHome, "active_profile"),
		HermesHome:         s.icebathHome,
		ProfilesRoot:       filepath.Join(s.icebathHome, "profiles"),
		IsHermesProcess:    isHermesProcess,
		Now:                func() float64 { return float64(time.Now().UnixMilli()) },
		IsValidProfileName: desktopProfileNameRE.MatchString,
		WriteJSON: func(target string, decision MigrationDecision) error {
			if err := os.MkdirAll(filepath.Dir(target), 0o777); err != nil {
				return err
			}
			body, err := jsonStringifyIndent(decision)
			if err != nil {
				return err
			}
			tmp := target + ".tmp"
			if err := os.WriteFile(tmp, body, 0o666); err != nil {
				return err
			}
			return os.Rename(tmp, target)
		},
	}
	s.profiles.mu.Lock()
	defer s.profiles.mu.Unlock()
	_, err := MigrateActiveProfileIfMissing(s.profiles.path, deps)
	return err
}
