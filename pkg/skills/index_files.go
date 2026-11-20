package skills

import "os"

// Sorted SKILL.md style index paths under a skills dir, skipping excluded trees and skill support dirs
func IterSkillIndexFiles(skillsDir, filename string) []string {
	return iterSkillIndexFiles(skillsDir, filename)
}

// Replaces the directory listing of the skill tree walk until the returned restore runs
func SwapWalkReadDir(f func(dir string) ([]os.DirEntry, error)) (restore func()) {
	old := walkReadDir
	walkReadDir = f
	return func() { walkReadDir = old }
}
