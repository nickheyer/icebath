package skills

import (
	"fmt"
	"path/filepath"
	"strings"
)

// Resolved lock install path strictly under the skills dir, refusing redirects and the root itself
func resolveLockInstallPath(s *Scope, installPath, skillName string) (string, error) {
	normalized, err := normalizeLockInstallPath(installPath, skillName)
	if err != nil {
		return "", err
	}
	target := s.SkillsDir()
	skillsRoot := resolvePath(target)
	for _, part := range strings.Split(normalized, "/") {
		target = filepath.Join(target, part)
		if isPathRedirect(target) {
			return "", fmt.Errorf("Unsafe install path: %s", installPath)
		}
	}
	resolved := resolvePath(target)
	if resolved == skillsRoot || !isRelativeTo(resolved, skillsRoot) {
		return "", fmt.Errorf("Unsafe install path: %s", installPath)
	}
	return resolved, nil
}
