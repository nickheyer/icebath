package termlab

import (
	"bufio"
	"bytes"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"image"
	"math"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	uv "github.com/charmbracelet/ultraviolet"

	"github.com/nickheyer/icebath/pkg/internal/proc"
	"github.com/nickheyer/icebath/pkg/term"
)

// Terminal emulator a session runs in, with the command line that runs a program inside it
type Emulator struct {
	Name string
	argv func(cmd []string) []string
}

// KDE terminal without its menu and tab bars at 160 by 48 cells
var Konsole = Emulator{Name: "konsole", argv: func(cmd []string) []string {
	return append([]string{"konsole", "--nofork", "--hide-menubar", "--hide-tabbar", "-p", "TerminalColumns=160", "-p", "TerminalRows=48", "-e"}, cmd...)
}}

// X terminal emulating a vt340 so it draws sixel, at 160 by 48 cells
var Xterm = Emulator{Name: "xterm", argv: func(cmd []string) []string {
	return append([]string{"xterm", "-ti", "vt340", "-geometry", "160x48", "-fa", "Monospace", "-fs", "10", "-e"}, cmd...)
}}

// Program running in an emulator on a lab display, reached through the driver socket, with the cell geometry calibration found
type Session struct {
	lab        *Lab
	emu        Emulator
	cmd        *exec.Cmd
	done       chan struct{}
	dir        string
	pid        int
	elog       syncBuffer
	conn       net.Conn
	rd         *bufio.Reader
	reqMu      sync.Mutex
	cols, rows int
	origin     image.Point
	pitchX     float64
	pitchY     float64
	outMu      sync.Mutex
	mirror     *term.Emulator
	out        []byte
	chunks     []Chunk
	input      []Chunk
	watches    []*Watch
	exit       string
	pollErr    error
	stop       chan struct{}
	polled     chan struct{}
}

// Starts the emulator on the display running the test binary as driver, calibrates the cell grid and starts the program
func (l *Lab) Open(e Emulator, p Program) (*Session, error) {
	if err := Missing(e.Name); err != nil {
		return nil, err
	}
	self, err := os.Executable()
	if err != nil {
		return nil, err
	}
	dir, err := os.MkdirTemp("", "termlab-")
	if err != nil {
		return nil, err
	}
	s := &Session{lab: l, emu: e, dir: dir, done: make(chan struct{}), stop: make(chan struct{}), polled: make(chan struct{})}
	if err := s.launch(self); err != nil {
		s.teardown()
		return nil, err
	}
	if err := s.calibrate(); err != nil {
		s.teardown()
		return nil, fmt.Errorf("calibrate %s: %w", e.Name, err)
	}
	raw, err := json.Marshal(p)
	if err != nil {
		s.teardown()
		return nil, err
	}
	f, err := s.request("start " + base64.StdEncoding.EncodeToString(raw))
	if err != nil {
		s.teardown()
		return nil, fmt.Errorf("start %s: %w", p.Path, err)
	}
	if len(f) > 0 {
		s.pid, _ = strconv.Atoi(f[0])
	}
	s.mirror = term.New(s.cols, s.rows, term.Options{})
	go s.poll()
	return s, nil
}

// Clean environment of the emulator, its own home and runtime directory so no desktop setting leaks in
func (s *Session) environ(sock string) ([]string, error) {
	home := filepath.Join(s.dir, "home")
	run := filepath.Join(s.dir, "run")
	for _, d := range []string{home, run} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			return nil, err
		}
	}
	lang := os.Getenv("LANG")
	if !strings.Contains(strings.ToLower(lang), "utf") {
		lang = "C.UTF-8"
	}
	env := []string{
		"PATH=" + os.Getenv("PATH"),
		"HOME=" + home,
		"USER=" + os.Getenv("USER"),
		"LOGNAME=" + os.Getenv("LOGNAME"),
		"LANG=" + lang,
		"SHELL=/bin/sh",
		"XDG_RUNTIME_DIR=" + run,
		"XDG_CONFIG_HOME=" + filepath.Join(home, ".config"),
		"XDG_DATA_HOME=" + filepath.Join(home, ".local", "share"),
		"XDG_STATE_HOME=" + filepath.Join(home, ".local", "state"),
		"XDG_CACHE_HOME=" + filepath.Join(home, ".cache"),
		"DISPLAY=" + s.lab.Name(),
		"QT_QPA_PLATFORM=xcb",
		DriverEnv + "=" + sock,
	}
	return env, nil
}

// Starts the emulator and dials the driver it runs, failing when the emulator exits first
func (s *Session) launch(self string) error {
	sock := filepath.Join(s.dir, "driver.sock")
	env, err := s.environ(sock)
	if err != nil {
		return err
	}
	argv := s.emu.argv([]string{self})
	cmd := exec.Command(argv[0], argv[1:]...)
	cmd.Env = env
	cmd.Dir = filepath.Join(s.dir, "home")
	cmd.Stdout, cmd.Stderr = &s.elog, &s.elog
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true, Pdeathsig: syscall.SIGKILL}
	if err := cmd.Start(); err != nil {
		return err
	}
	s.cmd = cmd
	go func() {
		cmd.Wait()
		close(s.done)
	}()
	deadline := time.Now().Add(20 * time.Second)
	for {
		select {
		case <-s.done:
			return fmt.Errorf("%s exited before its driver answered: %s", s.emu.Name, s.emuLog())
		default:
		}
		conn, err := net.Dial("unix", sock)
		if err == nil {
			s.conn, s.rd = conn, bufio.NewReaderSize(conn, 1<<20)
			break
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("%s driver gave no socket within 20s: %s", s.emu.Name, s.emuLog())
		}
		time.Sleep(25 * time.Millisecond)
	}
	f, err := s.request("size")
	if err != nil {
		return err
	}
	if len(f) < 2 {
		return fmt.Errorf("size reply %v", f)
	}
	s.cols, _ = strconv.Atoi(f[0])
	s.rows, _ = strconv.Atoi(f[1])
	if s.cols < 2 || s.rows < 2 {
		return fmt.Errorf("%s reports a %dx%d grid", s.emu.Name, s.cols, s.rows)
	}
	return nil
}

// Tail of what the emulator printed, for errors
func (s *Session) emuLog() string {
	b := bytes.TrimSpace(s.elog.Bytes())
	if len(b) > 2000 {
		b = b[len(b)-2000:]
	}
	return string(b)
}

// Buffer safe to write from the process copier while the test reads it
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

// Copy of everything written so far
func (b *syncBuffer) Bytes() []byte {
	b.mu.Lock()
	defer b.mu.Unlock()
	return bytes.Clone(b.buf.Bytes())
}

// Finds the pixel origin and pitch of the cell grid, measuring again while transient emulator overlays spoil a measurement
func (s *Session) calibrate() error {
	deadline := time.Now().Add(15 * time.Second)
	for {
		err := s.measure()
		if err == nil {
			_, err = s.request("paint " + b64("\x1b[2J\x1b[H\x1b[?25h"))
			return err
		}
		if time.Now().After(deadline) {
			return err
		}
	}
}

// Inverts the first and last cell of a cleared screen, reading origin and pitch from where the screenshot changed
func (s *Session) measure() error {
	if _, err := s.request("paint " + b64("\x1b[0m\x1b[?25l\x1b[2J\x1b[H")); err != nil {
		return err
	}
	blank, err := s.settle(3 * time.Second)
	if err != nil {
		return err
	}
	marks := fmt.Sprintf("\x1b[1;1H\x1b[7m \x1b[%d;%dH \x1b[0m", s.rows, s.cols)
	if _, err := s.request("paint " + b64(marks)); err != nil {
		return err
	}
	half := image.Rect(0, 0, s.lab.W/2, s.lab.H/2)
	var tl, br image.Rectangle
	deadline := time.Now().Add(3 * time.Second)
	for {
		shot, err := s.settle(3 * time.Second)
		if err != nil {
			return err
		}
		tl = DiffBounds(blank, shot, half)
		br = DiffBounds(blank, shot, image.Rect(tl.Max.X, tl.Max.Y, s.lab.W, s.lab.H))
		if !tl.Empty() && !br.Empty() {
			break
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("inverted corner cells never showed, first %v last %v", tl, br)
		}
	}
	px := float64(br.Min.X-tl.Min.X) / float64(s.cols-1)
	py := float64(br.Min.Y-tl.Min.Y) / float64(s.rows-1)
	if math.Abs(px-float64(tl.Dx())) > 1 || math.Abs(py-float64(tl.Dy())) > 1 || br.Dx() != tl.Dx() || br.Dy() != tl.Dy() {
		return fmt.Errorf("first cell %v and last cell %v disagree with a pitch of %.2fx%.2f", tl, br, px, py)
	}
	s.origin, s.pitchX, s.pitchY = tl.Min, px, py
	return nil
}

// Takes screenshots until two in a row match, returning the settled one
func (s *Session) settle(timeout time.Duration) (*image.RGBA, error) {
	prev, err := s.lab.Shot()
	if err != nil {
		return nil, err
	}
	deadline := time.Now().Add(timeout)
	for {
		time.Sleep(40 * time.Millisecond)
		cur, err := s.lab.Shot()
		if err != nil {
			return nil, err
		}
		if Differs(prev, cur, cur.Bounds()) == 0 {
			return cur, nil
		}
		if time.Now().After(deadline) {
			return nil, fmt.Errorf("screen kept changing for %s", timeout)
		}
		prev = cur
	}
}

// Sends one line to the driver and returns the fields of an ok reply
func (s *Session) request(line string) ([]string, error) {
	s.reqMu.Lock()
	defer s.reqMu.Unlock()
	if s.conn == nil {
		return nil, errors.New("driver not connected")
	}
	s.conn.SetDeadline(time.Now().Add(30 * time.Second))
	if _, err := s.conn.Write([]byte(line + "\n")); err != nil {
		return nil, fmt.Errorf("driver: %w", err)
	}
	reply, err := s.rd.ReadString('\n')
	if err != nil {
		return nil, fmt.Errorf("driver: %w", err)
	}
	reply = strings.TrimSuffix(reply, "\n")
	if msg, ok := strings.CutPrefix(reply, "err "); ok {
		return nil, errors.New(msg)
	}
	f := strings.Fields(reply)
	if len(f) == 0 || f[0] != "ok" {
		return nil, fmt.Errorf("driver replied %q", reply)
	}
	return f[1:], nil
}

// Pulls program output and emulator input from the driver every few milliseconds, and the program exit once it ends
func (s *Session) poll() {
	defer close(s.polled)
	tick := time.NewTicker(15 * time.Millisecond)
	defer tick.Stop()
	fail := func(err error) {
		s.outMu.Lock()
		s.pollErr = err
		s.outMu.Unlock()
	}
	for n := 0; ; n++ {
		select {
		case <-s.stop:
			return
		case <-tick.C:
		}
		got, err := s.pull("read")
		if err != nil {
			fail(err)
			return
		}
		in, err := s.pull("input")
		if err != nil {
			fail(err)
			return
		}
		exit := ""
		if n%10 == 0 {
			st, err := s.request("status")
			if err != nil {
				fail(err)
				return
			}
			if len(st) > 0 && st[0] == "exited" {
				exit = strings.Join(st, " ")
			}
		}
		s.outMu.Lock()
		for _, c := range got {
			s.out = append(s.out, c.Data...)
			s.mirror.Write(c.Data)
			for _, w := range s.watches {
				w.track(c.At)
			}
		}
		s.chunks = append(s.chunks, got...)
		s.input = append(s.input, in...)
		if exit != "" {
			s.exit = exit
		}
		s.outMu.Unlock()
	}
}

// Takes the chunks one driver stream gathered since it was last asked
func (s *Session) pull(verb string) ([]Chunk, error) {
	f, err := s.request(verb)
	if err != nil {
		return nil, err
	}
	return decodeChunks(f)
}

// Everything the program wrote so far, a view that later output never changes
func (s *Session) Output() []byte {
	s.outMu.Lock()
	defer s.outMu.Unlock()
	return s.out[:len(s.out):len(s.out)]
}

// Cells of the screen as the program painted them, read from a mirror emulator fed the same output
func (s *Session) Grid() [][]uv.Cell {
	s.outMu.Lock()
	defer s.outMu.Unlock()
	base := s.mirror.Base()
	out := make([][]uv.Cell, s.rows)
	for y := range out {
		row := make([]uv.Cell, s.cols)
		if l, ok := s.mirror.Row(base + uint64(y)); ok {
			copy(row, l.Cells)
		}
		out[y] = row
	}
	return out
}

// Text of each mirrored cell, a space for a blank cell and empty for the tail of a wide one
func (s *Session) Screen() [][]string {
	grid := s.Grid()
	out := make([][]string, len(grid))
	for y, cells := range grid {
		row := make([]string, len(cells))
		for x, c := range cells {
			switch {
			case c.Content != "":
				row[x] = c.Content
			case c.Width == 0 && x > 0 && cells[x-1].Width > 1:
				row[x] = ""
			default:
				row[x] = " "
			}
		}
		out[y] = row
	}
	return out
}

// Zero based column and row where the text starts on the mirrored screen, false when it is not there
func (s *Session) Find(text string) (col, row int, ok bool) {
	want := []rune(text)
	for y, cells := range s.Screen() {
		for x := range cells {
			match := x+len(want) <= len(cells)
			for k := 0; match && k < len(want); k++ {
				match = cells[x+k] == string(want[k])
			}
			if match {
				return x, y, true
			}
		}
	}
	return 0, 0, false
}

// One row of the mirrored screen as plain text
func Line(cells []string) string { return strings.Join(cells, "") }

// Program output in the pieces the driver read, each with its read time
func (s *Session) Chunks() []Chunk {
	s.outMu.Lock()
	defer s.outMu.Unlock()
	return s.chunks[:len(s.chunks):len(s.chunks)]
}

// What the emulator sent the program, key presses and query answers, in the pieces the driver read
func (s *Session) Input() []Chunk {
	s.outMu.Lock()
	defer s.outMu.Unlock()
	return s.input[:len(s.input):len(s.input)]
}

// Why the session stopped receiving output, empty while it still does
func (s *Session) Ended() string {
	s.outMu.Lock()
	defer s.outMu.Unlock()
	switch {
	case s.pollErr != nil:
		return "driver lost: " + s.pollErr.Error()
	case s.exit != "":
		return "program " + s.exit
	}
	return ""
}

// Injects raw bytes as terminal input, returning when the driver wrote them into the pseudo terminal
func (s *Session) Write(b []byte) (time.Time, error) {
	f, err := s.request("write " + base64.StdEncoding.EncodeToString(b))
	if err != nil {
		return time.Time{}, err
	}
	if len(f) == 0 {
		return time.Time{}, errors.New("write reply carries no time")
	}
	ns, err := strconv.ParseInt(f[0], 10, 64)
	if err != nil {
		return time.Time{}, err
	}
	return time.Unix(0, ns), nil
}

// Types text as the keyboard would send it
func (s *Session) Type(keys string) (time.Time, error) { return s.Write([]byte(keys)) }

// Sends one key as its escape sequence
func (s *Session) Key(seq []byte) (time.Time, error) { return s.Write(seq) }

// Sends an SGR 1006 mouse press or release of a button at the one based column x and row y
func (s *Session) Mouse(x, y, button int, press bool) (time.Time, error) {
	return s.Write(MouseSeq(x, y, button, press))
}

// SGR 1006 bytes of a mouse press or release, button 0 left, 1 middle, 2 right, 64 and 65 wheel
func MouseSeq(x, y, button int, press bool) []byte {
	end := 'm'
	if press {
		end = 'M'
	}
	return fmt.Appendf(nil, "\x1b[<%d;%d;%d%c", button, x, y, end)
}

// Time from the last injected write to the next program output, failing when none arrives within the timeout
func (s *Session) Latency(timeout time.Duration) (time.Duration, error) {
	f, err := s.request("latency " + strconv.FormatInt(timeout.Milliseconds(), 10))
	if err != nil {
		return 0, err
	}
	if len(f) == 0 {
		return 0, errors.New("latency reply carries no duration")
	}
	ns, err := strconv.ParseInt(f[0], 10, 64)
	return time.Duration(ns), err
}

// Screenshot of the whole lab display
func (s *Session) Shot() (*image.RGBA, error) { return s.lab.Shot() }

// Columns and rows of the grid the emulator gave the program
func (s *Session) Size() (cols, rows int) { return s.cols, s.rows }

// Pixel rectangle of the screen covering w by h cells from the zero based column and row
func (s *Session) Cells(col, row, w, h int) image.Rectangle {
	x0 := s.origin.X + int(math.Round(float64(col)*s.pitchX))
	y0 := s.origin.Y + int(math.Round(float64(row)*s.pitchY))
	x1 := s.origin.X + int(math.Round(float64(col+w)*s.pitchX))
	y1 := s.origin.Y + int(math.Round(float64(row+h)*s.pitchY))
	return image.Rect(x0, y0, x1, y1)
}

// Pixels across and down one cell
func (s *Session) Pitch() (float64, float64) { return s.pitchX, s.pitchY }

// Waits until the output satisfies the condition, failing at the timeout or when the program ends first
func (s *Session) WaitFor(cond func([]byte) bool, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for {
		if cond(s.Output()) {
			return nil
		}
		if why := s.Ended(); why != "" {
			time.Sleep(50 * time.Millisecond)
			if cond(s.Output()) {
				return nil
			}
			return errors.New(why)
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("output of %d bytes did not satisfy the wait within %s", len(s.Output()), timeout)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// Takes screenshots until the region differs from the given one, returning the first that does
func (s *Session) WaitChange(from image.Image, region image.Rectangle, timeout time.Duration) (*image.RGBA, error) {
	deadline := time.Now().Add(timeout)
	for {
		cur, err := s.lab.Shot()
		if err != nil {
			return nil, err
		}
		if Differs(from, cur, region) > 0 {
			return cur, nil
		}
		if time.Now().After(deadline) {
			return cur, fmt.Errorf("region %v unchanged for %s", region, timeout)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// Stops polling and kills the emulator with the driver and the program under it
func (s *Session) Close() error {
	close(s.stop)
	<-s.polled
	return s.teardown()
}

// Kills the process tree of the emulator and the session of the program, then removes the session directory
func (s *Session) teardown() error {
	var err error
	if s.cmd != nil && s.cmd.Process != nil {
		select {
		case <-s.done:
		default:
			proc.KillTree(s.cmd.Process.Pid)
		}
		syscall.Kill(-s.cmd.Process.Pid, syscall.SIGKILL)
	}
	if s.pid > 0 {
		syscall.Kill(-s.pid, syscall.SIGKILL)
	}
	if s.conn != nil {
		s.conn.Close()
	}
	if s.cmd != nil {
		select {
		case <-s.done:
		case <-time.After(5 * time.Second):
			err = fmt.Errorf("%s outlived a kill for 5s", s.emu.Name)
		}
	}
	if rmErr := os.RemoveAll(s.dir); err == nil {
		err = rmErr
	}
	return err
}

// Standard base64 of a string, for request arguments
func b64(s string) string { return base64.StdEncoding.EncodeToString([]byte(s)) }
