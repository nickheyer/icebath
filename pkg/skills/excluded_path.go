package skills

// Reports whether a path has an excluded directory component or sits in a skill support package
func IsExcludedSkillPath(path, root string) bool { return isExcludedSkillPath(path, root) }
