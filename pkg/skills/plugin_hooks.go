package skills

import "github.com/nickheyer/icebath/pkg/tools"

// Every skill name skill_view loads for the env, disabled ones included
func AllSkillNames(env *tools.Env) []string {
	names := []string{}
	for _, skill := range ScopeFromEnv(env).FindAllSkills(false) {
		if name, ok := skill["name"].(string); ok {
			names = append(names, name)
		}
	}
	return names
}

// Copy of the skills hub source id alias table the shared metrics field normalizer reads
func SourceIDAliases() map[string]string {
	out := make(map[string]string, len(sourceIDAliases))
	for k, v := range sourceIDAliases {
		out[k] = v
	}
	return out
}
