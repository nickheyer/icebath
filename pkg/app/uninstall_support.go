package app

import (
	"fmt"
	"io"
	"os"
	"runtime"

	"github.com/nickheyer/icebath/pkg/cliui"
	"github.com/nickheyer/icebath/pkg/internal/pypath"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
)

// Python sys.platform of the running OS
func uninstallPlatform() string {
	switch runtime.GOOS {
	case "windows":
		return "win32"
	}
	return runtime.GOOS
}

// Marked line printer of the uninstall screens
func uninstallLine(w io.Writer, mark, col, msg string) {
	fmt.Fprintf(w, "%s %s\n", cliui.Color(mark, col), msg)
}

// User home the way Path.home answers
func uninstallUserHome() string {
	h, err := pypath.PathHome()
	if err != nil {
		panic(err)
	}
	return h
}

// Whether a path exists the way Path.exists answers, symlinks followed
func uninstallPathExists(p string) bool {
	_, err := os.Stat(p)
	return err == nil
}

// Three line framed heading of the uninstall screens
func uninstallBox(w io.Writer, middle, col string) {
	fmt.Fprintln(w, cliui.Color("┌─────────────────────────────────────────────────────────┐", col, cliui.Bold))
	fmt.Fprintln(w, cliui.Color(middle, col, cliui.Bold))
	fmt.Fprintln(w, cliui.Color("└─────────────────────────────────────────────────────────┘", col, cliui.Bold))
}

// Asks the user to type yes, printing the cancel line otherwise unless the prompt was aborted
func uninstallConfirmYes(w io.Writer, text string) bool {
	con := console()
	prompter := &cliui.Console{In: con.In, Out: w, Getenv: con.Getenv, StdinTTY: con.StdinIsTTY, StdoutTTY: con.StdoutIsTTY}
	answer, err := prompter.Input("Type '" + cliui.Color("yes", cliui.Yellow) + "' " + text + ": ")
	if err != nil {
		fmt.Fprintln(w)
		fmt.Fprintln(w, "Cancelled.")
		return false
	}
	confirm := pystr.Lower(pystr.Strip(answer))
	if confirm != "yes" {
		fmt.Fprintln(w)
		fmt.Fprintln(w, "Uninstall cancelled.")
	}
	return confirm == "yes"
}
