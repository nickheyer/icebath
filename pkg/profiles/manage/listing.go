package manage

import (
	"context"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/nickheyer/icebath/pkg/profiles"
	"github.com/nickheyer/icebath/pkg/skills"
)

// Summary of one profile as the profile list and detail views show it
type ProfileInfo struct {
	Name                string
	Path                string
	IsDefault           bool
	GatewayRunning      bool
	Model               any
	Provider            any
	HasEnv              bool
	SkillCount          int
	AliasPath           string
	AliasName           string
	DistributionName    any
	DistributionVersion any
	DistributionSource  any
	Description         string
	DescriptionAuto     bool
	DisplayName         string
	BotTitle            string
	PreviousNames       []string
}

const (
	skillCountTTL     = 600.0
	skillCountRecheck = 60.0
)

type skillCountEntry struct {
	signature, stamped float64
	count              int
}

var (
	skillCountMu        sync.Mutex
	skillCountCache     = map[string]skillCountEntry{}
	skillCountNextCheck = map[string]float64{}
	skillCountScanLocks = map[string]*sync.Mutex{}
	skillCountRefreshes sync.WaitGroup
)

// Seconds since the epoch, replaced by tests that drive the skill count clock
var nowSeconds = func() float64 { return float64(time.Now().UnixNano()) / 1e9 }

// Number of SKILL.md index files under a skills tree, vanished subtrees skipped
var walkSkillCount = func(skillsDir string) int {
	return len(skills.IterSkillIndexFiles(skillsDir, "SKILL.md"))
}

// Gateway liveness of a profile dir that never removes another profile's stale pid file
var checkGatewayRunning = func(profileDir string) bool { return gatewayProfileRunning(profileDir) }

// Whether the live default gateway multiplexes a named profile
var servedByRunningMultiplexer = func(name string) bool { return gatewayServesNamedProfile(name) }

// Gateway liveness of a profile dir without unlinking its stale pid file
func CheckGatewayRunning(profileDir string) bool { return checkGatewayRunning(profileDir) }

// Whether the live default gateway multiplexes the named profile
func ServedByRunningMultiplexer(name string) bool { return servedByRunningMultiplexer(name) }

func mtimeSeconds(fi os.FileInfo) float64 { return float64(fi.ModTime().UnixNano()) / 1e9 }

// Newest mtime of the skills dir and its immediate subdirectories, zero when the dir cannot be stat'ed
func skillsDirSignature(skillsDir string) float64 {
	st, err := os.Stat(skillsDir)
	if err != nil {
		return 0
	}
	sig := mtimeSeconds(st)
	entries, err := os.ReadDir(skillsDir)
	if err != nil {
		return sig
	}
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		fi, err := os.Lstat(filepath.Join(skillsDir, e.Name()))
		if err != nil {
			continue
		}
		sig = max(sig, mtimeSeconds(fi))
	}
	return sig
}

func freshSkillCount(key string, signature float64) (int, bool) {
	skillCountMu.Lock()
	defer skillCountMu.Unlock()
	cached, ok := skillCountCache[key]
	if ok && cached.signature == signature && nowSeconds()-cached.stamped < skillCountTTL {
		return cached.count, true
	}
	return 0, false
}

// Installed skill count of a profile, walking only when the tree signature changed or the TTL passed
func countSkills(profileDir string) int {
	skillsDir := filepath.Join(profileDir, "skills")
	if st, err := os.Stat(skillsDir); err != nil || !st.IsDir() {
		return 0
	}
	key := skillsDir
	if n, ok := freshSkillCount(key, skillsDirSignature(skillsDir)); ok {
		return n
	}
	skillCountMu.Lock()
	scan, ok := skillCountScanLocks[key]
	if !ok {
		scan = &sync.Mutex{}
		skillCountScanLocks[key] = scan
	}
	skillCountMu.Unlock()
	scan.Lock()
	defer scan.Unlock()
	signature := skillsDirSignature(skillsDir)
	if n, ok := freshSkillCount(key, signature); ok {
		return n
	}
	count := walkSkillCount(skillsDir)
	skillCountMu.Lock()
	skillCountCache[key] = skillCountEntry{signature, nowSeconds(), count}
	skillCountMu.Unlock()
	return count
}

// Last known skill count without touching the skill tree, scheduling one background refresh per recheck window
func cachedSkillCount(profileDir string) int {
	key := filepath.Join(profileDir, "skills")
	now := nowSeconds()
	skillCountMu.Lock()
	due := skillCountNextCheck[key] <= now
	if due {
		skillCountNextCheck[key] = now + skillCountRecheck
	}
	skillCountMu.Unlock()
	if due {
		skillCountRefreshes.Add(1)
		go func() {
			defer skillCountRefreshes.Done()
			countSkills(profileDir)
		}()
	}
	skillCountMu.Lock()
	defer skillCountMu.Unlock()
	return skillCountCache[key].count
}

// Builds the summary of one profile directory
func profileInfo(ctx context.Context, name, path string, isDefault bool, aliasName string, lazySkillCount bool) (ProfileInfo, error) {
	model, provider, err := profiles.ReadConfigModel(ctx, path)
	if err != nil {
		return ProfileInfo{}, err
	}
	distName, distVersion, distSource := profiles.ReadDistributionMeta(path)
	meta := profiles.ReadProfileMeta(path)
	aliasPath := ""
	if aliasName != "" {
		if aliasPath, err = profiles.FindOurWrapper(aliasName); err != nil {
			return ProfileInfo{}, err
		}
	}
	running := checkGatewayRunning(path)
	if !isDefault {
		running = running || servedByRunningMultiplexer(name)
	}
	skillCount := 0
	if lazySkillCount {
		skillCount = cachedSkillCount(path)
	} else {
		skillCount = countSkills(path)
	}
	_, envErr := os.Stat(filepath.Join(path, ".env"))
	return ProfileInfo{
		Name: name, Path: path, IsDefault: isDefault, GatewayRunning: running, Model: model, Provider: provider,
		HasEnv: envErr == nil, SkillCount: skillCount, AliasPath: aliasPath, AliasName: aliasName,
		DistributionName: distName, DistributionVersion: distVersion, DistributionSource: distSource,
		Description: meta.Description, DescriptionAuto: meta.DescriptionAuto, DisplayName: meta.DisplayName,
		BotTitle: meta.BotTitle, PreviousNames: meta.PreviousNames,
	}, nil
}

// Every profile including the default, lazy skill counts serve the last known value for polled callers
func ListProfiles(ctx context.Context, lazySkillCount bool) ([]ProfileInfo, error) {
	var out []ProfileInfo
	def := profiles.DefaultHome()
	if st, err := os.Stat(def); err == nil && st.IsDir() {
		info, err := profileInfo(ctx, "default", def, true, "", lazySkillCount)
		if err != nil {
			return nil, err
		}
		out = append(out, info)
	}
	named, err := profiles.IterNamedProfileDirs(true)
	if err != nil {
		return nil, err
	}
	if len(named) == 0 {
		return out, nil
	}
	aliases, err := profiles.BuildAliasMap()
	if err != nil {
		return nil, err
	}
	for _, entry := range named {
		canon, err := profiles.NormalizeProfileName(filepath.Base(entry))
		if err != nil {
			return nil, err
		}
		info, err := profileInfo(ctx, filepath.Base(entry), entry, false, aliases[canon], lazySkillCount)
		if err != nil {
			return nil, err
		}
		out = append(out, info)
	}
	return out, nil
}

// Skill count of one profile walked on the calling goroutine when stale, for detail views
func CountSkills(profileDir string) int { return countSkills(profileDir) }
