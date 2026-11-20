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

// Project name trimmed and cut at the end with an ellipsis past max units
func shortProject(name string, maxUnits int) string {
	name = strings.TrimFunc(name, isJSSpace)
	if unitLen(name) <= maxUnits {
		return name
	}
	return jsSlice(name, 0, max(1, maxUnits-1)) + "…"
}

// Folder label led by the project name, the name alone when the folder would get under eight units and the plain folder label outside a project
func fmtProjectCwdBranch(cwd, branch, project string, maxUnits int) string {
	name := shortProject(project, 18)
	if name == "" {
		return fmtCwdBranch(cwd, branch, maxUnits)
	}
	const separator = " · "
	remaining := maxUnits - unitLen(name) - unitLen(separator)
	if remaining < 8 {
		return shortProject(name, maxUnits)
	}
	return name + separator + fmtCwdBranch(cwd, branch, remaining)
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
