package skills

// Appends the skill's resolved config values block the way the cron prompt builder reads it
func (s *Scope) InjectSkillConfig(loaded map[string]any, parts *[]string) {
	s.injectSkillConfig(loaded, parts)
}
