package consoleutil

import (
	"bufio"
	"bytes"
	"io"
	"os"
	"runtime/debug"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/charmbracelet/colorprofile"
	xterm "github.com/charmbracelet/x/term"
	termutil "github.com/nickheyer/icebath/pkg/utils/term"
)

// Region running at the moment, nil when none runs; a process has one terminal to draw on, so at most one runs
var liveActive atomic.Pointer[liveRegion]

// Held while a region starts or ends, so two never overlap
var liveMu sync.Mutex

// Longest Stop waits for a captured stream to empty after giving the descriptor back, the time a child process still holding the pipe gets before it is cut off
const drainGrace = 500 * time.Millisecond

// Longest captured line printed whole above a region, longer ones split
const maxCapturedLine = 64 << 10

// Standard descriptor pointed at a pipe while a region runs: its number, a close on exec duplicate of the terminal it pointed at and that duplicate's number, the read end of the pipe
type capturedStream struct {
	fd     int
	term   *os.File
	termFd uintptr
	read   *os.File
}

// Lines at the bottom of a terminal redrawn in place while a status spinner or progress display runs. Everything else printed to that terminal meanwhile shows above it a whole line at a time: prints of every console writing there are routed above it under its lock, termutil.Stdout and termutil.Stderr are routed to it on every platform, and on Unix the standard output and error descriptors that are terminals point at pipes it drains above itself, catching os.Stdout, os.Stderr, files holding those descriptors and inherited by child processes, without any Go variable changing
type liveRegion struct {
	c       *Console
	frame   func(width int) []segments
	period  time.Duration
	profile colorprofile.Profile
	links   bool
	// where the region draws and the descriptor behind it when it has one
	out   io.Writer
	outFd uintptr
	hasFd bool
	// captured standard descriptors, and the one among them the region draws on
	streams   []*capturedStream
	outStream *capturedStream
	// whether runtime crash reports were pointed at the captured stderr's terminal
	crash bool
	// writers termutil.Stdout and termutil.Stderr are routed to, and what puts the previous routes back
	routes  []*routedStream
	unroute func()

	mu     sync.Mutex
	height int
	closed bool

	stop    chan struct{}
	done    chan struct{}
	drained sync.WaitGroup

	// text written through the routes, waiting for the pump to print it above the region
	qmu      sync.Mutex
	pending  []pendingText
	wake     chan struct{}
	pumpStop chan struct{}
	pumpDone chan struct{}
}

// Starts a region the console redraws every period from frame, which lays the lines out for the width they are drawn at; nil when the console is no terminal or another region runs
func startLive(c *Console, period time.Duration, frame func(width int) []segments) *liveRegion {
	if !c.Terminal || c.Quiet || c.Out == nil || c.Capture != nil || !escapesWork(c.Out) {
		return nil
	}
	liveMu.Lock()
	defer liveMu.Unlock()
	if liveActive.Load() != nil {
		return nil
	}
	p := c.profile()
	l := &liveRegion{c: c, frame: frame, period: period, profile: p, links: p > colorprofile.NoTTY && !c.NoLinks,
		out: c.Out, stop: make(chan struct{}), done: make(chan struct{}),
		wake: make(chan struct{}, 1), pumpStop: make(chan struct{}), pumpDone: make(chan struct{})}
	if f, ok := termutil.As[*os.File](c.Out); ok && f != nil {
		l.out = f
	}
	l.outFd, l.hasFd = termutil.Fd(l.out)
	if l.hasFd && xterm.IsTerminal(l.outFd) {
		l.capture()
	}
	var outRoute, errRoute io.Writer
	if w := l.routed(os.Stdout); w != nil {
		outRoute = w
	}
	if w := l.routed(os.Stderr); w != nil {
		errRoute = w
	}
	l.unroute = termutil.Route(outRoute, errRoute)
	liveActive.Store(l)
	f := l.next()
	l.mu.Lock()
	io.WriteString(l.out, "\x1b[?25l"+l.erase()+l.paint(f))
	l.mu.Unlock()
	for _, s := range l.streams {
		l.drained.Add(1)
		go l.drain(s)
	}
	go l.pump()
	go l.loop()
	return l
}

// Points the standard descriptors that are terminals at pipes, the region then drawing on its own duplicate of the one it draws on, and crash reports sent to the terminal stderr pointed at
func (l *liveRegion) capture() {
	for _, fd := range standardFds {
		if !xterm.IsTerminal(uintptr(fd)) {
			continue
		}
		s, err := captureStream(fd)
		if err != nil {
			continue
		}
		l.streams = append(l.streams, s)
		if uintptr(fd) == l.outFd {
			l.out, l.outFd, l.outStream = s.term, s.termFd, s
		}
		if fd == 2 {
			l.crash = debug.SetCrashOutput(s.term, debug.CrashOptions{}) == nil
		}
	}
}

// Whether descriptor fd is one the region pointed at a pipe
func (l *liveRegion) captures(fd uintptr) bool {
	for _, s := range l.streams {
		if fd == uintptr(s.fd) {
			return true
		}
	}
	return false
}

// Cursor moves erasing the lines drawn last, leaving the cursor where they began, the lock held
func (l *liveRegion) erase() string {
	if l.height == 0 {
		return ""
	}
	return "\r\x1b[2K" + strings.Repeat("\x1b[1A\x1b[2K", l.height-1)
}

// Lines of a region's next frame, laid out for the width they are drawn at, and the rows of the terminal, zero when unknown
type liveFrame struct {
	lines       []segments
	width, rows int
}

// Next frame laid out at the console width narrowed to the terminal's
func (l *liveRegion) next() liveFrame {
	f := liveFrame{width: l.c.width()}
	if l.hasFd {
		if cols, rows, err := termutil.SizeOf(l.outFd); err == nil {
			f.width, f.rows = min(f.width, cols), rows
		}
	}
	f.lines = l.frame(f.width)
	return f
}

// Text drawing a frame as the region, each line cut to its width, a frame taller than the terminal ending in an ellipsis line on its last row; records the height, the lock held
func (l *liveRegion) paint(f liveFrame) string {
	lines := f.lines
	if f.rows > 0 && len(lines) > f.rows {
		lines = append(lines[:f.rows-1:f.rows-1], segments{{"…", Style{}}})
	}
	var b strings.Builder
	for i, line := range lines {
		if i > 0 {
			b.WriteByte('\n')
		}
		b.WriteString(renderLine(line.crop(f.width), l.profile, l.links))
	}
	l.height = len(lines)
	return b.String()
}

// Writes text to dst above the region and redraws the region below it, in one write when dst is what the region draws on, the lock held
func (l *liveRegion) print(dst io.Writer, same bool, text string, f liveFrame) {
	if same {
		io.WriteString(l.out, l.erase()+text+l.paint(f))
		return
	}
	io.WriteString(l.out, l.erase())
	l.height = 0
	io.WriteString(dst, text)
	io.WriteString(l.out, l.paint(f))
}

// Writer a console's prints reach the region's terminal through, whether it is the one the region draws on, false when the console writes elsewhere
func (l *liveRegion) route(c *Console) (io.Writer, bool, bool) {
	if c == l.c {
		return l.out, true, true
	}
	return l.reach(c.Out)
}

// Writer text written to w reaches the region's terminal through: the terminal duplicate of a captured descriptor, else the file w wraps or w itself when it is on that terminal; whether it is the one the region draws on, false when w writes elsewhere
func (l *liveRegion) reach(w io.Writer) (io.Writer, bool, bool) {
	fd, ok := termutil.Fd(w)
	if !ok || !l.hasFd {
		return nil, false, false
	}
	for _, s := range l.streams {
		if fd == uintptr(s.fd) {
			return s.term, s == l.outStream, true
		}
	}
	if fd != l.outFd && !sameTerminal(fd, l.outFd) {
		return nil, false, false
	}
	if f, ok := termutil.As[*os.File](w); ok && f != nil {
		return f, l.out == io.Writer(f), true
	}
	return w, false, true
}

// Prints text a console wrote above the region, false when the console writes to another destination or the region has ended
func (l *liveRegion) above(c *Console, text string) bool {
	dst, same, ok := l.route(c)
	if !ok {
		return false
	}
	f := l.next()
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.closed {
		return false
	}
	l.print(dst, same, text, f)
	return true
}

func (l *liveRegion) loop() {
	defer close(l.done)
	tick := time.NewTicker(l.period)
	defer tick.Stop()
	for {
		select {
		case <-l.stop:
			return
		case <-tick.C:
			f := l.next()
			l.mu.Lock()
			io.WriteString(l.out, l.erase()+l.paint(f))
			l.mu.Unlock()
		}
	}
}

// Prints the lines written to a captured descriptor above the region on the terminal the descriptor pointed at, until the pipe ends or is closed
func (l *liveRegion) drain(s *capturedStream) {
	defer l.drained.Done()
	defer s.read.Close()
	br := bufio.NewReaderSize(s.read, maxCapturedLine)
	for {
		text, err := readLines(br)
		if text != "" {
			f := l.next()
			l.mu.Lock()
			if l.closed {
				io.WriteString(s.term, text)
			} else {
				l.print(s.term, s == l.outStream, text, f)
			}
			l.mu.Unlock()
		}
		if err != nil {
			return
		}
	}
}

// Whole lines from br: the first waited for, then every further one already buffered; a line longer than the buffer or cut off by the end of the stream ended with a newline; the read error, nil while more may follow
func readLines(br *bufio.Reader) (string, error) {
	var b []byte
	for {
		chunk, err := br.ReadSlice('\n')
		b = append(b, chunk...)
		switch {
		case err == bufio.ErrBufferFull:
			b = append(b, '\n')
		case err != nil:
			if len(b) > 0 && b[len(b)-1] != '\n' {
				b = append(b, '\n')
			}
			return string(b), err
		}
		n := br.Buffered()
		if n == 0 || len(b) >= maxCapturedLine {
			return string(b), nil
		}
		if buf, _ := br.Peek(n); bytes.IndexByte(buf, '\n') < 0 {
			return string(b), nil
		}
	}
}

// Writer termutil.Stdout or termutil.Stderr is routed to while a region runs: whole lines queued for the pump to print above the region, never waiting on the region's lock, so a write from anywhere, the region's own drawing included, cannot deadlock; once the region ends, writes go straight to the standard stream
type routedStream struct {
	l *liveRegion
	// the standard stream, and where its lines are printed above the region and whether that is what the region draws on
	real *os.File
	dst  io.Writer
	same bool

	mu   sync.Mutex
	buf  []byte
	done bool
}

// Text a route took in, waiting to be printed
type pendingText struct {
	from *routedStream
	text string
}

// Route for a standard stream when it reaches the region's terminal, nil when it does not
func (l *liveRegion) routed(f *os.File) *routedStream {
	if f == nil {
		return nil
	}
	dst, same, ok := l.reach(f)
	if !ok {
		return nil
	}
	w := &routedStream{l: l, real: f, dst: dst, same: same}
	l.routes = append(l.routes, w)
	return w
}

// Queues the complete lines written, a line longer than maxCapturedLine split; after the region ends writes p to the stream itself
func (w *routedStream) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.done {
		return w.real.Write(p)
	}
	w.buf = append(w.buf, p...)
	if i := bytes.LastIndexByte(w.buf, '\n'); i >= 0 {
		w.l.queue(w, string(w.buf[:i+1]))
		w.buf = append(w.buf[:0], w.buf[i+1:]...)
	}
	if len(w.buf) >= maxCapturedLine {
		w.l.queue(w, string(w.buf)+"\n")
		w.buf = w.buf[:0]
	}
	return len(p), nil
}

// Queues what is left of a line without its newline and sends later writes straight to the stream
func (w *routedStream) finish() {
	w.mu.Lock()
	defer w.mu.Unlock()
	if len(w.buf) > 0 {
		w.l.queue(w, string(w.buf)+"\n")
		w.buf = nil
	}
	w.done = true
}

func (l *liveRegion) queue(from *routedStream, text string) {
	l.qmu.Lock()
	l.pending = append(l.pending, pendingText{from, text})
	l.qmu.Unlock()
	select {
	case l.wake <- struct{}{}:
	default:
	}
}

// Prints queued route text above the region as it arrives, and what is left when told to stop
func (l *liveRegion) pump() {
	defer close(l.pumpDone)
	for {
		select {
		case <-l.wake:
			l.printQueued()
		case <-l.pumpStop:
			l.printQueued()
			return
		}
	}
}

// Prints every queued text above the region, neighbouring texts of one route in one write
func (l *liveRegion) printQueued() {
	l.qmu.Lock()
	batch := l.pending
	l.pending = nil
	l.qmu.Unlock()
	for len(batch) > 0 {
		from := batch[0].from
		var b strings.Builder
		n := 0
		for ; n < len(batch) && batch[n].from == from; n++ {
			b.WriteString(batch[n].text)
		}
		batch = batch[n:]
		f := l.next()
		l.mu.Lock()
		if l.closed {
			io.WriteString(from.real, b.String())
		} else {
			l.print(from.dst, from.same, b.String(), f)
		}
		l.mu.Unlock()
	}
}

// Waits for every captured pipe to reach its end, closing the read ends once drainGrace passes so a child process that keeps a pipe open cannot hold Stop up
func (l *liveRegion) awaitDrains() {
	drained := make(chan struct{})
	go func() {
		l.drained.Wait()
		close(drained)
	}()
	select {
	case <-drained:
		return
	case <-time.After(drainGrace):
	}
	for _, s := range l.streams {
		s.read.Close()
	}
	<-drained
}

// Ends the region: the ticker stopped, the routes put back and what came through them printed, the standard descriptors given back and what was written to them printed, then the last frame kept on screen or erased and every descriptor and goroutine it made gone
func (l *liveRegion) close(keep bool) {
	close(l.stop)
	<-l.done
	l.unroute()
	for _, w := range l.routes {
		w.finish()
	}
	close(l.pumpStop)
	<-l.pumpDone
	for _, s := range l.streams {
		s.release()
	}
	l.awaitDrains()
	var f liveFrame
	if keep {
		f = l.next()
	}
	liveMu.Lock()
	defer liveMu.Unlock()
	l.mu.Lock()
	text := l.erase()
	if keep {
		text += l.paint(f)
		if l.height > 0 {
			text += "\n"
		}
	}
	io.WriteString(l.out, text+"\x1b[?25h")
	l.height, l.closed = 0, true
	l.mu.Unlock()
	liveActive.Store(nil)
	if l.crash {
		debug.SetCrashOutput(nil, debug.CrashOptions{})
	}
	for _, s := range l.streams {
		s.term.Close()
	}
}
