package agentimport

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/nickheyer/icebath/pkg/cliui"
	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/internal/pypath"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
)

// Prints how to ask each Bot for its export and how to bring it back
func printExportInstructions(c *cliui.Console, promptFile string) {
	fmt.Fprintln(c.Out)
	c.PrintHeader("Step 1 — ask each Grok Bot to export itself")
	c.PrintInfo("Grok Bot keeps your routines in its cloud account and has no export button, so each Bot")
	c.PrintInfo("writes its own export. Open a Bot's chat, paste the message below, and send it.")
	c.PrintInfo("Do this once per Bot you want to bring over. Nothing in Grok Bot is changed.")
	if bots := ReadGrokBotCache().Bots; len(bots) > 0 {
		c.PrintInfo("Your Bots in the Grok Bot app on this computer: " + strings.Join(bots, ", "))
	}
	if promptFile != "" {
		c.PrintInfo("(The same message is saved in " + promptFile + ")")
	}
	fmt.Fprintln(c.Out)
	fmt.Fprint(c.Out, ExportPrompt+"\n")
	c.PrintHeader("Step 2 — bring the export back")
	c.PrintInfo("Copy the Bot's whole reply and press Enter when asked, or save the file it attaches")
	c.PrintInfo("to ~/Downloads (grok-bot-export-<bot>.json, one per Bot) and run this command again.")
	c.PrintInfo("Exports in ~/Downloads named grok-bot-export*.json are found without --source; a file")
	c.PrintInfo("elsewhere: icebath import-agent grok-bot --source <file>")
}

// The export from the console input, piped or pasted at the terminal up to Ctrl-D
func readPastedExport(c *cliui.Console) (string, error) {
	if c.StdinIsTTY() {
		c.PrintInfo("Paste the Bot's whole reply.")
		return readTerminalPaste(c)
	}
	raw, err := io.ReadAll(c.In)
	return string(raw), err
}

// Clipboard text through the OS tool, false when there is none to run
func readClipboardText(ctx context.Context) (string, bool) {
	var commands [][]string
	switch runtime.GOOS {
	case "darwin":
		commands = [][]string{{"pbpaste"}}
	case "windows":
		commands = [][]string{{"powershell", "-NoProfile", "-Command", "[Console]::OutputEncoding=[Text.Encoding]::UTF8; Get-Clipboard -Raw"}}
	default:
		commands = [][]string{{"wl-paste", "--no-newline"}, {"xclip", "-selection", "clipboard", "-o"}, {"xsel", "--clipboard", "--output"}}
	}
	for _, command := range commands {
		exe, err := exec.LookPath(command[0])
		if err != nil {
			continue
		}
		runCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
		cmd := exec.CommandContext(runCtx, exe, command[1:]...)
		cmd.Stdin = nil
		out, err := cmd.Output()
		cancel()
		if err != nil {
			continue
		}
		return pystr.DecodeUTF8Replace(out), true
	}
	return "", false
}

// Reports whether text holds a readable export
func isExport(text string) bool {
	bots, err := ParseExport(text)
	return err == nil && len(bots) > 0
}

// Export files of this run, with none found printing the export steps and asking for a path on a terminal
func resolveExportFiles(ctx context.Context, args *Args) ([]string, int, error) {
	c := args.Console
	if args.Source == "-" {
		return []string{Pasted}, 0, nil
	}
	source := ""
	if args.Source != "" {
		resolved, err := pypath.Resolve(pypath.Expanduser(args.Source))
		if err != nil {
			return nil, 0, err
		}
		source = resolved
		if !exists(source) {
			fmt.Fprintln(c.Out)
			c.PrintError("Export file not found: " + source)
			return nil, 1, nil
		}
	}
	if files := FindExportFiles(source); len(files) > 0 {
		return files, 0, nil
	}
	if source != "" {
		fmt.Fprintln(c.Out)
		c.PrintError("No Grok Bot export found in " + source)
		return nil, 1, nil
	}
	promptFile := ""
	if !args.DryRun {
		promptFile = filepath.Join(home.Dir(ctx), "grok-bot-import", "export-prompt.txt")
		if err := os.MkdirAll(filepath.Dir(promptFile), 0o777); err != nil {
			promptFile = ""
		} else if err := atomicWriteText(ctx, promptFile, ExportPrompt); err != nil {
			promptFile = ""
		}
	}
	printExportInstructions(c, promptFile)
	if args.DryRun || args.Yes || !c.StdinIsTTY() {
		return nil, 0, nil
	}
	picked := ""
	for {
		fmt.Fprintln(c.Out)
		answer, err := c.Input("Copy the Bot's reply and press Enter (or type the path to its file; q to stop): ")
		if err != nil {
			if errors.Is(err, cliui.ErrEOF) || errors.Is(err, cliui.ErrInterrupt) {
				return nil, 0, nil
			}
			return nil, 0, err
		}
		answer = pystr.Strip(answer)
		if low := pystr.Lower(answer); low == "q" || low == "quit" {
			return nil, 0, nil
		}
		if answer == "" {
			if found := FindExportFiles(""); len(found) > 0 {
				return found, 0, nil
			}
			text, ok := readClipboardText(ctx)
			if ok && text != "" && isExport(text) {
				args.pastedText = &text
				return []string{Pasted}, 0, nil
			}
			suffix := ""
			if !ok {
				suffix = " (no clipboard tool found)"
			}
			c.PrintError("The clipboard holds no Grok Bot export" + suffix + ". Copy the Bot's whole reply, or save it as")
			c.PrintError("~/Downloads/grok-bot-export-<bot>.json, then press Enter again.")
			continue
		}
		picked = pypath.Expanduser(strings.Trim(answer, `'"`))
		if exists(picked) {
			break
		}
		discardPendingInput(c)
		shown := []rune(answer)
		if len(shown) > 60 {
			shown = shown[:60]
		}
		c.PrintError(fmt.Sprintf("No file at %s. If that was the reply pasted here: a terminal cuts it short,", pyrepr.StrRepr(string(shown))))
		c.PrintError("so copy it and just press Enter instead (or pipe it in with --source -).")
	}
	found := FindExportFiles(picked)
	if len(found) == 0 {
		c.PrintError("No export found at " + picked)
		return nil, 1, nil
	}
	return found, 0, nil
}
