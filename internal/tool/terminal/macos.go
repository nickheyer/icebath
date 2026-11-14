package terminal

import (
	"strings"

	"github.com/nickheyer/icebath/internal/shell"
)

var macOSOpenNoTransform = map[string]bool{
	"-R": true, "-e": true, "-t": true, "-f": true, "-W": true, "-n": true, "-g": true, "-h": true, "-b": true, "-D": true,
}

const appSplice = `"'"$_H_APP"'"`

func osascriptApp(script string) string {
	return "osascript -e '" + script + "' 2>/dev/null"
}

func macOSRaiseLadder(app *string, file string) string {
	appAssign := `_H_APP="$(osascript -e 'tell application "System Events" to get name of first application process whose frontmost is true' 2>/dev/null)"`
	if app != nil {
		appAssign = "_H_APP=" + shell.Quote(*app)
	}
	fileQ := shell.Quote(file)
	activate := osascriptApp("tell application " + appSplice + " to activate")
	frontmost := osascriptApp(`tell application "System Events" to get frontmost of application process ` + appSplice)
	unminimise := osascriptApp(`tell application "System Events" to set visible of every process to true`)
	asn := `lsappinfo find "$_H_APP" 2>/dev/null | awk '{print $1}'`
	setfront := `[ -n "$_H_ASN" ] && lsappinfo setfront "$_H_ASN" 2>/dev/null`
	reopen := `open -a "$_H_APP" ` + fileQ + " 2>/dev/null"
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

func transformMacOSOpen(command, system string) string {
	if system != "darwin" {
		return command
	}
	tokens, err := shell.Split(command)
	if err != nil || len(tokens) == 0 || tokens[0] != "open" {
		return command
	}
	args := tokens[1:]
	if len(args) == 0 {
		return command
	}
	first := args[0]
	var app *string
	file := ""
	if strings.HasPrefix(first, "-") {
		if macOSOpenNoTransform[first] || first != "-a" {
			return command
		}
		if len(args) < 3 {
			return command
		}
		a := args[1]
		file = args[2]
		if strings.HasPrefix(file, "-") || len(tokens) != 4 {
			return command
		}
		app = &a
	} else {
		file = first
		if len(tokens) != 2 {
			return command
		}
	}
	return command + "; " + macOSRaiseLadder(app, file)
}
