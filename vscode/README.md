# icebath

Opens the file changes of the icebath TUI running in a VS Code terminal as diffs in the editor, with Revert, and shows its session and mode in the status bar. Approvals stay in the TUI.

## Install

Run `icebath` in an terminal to install this extension idempotently. Without the binary, run the palette command below, it starts the site's installer in a terminal.

```
icebath: install binary
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
```
