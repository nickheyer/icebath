package skills

// Bundled manifest names plus the curator suppression list, for callers ranking the skill catalog
func (s *Scope) ReadBundledNames() (map[string]bool, error) { return s.readBundledNames() }

// Hub lock names plus the frontmatter name of each in tree install path, for callers ranking the skill catalog
func (s *Scope) ReadHubInstalledNames() map[string]bool { return s.readHubInstalledNames() }
