# icebath

Opens the file changes of the icebath TUI running in a VS Code terminal as diffs in the editor, with Revert, and shows its session and mode in the status bar. Approvals stay in the TUI.

## Install

Run `/vsc-install` in the TUI, it installs the extension built into the binary. Auto install in the VS Code panel of the TUI does the same on its own when the TUI starts in a VS Code terminal without it. Without the binary, run the palette command below, it starts the site's installer in a terminal.

```
icebath: install binary
```

On macOS the shortcuts of the TUI need Option sent as Alt. The extension offers to turn `terminal.integrated.macOptionIsMeta` on when a TUI connects, the command below does it at any time.

```
icebath: send Option as Alt
```

## Usage

```
icebath: open            start icebath in a new terminal
icebath: prompt          send a prompt to the connected TUI
icebath: cancel run      stop the run
icebath: sessions        open a stored session in the TUI
icebath: mode            change the permission mode
icebath: show log        protocol traffic

icebath.path             binary when it is not on PATH
icebath.editorContext    send the active file and selection with prompts
icebath.openDiffs        most diffs kept open at once, 8 unless set
```
