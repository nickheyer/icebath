package tui

import (
	"os"
	"strings"
)

// Working folder with the home folder as a tilde, cut from the left with an ellipsis past max units
func shortCwd(cwd string, maxUnits int) string {
	p := cwd
	if home := os.Getenv("HOME"); home != "" && strings.HasPrefix(cwd, home) {
		p = "~" + cwd[len(home):]
	}
	if unitLen(p) <= maxUnits {
		return p
	}
	return "…" + jsSlice(p, -(maxUnits-1), unitLen(p))
}

// Working folder with the git branch in parentheses, a long branch cut to its last fifteen units
func fmtCwdBranch(cwd, branch string, maxUnits int) string {
	if branch == "" {
		return shortCwd(cwd, maxUnits)
	}
	shown := branch
	if unitLen(branch) > 16 {
		shown = "…" + jsSlice(branch, -15, unitLen(branch))
	}
	tag := " (" + shown + ")"
	return shortCwd(cwd, max(8, maxUnits-unitLen(tag))) + tag
}

// Trimmed project name cut to max units with a trailing ellipsis
func shortProject(projectName string, maxUnits int) string {
	name := strings.TrimFunc(projectName, isJSSpace)
	if unitLen(name) <= maxUnits {
		return name
	}
	return jsSlice(name, 0, max(1, maxUnits-1)) + "…"
}

// Project name before the folder and branch, the project alone when fewer than eight units remain
func fmtProjectCwdBranch(cwd, branch, projectName string, maxUnits int) string {
	project := shortProject(projectName, 18)
	if project == "" {
		return fmtCwdBranch(cwd, branch, maxUnits)
	}
	const separator = " · "
	remaining := maxUnits - unitLen(project) - unitLen(separator)
	if remaining < 8 {
		return shortProject(project, maxUnits)
	}
	return project + separator + fmtCwdBranch(cwd, branch, remaining)
}

// Terminal title of a status marker and the session name, model and folder that are set
func composeTabTitle(marker, sessionName, model, cwd string, maxName int) string {
	name := strings.TrimFunc(sessionName, isJSSpace)
	if unitLen(name) > maxName {
		name = jsSlice(name, 0, maxName-1) + "…"
	}
	var segments []string
	for _, s := range []string{name, model, cwd} {
		if s != "" {
			segments = append(segments, s)
		}
	}
	if len(segments) == 0 {
		return marker
	}
	return marker + " " + strings.Join(segments, " · ")
}
