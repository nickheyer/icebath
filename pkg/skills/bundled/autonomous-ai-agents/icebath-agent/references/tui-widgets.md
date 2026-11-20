# TUI Widgets — Live Panels in the Terminal UI

A widget is a small view a plugin registers with `Host.RegisterWidget` of the
Go plugin library `github.com/nickheyer/icebath/pkg/pluginsdk`. Its id becomes
a slash command of the terminal UI (bare `icebath`). An ambient widget sits in
a box beside the transcript or in a dock row; a modal widget opens a window
over the session and takes every key.

## When to Use

- The user asks for a live panel in the terminal UI (clock, countdown, status
  card, a readout of an API).
- The user wants a modal tool bound to a slash command (picker, calculator,
  viewer).

## Prerequisites

- The terminal UI must be in use. Widgets do not render in `chat -q` runs or
  on messaging platforms.
- Go to build the plugin program. Network backed widgets need whatever
  credentials their API needs; a failing fetch shows as an error line in the
  widget, never a crash.

## How to Run

1. Copy `templates/clock-widget/` (go.mod, main.go, plugin.yaml) to
   `~/.icebath/plugins/clock-widget/`.
2. Build it there: `go mod tidy && go build -o clock-widget .` — the binary
   name matches the `entry` of plugin.yaml.
3. Activate it: `icebath plugins enable clock-widget`.
4. In the terminal UI run `/widgets-reload`, then `/clock` or
   `/clock Asia/Tokyo`. `/clock` again closes it.

## Quick Reference

```go
pluginsdk.Register(func(h pluginsdk.Host) error {
	_, err := h.RegisterWidget(pluginsdk.Widget{
		ID:      "clock",          // slash command name
		Help:    "live clock",     // shown in the / completion
		Usage:   "usage: /clock [timezone]",
		Mode:    "ambient",        // "ambient" beside the transcript, "modal" takes keys
		Zone:    "top-right",      // ambient placement
		Width:   26,               // box width in columns
		Refresh: time.Second,      // refresh interval, zero for input only
		Open:    open,             // argument to first state, false prints Usage
		Tick:    nil,              // next state at each refresh, nil to render again only
		Render:  render,           // state to styled lines
	})
	return err
})
```

`main` runs `pluginsdk.Main()`. plugin.yaml names the program:

```yaml
name: clock-widget
version: 0.1.0
description: Live clock beside the transcript of the terminal UI
runtime: program
entry:
  - clock-widget
```

## Refresh

`Refresh` is the interval the terminal UI refreshes a shown widget at, on the
frame clock of its window manager: each time the interval passes it runs
`Tick` and renders the widget again. Shortest interval 100 ms. A clock needs no `Tick`: its `Render`
reads the time on every refresh. A countdown keeps its state in `Tick`:

```go
Refresh: time.Second,
Tick: func(_ context.Context, state any) (any, error) {
	left, _ := state.(float64)
	return max(left-1, 0), nil
},
```

A `Tick` without a `Refresh` is refused at registration. A `Tick` that fails
prints its error and the widget refreshes no more until it is opened again.

## Contract

- State is a JSON value: what `Open` answers is what `Tick`, `Input` and
  `Render` receive back (a Go struct arrives as a `map[string]any`).
- `Render` gets `WidgetFrame{Cols, Rows, State}` and answers lines of spans;
  `Role` picks a theme color (`primary`, `accent`, `text`, `muted`, `label`,
  `ok`, `error`, `warn`), never a hex value, so widgets follow `/theme`.
- Ambient widgets take no keys; the command toggles them. Zones:
  `top-left`, `top-right` (default), `bottom-left`, `bottom-right` lay a box
  over a corner of the transcript, `dock-top` and `dock-bottom` put a row
  above or below it. Keep them small (6 rows or fewer) and pad changing
  numbers so the box keeps its size.
- Modal widgets get every key as `WidgetInput{Text, Key, Ctrl, Alt, Shift}`
  (`Key` like `esc`, `enter`, `up`); `Input` answers the next state and
  `open`, false closing the window. Without `Input`, esc, q or ctrl+c close it.

## Pitfalls

- A modal widget needs a way out: answer `open` false on esc.
- An id a built in widget already uses is shadowed by the built in one.
- A plugin that is not enabled never registers: check
  `icebath plugins list`, then `/widgets-reload`.

## Verification

`/widgets-reload`, then `/<id>`: an ambient widget appears in its zone while
the composer keeps taking input, and a refreshing one changes on its interval;
`/<id>` again removes it.
