package app

import (
	"bufio"
	"bytes"
	"fmt"
	"io"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/nickheyer/icebath/pkg/internal/pystr"
	"github.com/nickheyer/icebath/pkg/logging"
)

// Longest child output record the drain logs, a longer line is dropped whole
const desktopConsoleLineLimit = 128 * 1024

// Logger of the desktop launcher records, routed to the gui component log
func desktopLog() *logging.Logger { return logging.GetLogger("hermes_cli.desktop") }

// Logs the packaged Windows launch message and prints it on every other launch
func desktopLaunchNotice(w io.Writer, message string, sourceMode bool) {
	if guiPlatform() == "win32" && !sourceMode {
		desktopLog().Info(message)
		return
	}
	fmt.Fprintln(w, message)
}

// Child stdio of a desktop launch and the close that ends its drains
type desktopConsoleStreams struct {
	Stdout *os.File
	Stderr *os.File
	close  func()
}

// Closes the write ends and waits at most one second for the drains to finish
func (s *desktopConsoleStreams) Close() {
	if s.close != nil {
		s.close()
	}
}

// Reads one line of at most max bytes, the bytes read and false at end of stream
func readBoundedLine(r *bufio.Reader, max int) ([]byte, bool) {
	var line []byte
	for len(line) < max {
		b, err := r.ReadByte()
		if err != nil {
			return line, len(line) > 0
		}
		line = append(line, b)
		if b == '\n' {
			break
		}
	}
	return line, true
}

// Logs every complete record of a pipe, an oversized line dropped whole and replaced by one notice
func drainDesktopConsole(read *os.File, level int) {
	defer read.Close()
	r := bufio.NewReader(read)
	for {
		data, ok := readBoundedLine(r, desktopConsoleLineLimit+1)
		if !ok {
			return
		}
		if len(data) > desktopConsoleLineLimit {
			for len(data) > 0 && !bytes.HasSuffix(data, []byte("\n")) {
				data, _ = readBoundedLine(r, 8192)
			}
			desktopLog().Log(level, "[desktop] [oversized line omitted]")
			continue
		}
		text := strings.ToValidUTF8(string(data), "\ufffd")
		desktopLog().Log(level, "[desktop] "+pystr.Rstrip(text))
	}
}

// Packaged Windows child stdio drained into the rotating logs, inherited streams for every other launch
func desktopConsoleOutput(sourceMode bool) (*desktopConsoleStreams, error) {
	if guiPlatform() != "win32" || sourceMode {
		return &desktopConsoleStreams{}, nil
	}
	streams := &desktopConsoleStreams{}
	var writers []*os.File
	var wg sync.WaitGroup
	closeAll := func() {
		for _, w := range writers {
			_ = w.Close()
		}
		done := make(chan struct{})
		go func() { wg.Wait(); close(done) }()
		select {
		case <-done:
		case <-time.After(time.Second):
		}
	}
	for _, spec := range []struct {
		name  string
		level int
	}{{"stdout", logging.LevelInfo}, {"stderr", logging.LevelError}} {
		read, write, err := os.Pipe()
		if err != nil {
			closeAll()
			return nil, err
		}
		writers = append(writers, write)
		if spec.name == "stdout" {
			streams.Stdout = write
		} else {
			streams.Stderr = write
		}
		wg.Add(1)
		go func(read *os.File, level int) {
			defer wg.Done()
			drainDesktopConsole(read, level)
		}(read, spec.level)
	}
	streams.close = closeAll
	return streams, nil
}
