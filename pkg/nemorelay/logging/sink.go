package logging

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"sync/atomic"
	"time"
)

// Destination of rendered records with its own level filter
type sink interface {
	level() Level
	log(r record)
	flush()
	flushOnExit() error
	setRootRelayID(id string)
}

// Sink failure shaped like the logging library error it stands for
type sinkError struct {
	kind string
	err  error
}

func (e *sinkError) Error() string { return e.kind + " error: " + osErrorText(e.err) }

func (e *sinkError) Unwrap() error { return e.err }

// Writes records straight to standard error
type stderrSink struct {
	mu         sync.Mutex
	filter     Level
	format     LogFormat
	rootID     atomic.Value
	diagnostic SinkRecorder
}

func newStderrSink(config LoggingConfig, rootRelayID string, diagnostic SinkRecorder) *stderrSink {
	s := &stderrSink{filter: config.Level, format: config.StderrFormat, diagnostic: diagnostic}
	s.rootID.Store(rootRelayID)
	return s
}

func (s *stderrSink) level() Level { return s.filter }

func (s *stderrSink) setRootRelayID(id string) { s.rootID.Store(id) }

func (s *stderrSink) log(r record) {
	line := renderRecord(s.format, s.rootID.Load().(string), r)
	s.mu.Lock()
	_, err := os.Stderr.Write(line)
	s.mu.Unlock()
	if err != nil {
		s.report(&sinkError{kind: "write record", err: err})
	}
}

func (s *stderrSink) report(err error) {
	recordSinkRuntimeError(s.diagnostic, err)
	fmt.Fprintf(os.Stderr, "nemo-relay: logging sink error (stderr): %s\n", err)
}

// Standard error is unbuffered so a flush has nothing to write
func (s *stderrSink) flush() {}

func (s *stderrSink) flushOnExit() error { return nil }

// Underlying writer of a file sink, plain append or size rotating
type fileWriter interface {
	io.Writer
	Flush() error
	Close() error
}

type plainFileWriter struct {
	file *os.File
	buf  *bufio.Writer
}

func (w *plainFileWriter) Write(p []byte) (int, error) { return w.buf.Write(p) }
func (w *plainFileWriter) Flush() error                { return w.buf.Flush() }
func (w *plainFileWriter) Close() error {
	err := w.buf.Flush()
	if cerr := w.file.Close(); err == nil {
		err = cerr
	}
	return err
}

// Opens a file in append mode, creating its parent directories
func openPlainFile(path string) (fileWriter, error) {
	if dir := filepath.Dir(path); dir != "" {
		if err := os.MkdirAll(dir, 0o777); err != nil {
			return nil, &sinkError{kind: "create directory", err: err}
		}
	}
	file, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o666)
	if err != nil {
		return nil, &sinkError{kind: "open file", err: err}
	}
	return &plainFileWriter{file: file, buf: bufio.NewWriter(file)}, nil
}

// One task of a file sink's queue, a record or a flush request
type fileTask struct {
	rec   record
	flush bool
	done  chan error
}

// Asynchronous file sink draining a bounded queue on its own goroutine
type fileSink struct {
	label        string
	filter       Level
	format       LogFormat
	rootID       atomic.Value
	queue        chan fileTask
	writer       fileWriter
	diagnostic   SinkRecorder
	recordNotice dropNoticeLimiter
	flushNotice  dropNoticeLimiter
	stopped      chan struct{}
	closeOnce    sync.Once
}

func newFileSink(config FileLogSinkConfig, label string, writer fileWriter, rootRelayID string, diagnostic SinkRecorder) *fileSink {
	s := &fileSink{label: label, filter: config.Level, format: config.Format, queue: make(chan fileTask, config.QueueCapacity),
		writer: writer, diagnostic: diagnostic, stopped: make(chan struct{})}
	s.rootID.Store(rootRelayID)
	go s.run()
	return s
}

func (s *fileSink) level() Level { return s.filter }

func (s *fileSink) setRootRelayID(id string) { s.rootID.Store(id) }

func (s *fileSink) run() {
	defer close(s.stopped)
	for task := range s.queue {
		var err error
		switch {
		case task.flush:
			if ferr := s.writer.Flush(); ferr != nil {
				err = &sinkError{kind: "flush buffer", err: ferr}
			}
		default:
			if _, werr := s.writer.Write(renderRecord(s.format, s.rootID.Load().(string), task.rec)); werr != nil {
				err = &sinkError{kind: "write record", err: werr}
			}
		}
		if err != nil {
			s.reportRuntime(err)
		}
		if task.done != nil {
			task.done <- err
		}
	}
}

func (s *fileSink) reportRuntime(err error) {
	recordSinkRuntimeError(s.diagnostic, err)
	fmt.Fprintf(os.Stderr, "nemo-relay: logging sink error (%s): %s\n", s.label, err)
}

func (s *fileSink) log(r record) {
	select {
	case s.queue <- fileTask{rec: r}:
	default:
		recordSinkDropped(s.diagnostic, false)
		if s.recordNotice.shouldReport(time.Now()) {
			fmt.Fprintf(os.Stderr, "nemo-relay: logging sink (%s): records are being dropped because the queue is full; repeated notices are limited to once per %dms\n", s.label, dropReportIntervalMillis)
		}
	}
}

func (s *fileSink) flush() {
	select {
	case s.queue <- fileTask{flush: true}:
	default:
		recordSinkDropped(s.diagnostic, true)
		if s.flushNotice.shouldReport(time.Now()) {
			fmt.Fprintf(os.Stderr, "nemo-relay: logging sink (%s): flush requests are being dropped because the queue is full; repeated notices are limited to once per %dms\n", s.label, dropReportIntervalMillis)
		}
	}
}

// Drains every queued task then flushes and closes the file
func (s *fileSink) flushOnExit() error {
	var err error
	s.closeOnce.Do(func() {
		done := make(chan error, 1)
		s.queue <- fileTask{flush: true, done: done}
		err = <-done
		close(s.queue)
		<-s.stopped
		if cerr := s.writer.Close(); cerr != nil && err == nil {
			err = &sinkError{kind: "flush buffer", err: cerr}
		}
	})
	return err
}

const dropReportIntervalMillis = 1000

// Limits a repeated stderr notice to once per report interval
type dropNoticeLimiter struct{ last atomic.Int64 }

func (l *dropNoticeLimiter) shouldReport(now time.Time) bool {
	millis := now.UnixMilli()
	for {
		last := l.last.Load()
		if millis-last < dropReportIntervalMillis {
			return false
		}
		if l.last.CompareAndSwap(last, millis) {
			return true
		}
	}
}

// File writer that rotates to numbered backups once a write would exceed the size limit
type sizeRotatingFileWriter struct {
	basePath      string
	file          *os.File
	buf           *bufio.Writer
	currentSize   uint64
	maxFileSize   uint64
	retainedFiles int
}

func newSizeRotatingFileWriter(basePath string, maxFileSize uint64, retainedFiles int) (*sizeRotatingFileWriter, error) {
	if dir := filepath.Dir(basePath); dir != "" {
		if err := os.MkdirAll(dir, 0o777); err != nil {
			return nil, &sinkError{kind: "open file", err: err}
		}
	}
	w := &sizeRotatingFileWriter{basePath: basePath, maxFileSize: maxFileSize, retainedFiles: retainedFiles}
	if err := w.open(false); err != nil {
		return nil, &sinkError{kind: "open file", err: err}
	}
	return w, nil
}

func (w *sizeRotatingFileWriter) open(truncate bool) error {
	flags := os.O_CREATE | os.O_WRONLY
	if truncate {
		flags |= os.O_TRUNC
	} else {
		flags |= os.O_APPEND
	}
	file, err := os.OpenFile(w.basePath, flags, 0o666)
	if err != nil {
		return err
	}
	info, err := file.Stat()
	if err != nil {
		file.Close()
		return err
	}
	w.file, w.buf, w.currentSize = file, bufio.NewWriter(file), uint64(info.Size())
	return nil
}

func (w *sizeRotatingFileWriter) Write(p []byte) (int, error) {
	if w.currentSize != 0 && w.currentSize+uint64(len(p)) > w.maxFileSize {
		if err := w.rotate(); err != nil {
			return 0, err
		}
	}
	if w.buf == nil {
		return 0, errors.New("rotating log file is not open")
	}
	if _, err := w.buf.Write(p); err != nil {
		return 0, err
	}
	w.currentSize += uint64(len(p))
	return len(p), nil
}

func (w *sizeRotatingFileWriter) rotate() error {
	if w.buf == nil {
		return errors.New("rotating log file is not open")
	}
	if err := w.buf.Flush(); err != nil {
		return err
	}
	w.file.Close()
	w.file, w.buf = nil, nil
	if err := rotateFiles(w.basePath, w.retainedFiles); err != nil {
		if rerr := w.open(false); rerr != nil {
			return fmt.Errorf("log rotation failed: %s; failed to reopen active log file: %s", osErrorText(err), osErrorText(rerr))
		}
		return err
	}
	if err := w.open(true); err != nil {
		return err
	}
	w.currentSize = 0
	return nil
}

func (w *sizeRotatingFileWriter) Flush() error {
	if w.buf == nil {
		return errors.New("rotating log file is not open")
	}
	return w.buf.Flush()
}

func (w *sizeRotatingFileWriter) Close() error {
	if w.buf == nil {
		return nil
	}
	err := w.buf.Flush()
	if cerr := w.file.Close(); err == nil {
		err = cerr
	}
	w.file, w.buf = nil, nil
	return err
}

// Shifts the active file and its backups up by one, dropping the oldest
func rotateFiles(basePath string, retainedFiles int) error {
	for index := retainedFiles; index >= 1; index-- {
		source := basePath
		if index != 1 {
			source = rotatedLogPath(basePath, index-1)
		}
		if _, err := os.Stat(source); err != nil {
			continue
		}
		if err := os.Rename(source, rotatedLogPath(basePath, index)); err != nil {
			return err
		}
	}
	return nil
}

// Backup path with the index inserted before the extension
func rotatedLogPath(basePath string, index int) string {
	ext := filepath.Ext(basePath)
	name := filepath.Base(basePath)
	stem := name[:len(name)-len(ext)]
	if stem == "" {
		stem, ext = name, ""
	}
	return filepath.Join(filepath.Dir(basePath), stem+"."+strconv.Itoa(index)+ext)
}
