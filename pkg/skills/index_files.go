package skills

// Sorted SKILL.md style index paths under a skills dir, skipping excluded trees and skill support dirs
func IterSkillIndexFiles(skillsDir, filename string) []string {
	return iterSkillIndexFiles(skillsDir, filename)
}
