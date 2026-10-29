package term

import (
	"encoding/hex"
	"fmt"
	"strings"
	"time"

	"github.com/charmbracelet/x/ansi"
)

// Carries out a device control string
func (e *Emulator) dcs(cmd ansi.Cmd, params ansi.Params, data []byte) {
	e.joiner = false
	inter, final := cmd.Intermediate(), cmd.Final()
	switch {
	case inter == '$' && final == 'q':
		e.requestStatus(string(data))
	case inter == '+' && final == 'q':
		e.termcap(string(data))
	case cmd.Prefix() == '=' && final == 's':
		first, _, _ := params.Param(0, 0)
		e.modes.sync = first == 1
		if e.modes.sync {
			e.syncAt = time.Now()
		}
	}
}

// Answers a status string request for the pen, the regions, the cursor style or the protection
func (e *Emulator) requestStatus(what string) {
	s := e.scr
	var value string
	switch what {
	case "m":
		value = strings.TrimSuffix(strings.TrimPrefix(s.cur.pen.String(), "\x1b["), "m")
		if value == "" {
			value = "0"
		}
		value += "m"
	case "r":
		value = fmt.Sprintf("%d;%dr", s.top+1, s.bot+1)
	case "s":
		value = fmt.Sprintf("%d;%ds", s.left+1, s.right+1)
	case " q":
		n := 1
		switch e.shape {
		case CursorUnderline:
			n = 3
		case CursorBar:
			n = 5
		}
		if !e.modes.blinkCursor {
			n++
		}
		value = fmt.Sprintf("%d q", n)
	case "\"q":
		n := 0
		if e.protect {
			n = 1
		}
		value = fmt.Sprintf("%d\"q", n)
	case "\"p":
		value = "62;1\"p"
	case "t":
		value = fmt.Sprintf("%dt", e.h)
	case "$|":
		value = fmt.Sprintf("%d$|", e.w)
	case "*|":
		value = fmt.Sprintf("%d*|", e.h)
	default:
		e.reply("\x1bP0$r\x1b\\")
		return
	}
	e.reply("\x1bP1$r" + value + "\x1b\\")
}

// Terminfo capabilities answered to XTGETTCAP, the keys as the legacy encoder sends them
var termcaps = map[string]string{
	"TN": "xterm-256color", "name": "xterm-256color", "Co": "256", "colors": "256", "RGB": "8/8/8", "Tc": "", "Su": "", "Smulx": "\x1b[4:%p1%dm",
	"setrgbf": "\x1b[38:2:%p1%d:%p2%d:%p3%dm", "setrgbb": "\x1b[48:2:%p1%d:%p2%d:%p3%dm",
	"kbs": "\x7f", "kcbt": "\x1b[Z", "kcub1": "\x1bOD", "kcud1": "\x1bOB", "kcuf1": "\x1bOC", "kcuu1": "\x1bOA",
	"khome": "\x1bOH", "kend": "\x1bOF", "kich1": "\x1b[2~", "kdch1": "\x1b[3~", "kpp": "\x1b[5~", "knp": "\x1b[6~",
	"kf1": "\x1bOP", "kf2": "\x1bOQ", "kf3": "\x1bOR", "kf4": "\x1bOS", "kf5": "\x1b[15~", "kf6": "\x1b[17~", "kf7": "\x1b[18~", "kf8": "\x1b[19~",
	"kf9": "\x1b[20~", "kf10": "\x1b[21~", "kf11": "\x1b[23~", "kf12": "\x1b[24~",
	"smcup": "\x1b[?1049h", "rmcup": "\x1b[?1049l", "smkx": "\x1b[?1h\x1b=", "rmkx": "\x1b[?1l\x1b>",
	"civis": "\x1b[?25l", "cnorm": "\x1b[?25h", "sitm": "\x1b[3m", "ritm": "\x1b[23m", "bold": "\x1b[1m", "sgr0": "\x1b(B\x1b[m",
	"kmous": "\x1b[M", "XM": "\x1b[?1006;1000%?%p1%{1}%=%th%el%;", "Ms": "\x1b]52;%p1%s;%p2%s\x07", "fullkbd": "",
}

// Answers a termcap query naming capabilities in hex, each known one with its value and the rest as invalid
func (e *Emulator) termcap(query string) {
	for _, name := range strings.Split(query, ";") {
		raw, err := hex.DecodeString(name)
		if err != nil {
			e.reply("\x1bP0+r" + name + "\x1b\\")
			continue
		}
		value, ok := termcaps[string(raw)]
		if !ok {
			e.reply("\x1bP0+r" + name + "\x1b\\")
			continue
		}
		e.reply("\x1bP1+r" + name + "=" + hexOf(value) + "\x1b\\")
	}
}
