package manage

// Last known skill count of a profile without walking its skill tree
func CachedSkillCount(profileDir string) int { return cachedSkillCount(profileDir) }
