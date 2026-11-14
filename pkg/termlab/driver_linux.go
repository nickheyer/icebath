package termlab

import (
	"bufio"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"os/signal"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	xterm "github.com/charmbracelet/x/term"
	"golang.org/x/sys/unix"

	"github.com/nickheyer/icebath/pkg/internal/proc"
)

// Environment variable naming the unix socket a driver serves
const DriverEnv = "TERMLAB_DRIVER"

// Command the driver starts on its pseudo terminal, Env laid over the environment the emulator gave the driver
type Program struct {
	Path string
	Args []string
	Env  []string
	Dir  string
}

// Bytes the program wrote and when the driver read them
type Chunk struct {
	At   time.Time
	Data []byte
}

// Pseudo terminal relay between the emulator and the program, answering the test over a socket
type driver struct {
	tty    *os.File
	pty    *os.File
	outMu  sync.Mutex
	mu     sync.Mutex
	cond   *sync.Cond
	chunks []Chunk
	input  []Chunk
	wrote  time.Time
	answer time.Time
	cmd    *exec.Cmd
	exited bool
	code   int
}

// Runs the driver, the probe or a memory limited command when the environment asks for one, returns at once otherwise
func RunHelper() {
	if raw := os.Getenv(limitEnv); raw != "" {
		err := execLimited(raw)
		fmt.Fprintln(os.Stderr, "termlab limit:", err)
		os.Exit(1)
	}
	if os.Getenv(ProbeEnv) != "" {
		if err := probe(); err != nil {
			fmt.Fprintln(os.Stderr, "termlab probe:", err)
			os.Exit(1)
		}
		os.Exit(0)
	}
	path := os.Getenv(DriverEnv)
	if path == "" {
		return
	}
	if err := drive(path); err != nil {
		fmt.Fprintln(os.Stderr, "termlab driver:", err)
		os.Exit(1)
	}
	os.Exit(0)
}

// Puts the emulator side in raw mode, relays both ways and serves the socket until the test hangs up
func drive(path string) error {
	state, err := xterm.MakeRaw(os.Stdin.Fd())
	if err != nil {
		return fmt.Errorf("raw mode: %w", err)
	}
	defer xterm.Restore(os.Stdin.Fd(), state)
	pty, tty, err := openPty()
	if err != nil {
		return fmt.Errorf("pty: %w", err)
	}
	d := &driver{pty: pty, tty: tty}
	d.cond = sync.NewCond(&d.mu)
	if _, err := copySize(os.Stdin, pty); err != nil {
		return fmt.Errorf("window size: %w", err)
	}
	winch := make(chan os.Signal, 1)
	signal.Notify(winch, unix.SIGWINCH)
	go func() {
		for range winch {
			copySize(os.Stdin, pty)
		}
	}()
	ln, err := net.Listen("unix", path)
	if err != nil {
		return err
	}
	defer ln.Close()
	go d.relayInput()
	go d.relayOutput()
	conn, err := ln.Accept()
	if err != nil {
		return err
	}
	defer d.stop()
	return d.serve(conn)
}

// Copies what the emulator sends, answers to queries and keys alike, into the program and keeps it for the test
func (d *driver) relayInput() {
	buf := make([]byte, 4096)
	for {
		n, err := os.Stdin.Read(buf)
		if n > 0 {
			at := time.Now()
			data := append([]byte(nil), buf[:n]...)
			d.mu.Lock()
			d.input = append(d.input, Chunk{At: at, Data: data})
			d.mu.Unlock()
			d.pty.Write(data)
		}
		if err != nil {
			return
		}
	}
}

// Copies program output to the emulator and keeps it stamped with its read time for the test
func (d *driver) relayOutput() {
	buf := make([]byte, 1<<16)
	for {
		n, err := d.pty.Read(buf)
		if n > 0 {
			at := time.Now()
			data := append([]byte(nil), buf[:n]...)
			d.outMu.Lock()
			os.Stdout.Write(data)
			d.outMu.Unlock()
			d.mu.Lock()
			d.chunks = append(d.chunks, Chunk{At: at, Data: data})
			if !d.wrote.IsZero() && d.answer.IsZero() && at.After(d.wrote) {
				d.answer = at
			}
			d.cond.Broadcast()
			d.mu.Unlock()
		}
		if err != nil {
			return
		}
	}
}

// Answers request lines until the connection closes
func (d *driver) serve(conn net.Conn) error {
	defer conn.Close()
	r := bufio.NewReaderSize(conn, 1<<16)
	w := bufio.NewWriterSize(conn, 1<<16)
	for {
		line, err := r.ReadString('\n')
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return err
		}
		reply, err := d.handle(strings.Fields(line))
		if err != nil {
			reply = "err " + strings.ReplaceAll(err.Error(), "\n", " ")
		}
		w.WriteString(reply)
		w.WriteByte('\n')
		if err := w.Flush(); err != nil {
			return err
		}
	}
}

// Carries out one request, returning the reply line without its newline
func (d *driver) handle(f []string) (string, error) {
	if len(f) == 0 {
		return "", errors.New("empty request")
	}
	arg := func(i int) ([]byte, error) {
		if len(f) <= i {
			return nil, fmt.Errorf("%s wants an argument", f[0])
		}
		return base64.StdEncoding.DecodeString(f[i])
	}
	switch f[0] {
	case "size":
		ws, err := unix.IoctlGetWinsize(int(os.Stdin.Fd()), unix.TIOCGWINSZ)
		if err != nil {
			return "", err
		}
		return fmt.Sprintf("ok %d %d %d %d", ws.Col, ws.Row, ws.Xpixel, ws.Ypixel), nil
	case "paint":
		b, err := arg(1)
		if err != nil {
			return "", err
		}
		d.outMu.Lock()
		_, err = os.Stdout.Write(b)
		d.outMu.Unlock()
		return "ok", err
	case "start":
		b, err := arg(1)
		if err != nil {
			return "", err
		}
		var p Program
		if err := json.Unmarshal(b, &p); err != nil {
			return "", err
		}
		pid, err := d.start(p)
		if err != nil {
			return "", err
		}
		return "ok " + strconv.Itoa(pid), nil
	case "write":
		b, err := arg(1)
		if err != nil {
			return "", err
		}
		d.mu.Lock()
		d.wrote, d.answer = time.Now(), time.Time{}
		at := d.wrote
		d.mu.Unlock()
		if _, err := d.pty.Write(b); err != nil {
			return "", err
		}
		return "ok " + strconv.FormatInt(at.UnixNano(), 10), nil
	case "read", "input":
		d.mu.Lock()
		var chunks []Chunk
		if f[0] == "read" {
			chunks, d.chunks = d.chunks, nil
		} else {
			chunks, d.input = d.input, nil
		}
		d.mu.Unlock()
		return "ok" + encodeChunks(chunks), nil
	case "latency":
		if len(f) < 2 {
			return "", errors.New("latency wants a timeout in milliseconds")
		}
		ms, err := strconv.Atoi(f[1])
		if err != nil {
			return "", err
		}
		lat, err := d.latency(time.Duration(ms) * time.Millisecond)
		if err != nil {
			return "", err
		}
		return "ok " + strconv.FormatInt(int64(lat), 10), nil
	case "status":
		d.mu.Lock()
		defer d.mu.Unlock()
		if d.exited {
			return "ok exited " + strconv.Itoa(d.code), nil
		}
		if d.cmd == nil {
			return "ok idle", nil
		}
		return "ok running", nil
	}
	return "", fmt.Errorf("unknown request %q", f[0])
}

// Chunks as space led fields of read time in nanoseconds and base64 data joined by a colon
func encodeChunks(chunks []Chunk) string {
	var sb strings.Builder
	for _, c := range chunks {
		sb.WriteByte(' ')
		sb.WriteString(strconv.FormatInt(c.At.UnixNano(), 10))
		sb.WriteByte(':')
		sb.WriteString(base64.StdEncoding.EncodeToString(c.Data))
	}
	return sb.String()
}

// Chunks back from the fields encodeChunks wrote
func decodeChunks(fields []string) ([]Chunk, error) {
	out := make([]Chunk, 0, len(fields))
	for _, item := range fields {
		ns, data, ok := strings.Cut(item, ":")
		if !ok {
			return nil, fmt.Errorf("chunk %.40q has no time", item)
		}
		at, err := strconv.ParseInt(ns, 10, 64)
		if err != nil {
			return nil, err
		}
		b, err := base64.StdEncoding.DecodeString(data)
		if err != nil {
			return nil, err
		}
		out = append(out, Chunk{At: time.Unix(0, at), Data: b})
	}
	return out, nil
}

// Starts the program as the session leader of the pseudo terminal
func (d *driver) start(p Program) (int, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.cmd != nil {
		return 0, errors.New("program already started")
	}
	cmd := exec.Command(p.Path, p.Args...)
	cmd.Dir = p.Dir
	cmd.Env = mergeEnv(os.Environ(), p.Env)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = d.tty, d.tty, d.tty
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true, Setctty: true, Ctty: 0}
	if err := cmd.Start(); err != nil {
		return 0, err
	}
	d.tty.Close()
	d.cmd = cmd
	go func() {
		err := cmd.Wait()
		code := 0
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			code = ee.ExitCode()
			if ws, ok := ee.Sys().(syscall.WaitStatus); ok && ws.Signaled() {
				code = 128 + int(ws.Signal())
			}
		} else if err != nil {
			code = -1
		}
		d.mu.Lock()
		d.exited, d.code = true, code
		d.cond.Broadcast()
		d.mu.Unlock()
	}()
	return cmd.Process.Pid, nil
}

// Waits for the first output after the last injected write, failing when the timeout passes first
func (d *driver) latency(timeout time.Duration) (time.Duration, error) {
	deadline := time.Now().Add(timeout)
	timer := time.AfterFunc(timeout, func() {
		d.mu.Lock()
		d.cond.Broadcast()
		d.mu.Unlock()
	})
	defer timer.Stop()
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.wrote.IsZero() {
		return 0, errors.New("nothing was written yet")
	}
	for d.answer.IsZero() {
		if d.exited {
			return 0, fmt.Errorf("program exited with code %d before answering", d.code)
		}
		if !time.Now().Before(deadline) {
			return 0, fmt.Errorf("no output within %s of the last write", timeout)
		}
		d.cond.Wait()
	}
	return d.answer.Sub(d.wrote), nil
}

// Kills the program with everything it started
func (d *driver) stop() {
	d.mu.Lock()
	cmd := d.cmd
	d.mu.Unlock()
	if cmd != nil && cmd.Process != nil {
		proc.KillTree(cmd.Process.Pid)
	}
}

// Environment with every override replacing the entry of the same name, the driver socket dropped
func mergeEnv(base, over []string) []string {
	keys := []string{DriverEnv}
	for _, kv := range over {
		k, _, _ := strings.Cut(kv, "=")
		keys = append(keys, k)
	}
	return append(without(base, keys...), over...)
}

// Environment minus the entries of the given names
func without(env []string, keys ...string) []string {
	drop := map[string]bool{}
	for _, k := range keys {
		drop[k] = true
	}
	var out []string
	for _, kv := range env {
		k, _, _ := strings.Cut(kv, "=")
		if !drop[k] {
			out = append(out, kv)
		}
	}
	return out
}
