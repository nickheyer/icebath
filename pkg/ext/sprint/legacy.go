package sprint

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/nickheyer/icebath/pkg/harness"
	"github.com/nickheyer/icebath/pkg/profiles"
)

// File the first version of the extension kept its settings in, under the ext directory of the configuration
const legacyFile = "sprint.json"

// Settings of the first version of the extension, with its role names
type legacySettings struct {
	PM          legacyRole `json:"pm"`
	Planner     legacyRole `json:"planner"`
	Lead        legacyRole `json:"lead"`
	Engineer    legacyRole `json:"engineer"`
	Team        legacyTeam `json:"team"`
	Limits      map[string]string
	Interactive *bool `json:"interactive"`
}

type legacyRole struct {
	Profile   string `json:"profile"`
	Model     string `json:"model"`
	MaxTokens int    `json:"max_tokens"`
	MaxRounds int    `json:"max_rounds"`
	Budget    int    `json:"budget"`
}

type legacyTeam struct {
	MaxEngineers int   `json:"max_engineers"`
	Parallel     *bool `json:"parallel"`
	MaxRework    *int  `json:"max_rework"`
	MaxTickets   int   `json:"max_tickets"`
}

// Where the first version kept its settings
func legacyPath() (string, error) {
	dir, err := profiles.ConfigDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "ext", legacyFile), nil
}

// Moves the settings of the first version into the configuration once and sets the old file aside
func migrateLegacy(a *harness.App) (int, error) {
	path, err := legacyPath()
	if err != nil {
		return 0, err
	}
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	for _, s := range Settings() {
		if a.Config.Setting(s.Key) != "" {
			return 0, nil
		}
	}
	var old legacySettings
	if err := json.Unmarshal(data, &old); err != nil {
		return 0, fmt.Errorf("%s: %w", path, err)
	}
	values := legacyValues(old)
	moved := 0
	for _, s := range Settings() {
		v, ok := values[s.Key]
		if !ok || v == "" {
			continue
		}
		if err := s.Validate(v); err != nil {
			return moved, fmt.Errorf("%s: %w", path, err)
		}
		if err := a.Config.SaveSetting(a.Dir, s.Key, v); err != nil {
			return moved, err
		}
		moved++
	}
	if err := os.Rename(path, path+".migrated"); err != nil {
		return moved, err
	}
	return moved, nil
}

// Settings of this version with the values of the first, the roles renamed and the limits renamed
func legacyValues(old legacySettings) map[string]string {
	out := map[string]string{}
	roles := map[Role]legacyRole{RoleDirector: old.PM, RoleArchitect: old.Planner, RoleReviewer: old.Lead, RoleBuilder: old.Engineer}
	for role, r := range roles {
		out[roleKey(role, "profile")] = strings.TrimSpace(r.Profile)
		out[roleKey(role, "model")] = strings.TrimSpace(r.Model)
		if r.MaxTokens > 0 {
			out[roleKey(role, "max_tokens")] = strconv.Itoa(r.MaxTokens)
		}
		if r.MaxRounds > 0 {
			out[roleKey(role, "max_rounds")] = strconv.Itoa(r.MaxRounds)
		}
		if r.Budget > 0 {
			out[roleKey(role, "budget")] = strconv.Itoa(r.Budget)
		}
	}
	if old.Team.MaxEngineers > 0 {
		out[Name+".team.builders"] = strconv.Itoa(min(old.Team.MaxEngineers, 8))
	}
	if old.Team.Parallel != nil {
		out[Name+".team.parallel"] = strconv.FormatBool(*old.Team.Parallel)
	}
	if old.Team.MaxRework != nil {
		out[Name+".team.max_rework"] = strconv.Itoa(*old.Team.MaxRework)
	}
	if old.Team.MaxTickets > 0 {
		out[Name+".team.max_tickets"] = strconv.Itoa(old.Team.MaxTickets)
	}
	for from, to := range map[string]string{"total": "total", "refine": "brief", "plan": "plan", "build": "build", "review": "review"} {
		if v := strings.TrimSpace(old.Limits[from]); v != "" {
			out[Name+".limits."+to] = v
		}
	}
	if old.Interactive != nil {
		out[Name+".interactive"] = strconv.FormatBool(*old.Interactive)
	}
	return out
}
