package scenario

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"os"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/charmbracelet/x/ansi"
	"github.com/charmbracelet/x/vt"
	"github.com/gorilla/websocket"
	"golang.org/x/sys/unix"
)

// Buffer safe for a process writing while the scenario reads
type output struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (o *output) Write(p []byte) (int, error) {
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.b.Write(p)
}

func (o *output) String() string {
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.b.String()
}

// Proc is one background process of the binary
type Proc struct {
	s      *S
	args   []string
	pid    int
	cancel context.CancelFunc
	input  io.WriteCloser
	stdout output
	stderr output
	done   chan struct{}
	err    error
}

// Starts the binary in the background, ended at the latest when the scenario finishes
func (s *S) Spawn(args ...string) *Proc {
	s.t.Helper()
	return s.SpawnStep(Step{Args: args})
}

// Starts one step of the binary in the background
func (s *S) SpawnStep(st Step) *Proc {
	s.t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	cmd := s.command(ctx, st, false)
	p := &Proc{s: s, args: st.Args, cancel: cancel, done: make(chan struct{})}
	cmd.Stdout, cmd.Stderr = &p.stdout, &p.stderr
	if st.Stdin != "" {
		cmd.Stdin = strings.NewReader(s.Expand(st.Stdin))
	}
	if st.Pipe {
		input, err := cmd.StdinPipe()
		if err != nil {
			cancel()
			s.t.Fatal(err)
		}
		p.input = input
	}
	if err := cmd.Start(); err != nil {
		cancel()
		s.Failf("starting icebath %s: %v", strings.Join(st.Args, " "), err)
	}
	p.pid = cmd.Process.Pid
	s.procs = append(s.procs, p)
	go func() {
		p.err = cmd.Wait()
		close(p.done)
	}()
	return p
}

// Returns everything the process printed so far without terminal escapes
func (p *Proc) Text() string { return ansi.Strip(p.stdout.String()) + ansi.Strip(p.stderr.String()) }

// Waits until the process printed text matching the pattern and returns the submatches, its early exit failing the scenario
func (p *Proc) Await(pattern string) []string {
	p.s.t.Helper()
	re := regexp.MustCompile(pattern)
	var found []string
	p.s.Eventually("output "+pattern+" of icebath "+strings.Join(p.args, " "), waitLimit, func() bool {
		if found = re.FindStringSubmatch(p.Text()); found != nil {
			return true
		}
		select {
		case <-p.done:
			if found = re.FindStringSubmatch(p.Text()); found != nil {
				return true
			}
			p.s.Failf("icebath %s ended before printing %s\n%s", strings.Join(p.args, " "), pattern, clip(p.Text()))
		default:
		}
		return false
	})
	return found
}

// Writes expanded text to the standard input of a process started with Pipe
func (p *Proc) Send(text string) {
	p.s.t.Helper()
	if p.input == nil {
		p.s.t.Fatalf("icebath %s was not started with Pipe", strings.Join(p.args, " "))
	}
	if _, err := io.WriteString(p.input, p.s.Expand(text)); err != nil {
		p.s.Failf("writing to icebath %s: %v", strings.Join(p.args, " "), err)
	}
}

// Closes the standard input of a process started with Pipe, the end of input for the program
func (p *Proc) CloseInput() {
	if p.input != nil {
		p.input.Close()
	}
}

// Interrupts the process the way Ctrl+C does and returns what it showed once it ended
func (p *Proc) Stop() *Result {
	p.s.t.Helper()
	return p.Signal(syscall.SIGINT)
}

// Sends the signal to the process group and returns what the process showed once it ended
func (p *Proc) Signal(sig syscall.Signal) *Result {
	p.s.t.Helper()
	syscall.Kill(-p.pid, sig)
	select {
	case <-p.done:
	case <-time.After(stopGrace):
		p.s.Failf("icebath %s still ran %s after %s\n%s", strings.Join(p.args, " "), stopGrace, sig, clip(p.Text()))
	}
	return p.Wait()
}

// Waits for the process to end by itself and returns what it showed
func (p *Proc) Wait() *Result {
	p.s.t.Helper()
	select {
	case <-p.done:
	case <-time.After(stepTimeout):
		p.s.Failf("icebath %s did not end within %s\n%s", strings.Join(p.args, " "), stepTimeout, clip(p.Text()))
	}
	r := &Result{s: p.s, Args: p.args, Stdout: p.stdout.String(), Stderr: p.stderr.String()}
	r.Code = exitCode(p.s, p.err)
	return r
}

// Ends a process the scenario left running, interrupt first and a kill after the grace period
func (p *Proc) end() {
	select {
	case <-p.done:
		p.cancel()
		return
	default:
	}
	syscall.Kill(-p.pid, syscall.SIGINT)
	select {
	case <-p.done:
	case <-time.After(stopGrace):
		p.cancel()
		<-p.done
	}
	p.cancel()
}

// Term is the binary running on a pseudo terminal whose output a terminal emulator renders
type Term struct {
	s        *S
	args     []string
	master   *os.File
	pid      int
	cancel   context.CancelFunc
	mu       sync.Mutex
	emu      *vt.SafeEmulator
	baseline string
	done     chan struct{}
	err      error
}

// Starts the binary on a fresh pseudo terminal of 120 columns and 40 rows
func (s *S) Terminal(args ...string) *Term {
	s.t.Helper()
	return s.TerminalStep(Step{Args: args})
}

// Starts one step of the binary on a fresh pseudo terminal
func (s *S) TerminalStep(st Step) *Term {
	s.t.Helper()
	master, err := os.OpenFile("/dev/ptmx", os.O_RDWR|unix.O_NOCTTY|unix.O_CLOEXEC, 0)
	if err != nil {
		s.t.Fatal(err)
	}
	fd := int(master.Fd())
	if err := unix.IoctlSetPointerInt(fd, unix.TIOCSPTLCK, 0); err != nil {
		s.t.Fatal(err)
	}
	number, err := unix.IoctlGetInt(fd, unix.TIOCGPTN)
	if err != nil {
		s.t.Fatal(err)
	}
	slave, err := os.OpenFile("/dev/pts/"+strconv.Itoa(number), os.O_RDWR|unix.O_NOCTTY, 0)
	if err != nil {
		s.t.Fatal(err)
	}
	if err := unix.IoctlSetWinsize(fd, unix.TIOCSWINSZ, &unix.Winsize{Row: 40, Col: 120}); err != nil {
		s.t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cmd := s.command(ctx, st, true)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = slave, slave, slave
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true, Setctty: true, Ctty: 0}
	if err := cmd.Start(); err != nil {
		cancel()
		s.Failf("starting icebath %s on a terminal: %v", strings.Join(st.Args, " "), err)
	}
	slave.Close()
	tm := &Term{s: s, args: st.Args, master: master, pid: cmd.Process.Pid, cancel: cancel, emu: vt.NewSafeEmulator(120, 40), done: make(chan struct{})}
	tm.emu.SetScrollbackSize(50000)
	go func() {
		chunk := make([]byte, 32*1024)
		for {
			n, err := master.Read(chunk)
			if n > 0 {
				tm.mu.Lock()
				tm.emu.Write(chunk[:n])
				tm.mu.Unlock()
			}
			if err != nil {
				return
			}
		}
	}()
	go io.Copy(master, tm.emu)
	go func() {
		tm.err = cmd.Wait()
		close(tm.done)
	}()
	s.closers = append(s.closers, tm.end)
	return tm
}

// Returns the rows the terminal shows now
func (tm *Term) Screen() string {
	tm.mu.Lock()
	defer tm.mu.Unlock()
	return tm.emu.String()
}

// Returns every row that scrolled off the terminal followed by the rows it shows now
func (tm *Term) Text() string {
	tm.mu.Lock()
	defer tm.mu.Unlock()
	var b strings.Builder
	if back := tm.emu.Scrollback(); back != nil {
		for _, line := range back.Lines() {
			b.WriteString(strings.TrimRight(line.String(), " "))
			b.WriteByte('\n')
		}
	}
	b.WriteString(tm.emu.String())
	return b.String()
}

// Waits until the pattern occurs on the terminal more often than it did before the last input
func (tm *Term) Await(pattern string) {
	tm.s.t.Helper()
	re := regexp.MustCompile(pattern)
	before := len(re.FindAllStringIndex(tm.baseline, -1))
	tm.s.Eventually("terminal text "+pattern, waitLimit, func() bool {
		if len(re.FindAllStringIndex(tm.Text(), -1)) > before {
			return true
		}
		select {
		case <-tm.done:
			if len(re.FindAllStringIndex(tm.Text(), -1)) <= before {
				tm.s.Failf("icebath %s ended before showing %s\n%s", strings.Join(tm.args, " "), pattern, clip(tm.Text()))
			}
			return true
		default:
		}
		return false
	})
}

// Types raw keys, later waits counting only what the terminal shows beyond its content at this moment
func (tm *Term) Send(keys string) {
	tm.s.t.Helper()
	tm.baseline = tm.Text()
	if _, err := tm.master.Write([]byte(keys)); err != nil {
		tm.s.Failf("typing into icebath %s: %v", strings.Join(tm.args, " "), err)
	}
}

// Types a line, waits until the screen shows its end, pauses past paste detection and presses Enter
func (tm *Term) Line(text string) {
	tm.s.t.Helper()
	tm.Send(text)
	tail := []rune(text)
	if len(tail) > 40 {
		tail = tail[len(tail)-40:]
	}
	tm.s.Eventually("the terminal showing the typed text "+text, waitLimit, func() bool {
		return strings.Contains(tm.Screen(), string(tail))
	})
	time.Sleep(1200 * time.Millisecond)
	if _, err := tm.master.Write([]byte("\r")); err != nil {
		tm.s.Failf("typing into icebath %s: %v", strings.Join(tm.args, " "), err)
	}
}

// Waits for the program on the terminal to end and returns its exit code
func (tm *Term) Wait() int {
	tm.s.t.Helper()
	select {
	case <-tm.done:
	case <-time.After(stepTimeout):
		tm.s.Failf("icebath %s on a terminal did not end within %s\n%s", strings.Join(tm.args, " "), stepTimeout, clip(tm.Text()))
	}
	return exitCode(tm.s, tm.err)
}

// Kills a terminal program the scenario left running and closes the terminal
func (tm *Term) end() {
	select {
	case <-tm.done:
	default:
		syscall.Kill(-tm.pid, syscall.SIGHUP)
		select {
		case <-tm.done:
		case <-time.After(stopGrace):
			tm.cancel()
			<-tm.done
		}
	}
	tm.cancel()
	tm.master.Close()
}

// Reply is one HTTP answer
type Reply struct {
	s      *S
	what   string
	Status int
	Header http.Header
	Body   string
}

// Sends one HTTP request, a non string body sent as JSON, headers given as name and value pairs
func (s *S) HTTP(method, url string, body any, headers ...string) *Reply {
	s.t.Helper()
	var payload io.Reader
	contentType := ""
	switch b := body.(type) {
	case nil:
	case string:
		payload = strings.NewReader(s.Expand(b))
	default:
		raw, err := json.Marshal(b)
		if err != nil {
			s.t.Fatal(err)
		}
		payload, contentType = bytes.NewReader(raw), "application/json"
	}
	url = s.Expand(url)
	req, err := http.NewRequest(method, url, payload)
	if err != nil {
		s.t.Fatal(err)
	}
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	for i := 0; i+1 < len(headers); i += 2 {
		req.Header.Set(headers[i], s.Expand(headers[i+1]))
	}
	client := &http.Client{Timeout: stepTimeout, Transport: &http.Transport{Proxy: nil}}
	defer client.CloseIdleConnections()
	resp, err := client.Do(req)
	if err != nil {
		s.Failf("%s %s: %v", method, url, err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		s.Failf("%s %s: reading the answer: %v", method, url, err)
	}
	return &Reply{s: s, what: method + " " + url, Status: resp.StatusCode, Header: resp.Header, Body: string(raw)}
}

// Requires the status code
func (r *Reply) Code(status int) *Reply {
	r.s.t.Helper()
	if r.Status != status {
		r.s.Failf("%s answered %d, want %d\n%s", r.what, r.Status, status, clip(r.Body))
	}
	return r
}

// Parses the body as one JSON value
func (r *Reply) JSON() any {
	r.s.t.Helper()
	var v any
	if err := json.Unmarshal([]byte(r.Body), &v); err != nil {
		r.s.Failf("%s did not answer JSON: %v\n%s", r.what, err, clip(r.Body))
	}
	return v
}

// Event is one server sent event of a streamed answer
type Event struct {
	Name string
	Data string
}

// Splits a text/event-stream body into its events
func (r *Reply) Events() []Event {
	var events []Event
	for _, frame := range strings.Split(strings.ReplaceAll(r.Body, "\r\n", "\n"), "\n\n") {
		ev, data, seen := Event{}, []string{}, false
		for _, line := range strings.Split(frame, "\n") {
			if rest, ok := strings.CutPrefix(line, "event:"); ok {
				ev.Name, seen = strings.TrimSpace(rest), true
			}
			if rest, ok := strings.CutPrefix(line, "data:"); ok {
				data, seen = append(data, strings.TrimPrefix(rest, " ")), true
			}
		}
		if seen {
			ev.Data = strings.Join(data, "\n")
			events = append(events, ev)
		}
	}
	return events
}

// Waits until a GET of the address is answered with any status
func (s *S) WaitHTTP(url string) {
	s.t.Helper()
	url = s.Expand(url)
	client := &http.Client{Timeout: 2 * time.Second, Transport: &http.Transport{Proxy: nil}}
	defer client.CloseIdleConnections()
	s.Eventually("an answer from "+url, waitLimit, func() bool {
		resp, err := client.Get(url)
		if err != nil {
			return false
		}
		resp.Body.Close()
		return true
	})
}

// Socket is one WebSocket connection to a product server
type Socket struct {
	s      *S
	conn   *websocket.Conn
	nextID int
	Events []map[string]any
}

// Opens a WebSocket connection, headers given as name and value pairs, closed when the scenario finishes
func (s *S) Dial(url string, headers ...string) *Socket {
	s.t.Helper()
	url = s.Expand(url)
	h := http.Header{}
	for i := 0; i+1 < len(headers); i += 2 {
		h.Set(headers[i], s.Expand(headers[i+1]))
	}
	dialer := websocket.Dialer{HandshakeTimeout: 30 * time.Second}
	conn, resp, err := dialer.Dial(url, h)
	if err != nil {
		status := ""
		if resp != nil {
			status = " (" + resp.Status + ")"
		}
		s.Failf("WebSocket %s: %v%s", url, err, status)
	}
	s.closers = append(s.closers, func() { conn.Close() })
	return &Socket{s: s, conn: conn}
}

// Returns the subprotocol the server selected
func (w *Socket) Subprotocol() string { return w.conn.Subprotocol() }

// Sends one JSON message
func (w *Socket) Send(v any) {
	w.s.t.Helper()
	if err := w.conn.WriteJSON(v); err != nil {
		w.s.Failf("WebSocket send: %v", err)
	}
}

// Reads the next JSON object message, three minutes of silence failing the scenario
func (w *Socket) Next() map[string]any {
	w.s.t.Helper()
	w.conn.SetReadDeadline(time.Now().Add(waitLimit))
	_, raw, err := w.conn.ReadMessage()
	if err != nil {
		w.s.Failf("WebSocket read: %v", err)
	}
	var msg map[string]any
	if err := json.Unmarshal(raw, &msg); err != nil {
		w.s.Failf("WebSocket message is not a JSON object: %v\n%s", err, clip(string(raw)))
	}
	return msg
}

// Reads messages until one satisfies the check and returns it, the others kept in Events
func (w *Socket) Until(what string, check func(map[string]any) bool) map[string]any {
	w.s.t.Helper()
	for deadline := time.Now().Add(stepTimeout); time.Now().Before(deadline); {
		msg := w.Next()
		if check(msg) {
			return msg
		}
		w.Events = append(w.Events, msg)
	}
	w.s.Failf("WebSocket never delivered %s", what)
	return nil
}

// Sends a JSON-RPC 2.0 request and returns the response carrying its id, notifications kept in Events
func (w *Socket) Call(method string, params any) map[string]any {
	w.s.t.Helper()
	w.nextID++
	id := float64(w.nextID)
	w.Send(map[string]any{"jsonrpc": "2.0", "id": w.nextID, "method": method, "params": params})
	return w.Until("the response to "+method, func(msg map[string]any) bool {
		_, isCall := msg["method"]
		return !isCall && msg["id"] == id
	})
}

// Sends one line to a unix socket and returns the first line of the answer, relative paths under the product home
func (s *S) UnixLine(rel, line string) string {
	s.t.Helper()
	conn, err := net.DialTimeout("unix", s.Path(rel), 10*time.Second)
	if err != nil {
		s.Failf("connecting to %s: %v", rel, err)
	}
	defer conn.Close()
	conn.SetDeadline(time.Now().Add(30 * time.Second))
	if _, err := conn.Write([]byte(s.Expand(line) + "\n")); err != nil {
		s.Failf("writing to %s: %v", rel, err)
	}
	answer, err := bufio.NewReader(conn).ReadString('\n')
	if err != nil {
		s.Failf("reading from %s: %v (got %q)", rel, err, answer)
	}
	return strings.TrimRight(answer, "\n")
}

// PeerRequest is one request a loopback peer received
type PeerRequest struct {
	Method string
	Path   string
	Header http.Header
	Body   string
}

// Peer is a loopback HTTP server standing in for a service the product talks to
type Peer struct {
	URL      string
	mu       sync.Mutex
	requests []PeerRequest
}

// Starts a loopback HTTP peer that records each request before the handler answers, its address defined as ${PEER_name}
func (s *S) Peer(name string, handler http.Handler) *Peer {
	s.t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		s.t.Fatal(err)
	}
	p := &Peer{URL: "http://" + ln.Addr().String()}
	srv := &http.Server{ReadHeaderTimeout: 30 * time.Second, Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		p.mu.Lock()
		p.requests = append(p.requests, PeerRequest{Method: r.Method, Path: r.URL.RequestURI(), Header: r.Header.Clone(), Body: string(raw)})
		p.mu.Unlock()
		r.Body = io.NopCloser(bytes.NewReader(raw))
		handler.ServeHTTP(w, r)
	})}
	go srv.Serve(ln)
	s.closers = append(s.closers, func() { srv.Close() })
	s.Set("PEER_"+name, p.URL)
	return p
}

// Returns the requests the peer received so far in arrival order
func (p *Peer) Requests() []PeerRequest {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]PeerRequest(nil), p.requests...)
}
