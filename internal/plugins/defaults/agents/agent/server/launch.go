package server

import (
	"context"
	"encoding/json"
	"slices"
	"strings"

	checkpointsconfig "github.com/nickheyer/icebath/internal/checkpoints/config"
	launchconfig "github.com/nickheyer/icebath/internal/launch/config"
	agentconfig "github.com/nickheyer/icebath/internal/plugins/defaults/agents/agent/config"
	agentv1 "github.com/nickheyer/icebath/pkg/gen/icebath/plugins/agent/v1"
	sessionsv1 "github.com/nickheyer/icebath/pkg/gen/icebath/sessions/v1"
)

// Key of the model settings of a session holding the skills a launch loaded into its prompt
const skillsKey = "_skills"

// Skills a launch loaded into the session's prompt, as it keeps them
func storedSkills(session *sessionsv1.Session) []string {
	var recorded struct {
		Skills []string `json:"_skills"`
	}
	if session.GetModelConfig() == "" || json.Unmarshal([]byte(session.GetModelConfig()), &recorded) != nil {
		return nil
	}
	return recorded.Skills
}

// Names of the skills of a launch, comma separated or one by one, each once in order
func skillNames(given []string) []string {
	var names []string
	for _, item := range given {
		for _, name := range strings.Split(item, ",") {
			if name = strings.TrimSpace(name); name != "" && !slices.Contains(names, name) {
				names = append(names, name)
			}
		}
	}
	return names
}

// Takes what a launch of the command line asks of the session from now on: the settings it sets over the profile's and the skills its
// prompt loads, kept with the session so a later run of it keeps them, each part given replacing what it held
func (a *Agent) launched(ctx context.Context, id string, launch *agentv1.Launch) error {
	if launch == nil {
		return nil
	}
	r, err := a.run(id)
	if err != nil {
		return err
	}
	store, err := a.store()
	if err != nil {
		return err
	}
	values := map[string]any{}
	if launch.MaxTurns != nil {
		values[agentconfig.MaxTurns] = max(int(launch.GetMaxTurns()), 0)
	}
	if launch.RunBudget != nil {
		values[agentconfig.RunBudget] = max(launch.GetRunBudget(), 0)
	}
	if launch.PassSessionId != nil {
		values[agentconfig.PassSessionID] = launch.GetPassSessionId()
	}
	if launch.IgnoreRules != nil {
		values[agentconfig.IgnoreRules] = launch.GetIgnoreRules()
	}
	if launch.Checkpoints != nil {
		values[checkpointsconfig.Enabled] = launch.GetCheckpoints()
	}
	var bare *bool
	if launch.IgnoreUserConfig != nil {
		bare = new(launch.GetIgnoreUserConfig())
	}
	if launch.SafeMode != nil {
		values[launchconfig.SafeMode] = launch.GetSafeMode()
		if launch.GetSafeMode() {
			values[agentconfig.IgnoreRules] = true
			bare = new(true)
		}
	}
	skills := skillNames(launch.GetSkills())
	if len(values) == 0 && bare == nil && len(skills) == 0 {
		return nil
	}
	r.mu.Lock()
	overrides := r.overrides
	r.mu.Unlock()
	merged := overrides.Merge(values, bare)
	change := merged.Change()
	if len(skills) > 0 {
		change.ModelConfig[skillsKey] = skills
	}
	if err := store.Update(ctx, id, change); err != nil {
		return err
	}
	r.mu.Lock()
	r.overrides = merged
	if len(skills) > 0 {
		r.skills = skills
	}
	r.mu.Unlock()
	a.changed(r)
	return nil
}
