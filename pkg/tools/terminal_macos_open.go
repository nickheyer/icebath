package tools

import "runtime"

// Open flags that do not open a file in a new frontmost window
var macosOpenNoTransformFlags = map[string]bool{
	"-R": true, "-e": true, "-t": true, "-f": true, "-W": true, "-n": true, "-g": true, "-h": true, "-b": true, "-D": true,
}

// Shell splice that breaks out of a single quoted osascript argument so the shell expands the app name
const macosAppSplice = `"'"$_H_APP"'"`

// Wraps an AppleScript snippet in an osascript invocation with errors suppressed
func osascriptApp(script string) string {
	return "osascript -e '" + script + "' 2>/dev/null"
}

// Shell raise ladder appended after a macOS open command so the opened document lands in front
func buildMacosOpenRaiseLadder(app string, hasApp bool, file string) string {
	appAssign := `_H_APP="$(osascript -e 'tell application "System Events" to get name of first application process whose frontmost is true' 2>/dev/null)"`
	if hasApp {
		appAssign = "_H_APP=" + shQuote(app)
	}
	fileQ := shQuote(file)
	activate := osascriptApp("tell application " + macosAppSplice + " to activate")
	frontmost := osascriptApp(`tell application "System Events" to get frontmost of application process ` + macosAppSplice)
	unminimise := osascriptApp(`tell application "System Events" to set visible of every process to true`)
	asn := `lsappinfo find "$_H_APP" 2>/dev/null | awk '{print $1}'`
	setfront := `[ -n "$_H_ASN" ] && lsappinfo setfront "$_H_ASN" 2>/dev/null`
	reopen := `open -a "$_H_APP" ` + fileQ + ` 2>/dev/null`
	frontmostName := `osascript -e 'tell application "System Events" to get name of first application process whose frontmost is true' 2>/dev/null`
	report := "if " + frontmost + "; then echo open: " + fileQ + " is now frontmost; " +
		"else _H_FOCUS=$(" + frontmostName + "); " +
		"echo open: " + fileQ + " opened but another application holds focus; " +
		`echo "frontmost: $_H_FOCUS"; fi`
	return appAssign + "; " +
		"if ! " + frontmost + "; then " +
		activate + "; sleep 0.2; " +
		"if ! " + frontmost + "; then " +
		"_H_ASN=$(" + asn + "); " + setfront + "; sleep 0.2; " +
		"if ! " + frontmost + "; then " +
		reopen + "; sleep 0.2; " + unminimise + "; " + activate + "; " +
		"fi; " +
		"fi; " +
		"fi; " +
		"sleep 0.8; " +
		"if ! " + frontmost + "; then " + activate + "; sleep 0.4; fi; " +
		report
}

// TransformMacosOpenCommand appends the frontmost raise ladder to a bare open command on the given system
func TransformMacosOpenCommand(command, system string) string {
	if system != "Darwin" {
		return command
	}
	tokens, err := shlexSplit(command)
	if err != nil || len(tokens) == 0 || tokens[0] != "open" {
		return command
	}
	args := tokens[1:]
	if len(args) == 0 {
		return command
	}
	first := args[0]
	var app, file string
	hasApp := false
	if len(first) > 0 && first[0] == '-' {
		if macosOpenNoTransformFlags[first] {
			return command
		}
		if first != "-a" {
			return command
		}
		if len(args) < 3 {
			return command
		}
		app, file = args[1], args[2]
		hasApp = true
		if len(file) > 0 && file[0] == '-' {
			return command
		}
		if len(tokens) != 4 {
			return command
		}
	} else {
		file = first
		if len(tokens) != 2 {
			return command
		}
	}
	return command + "; " + buildMacosOpenRaiseLadder(app, hasApp, file)
}

// Platform name in the Python platform.system spelling
func hostSystemName() string {
	switch runtime.GOOS {
	case "darwin":
		return "Darwin"
	case "windows":
		return "Windows"
	case "linux":
		return "Linux"
	}
	return runtime.GOOS
}
