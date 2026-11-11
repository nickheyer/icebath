package skills

import "github.com/nickheyer/icebath/pkg/tools"

// Loadable skills with kanban skills relevant, the shared filtered catalog entry restored afterwards
func (s *Scope) FindAllSkillsKanbanScoped() []map[string]any {
	const key = "false/false"
	skillsCacheMu.Lock()
	saved, had := skillsCache[key]
	delete(skillsCache, key)
	skillsCacheMu.Unlock()
	defer func() {
		skillsCacheMu.Lock()
		delete(skillsCache, key)
		if had {
			skillsCache[key] = saved
		}
		skillsCacheMu.Unlock()
	}()
	scoped := *s
	scoped.Env = tools.WithKanbanToolsetSelection(s.Env, []string{"kanban"})
	return scoped.FindAllSkills(false)
}
