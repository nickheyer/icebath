package termlab

import (
	"fmt"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"sync"

	xterm "github.com/charmbracelet/x/term"
	"golang.org/x/sys/unix"
)

// Environment variable that turns the test binary into the probe program the rig checks itself with
const ProbeEnv = "TERMLAB_PROBE"

// Echoes each terminal read as hex, reports the window size at start and on change, runs mark and resize commands
func probe() error {
	fd := os.Stdin.Fd()
	state, err := xterm.MakeRaw(fd)
	if err != nil {
		return err
	}
	defer xterm.Restore(fd, state)
	var mu sync.Mutex
	emit := func(s string) {
		mu.Lock()
		os.Stdout.WriteString(s)
		mu.Unlock()
	}
	size := func() {
		if ws, err := unix.IoctlGetWinsize(int(fd), unix.TIOCGWINSZ); err == nil {
			emit(fmt.Sprintf("size %dx%d\r\n", ws.Col, ws.Row))
		}
	}
	winch := make(chan os.Signal, 1)
	signal.Notify(winch, unix.SIGWINCH)
	go func() {
		for range winch {
			size()
		}
	}()
	size()
	buf := make([]byte, 4096)
	for {
		n, err := os.Stdin.Read(buf)
		if n > 0 {
			emit(fmt.Sprintf("in %x\r\n", buf[:n]))
		}
		if cmd, ok := strings.CutSuffix(string(buf[:n]), "\r"); ok {
			if f := strings.Fields(cmd); len(f) == 3 {
				a, _ := strconv.Atoi(f[1])
				b, _ := strconv.Atoi(f[2])
				switch f[0] {
				case "mark":
					emit(fmt.Sprintf("\x1b7\x1b[%d;%dH\x1b[7m \x1b[0m\x1b8", b+1, a+1))
				case "resize":
					emit(fmt.Sprintf("\x1b[8;%d;%dt", b, a))
				}
			}
		}
		if err != nil {
			return err
		}
	}
}
