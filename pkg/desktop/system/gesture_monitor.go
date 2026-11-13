package system

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"io/fs"
	"os/exec"
	"sync"
	"syscall"
	"time"
)

// GestureStatus is the helper state, code and reason set only for errors
type GestureStatus struct {
	Type   string `json:"type"`
	Code   string `json:"code,omitempty"`
	Reason string `json:"reason,omitempty"`
}

// GestureMonitorOptions name the helper binary, how its gesture lines parse and its deadlines
type GestureMonitorOptions[T any] struct {
	Path           string
	ParseGesture   func(message map[string]any) *T
	StartupTimeout time.Duration
	StopTimeout    time.Duration
}

// GestureMonitor supervises a native helper that prints one JSON message per line
type GestureMonitor[T any] struct {
	mu      sync.Mutex
	o       GestureMonitorOptions[T]
	cleanup *func()
}

// Monitor over the helper at o.Path
func NewGestureMonitor[T any](o GestureMonitorOptions[T]) *GestureMonitor[T] {
	return &GestureMonitor[T]{o: o}
}

func spawnFailure(err error) GestureStatus {
	status := GestureStatus{Type: "error", Code: "unavailable"}
	if errors.Is(err, fs.ErrNotExist) || errors.Is(err, exec.ErrNotFound) {
		status.Reason = "missing-helper"
	}
	return status
}

// Spawns the helper, reporting starting, ready, errors and gestures, the permission prompt variant waiting a minute
func (m *GestureMonitor[T]) Start(onGesture func(T), onStatus func(GestureStatus), requestPermission bool) {
	m.Stop()
	var args []string
	startup := m.o.StartupTimeout
	if requestPermission {
		args = []string{"--request-permission"}
	}
	if startup == 0 {
		startup = 5 * time.Second
		if requestPermission {
			startup = 60 * time.Second
		}
	}
	stopTimeout := m.o.StopTimeout
	if stopTimeout == 0 {
		stopTimeout = time.Second
	}
	cmd := exec.Command(m.o.Path, args...)
	stdin, errIn := cmd.StdinPipe()
	stdout, errOut := cmd.StdoutPipe()
	if errIn != nil || errOut != nil {
		onStatus(GestureStatus{Type: "error", Code: "unavailable"})
		return
	}
	if err := cmd.Start(); err != nil {
		onStatus(spawnFailure(err))
		return
	}
	var mu sync.Mutex
	ready, active := false, true
	var startupTimer *time.Timer
	var stop func()
	dispose := func() {
		active = false
		if startupTimer != nil {
			startupTimer.Stop()
		}
		m.mu.Lock()
		if m.cleanup == &stop {
			m.cleanup = nil
		}
		m.mu.Unlock()
	}
	terminate := func(status GestureStatus) {
		mu.Lock()
		if !active {
			mu.Unlock()
			return
		}
		dispose()
		mu.Unlock()
		stdin.Close()
		cmd.Process.Signal(syscall.SIGTERM)
		time.AfterFunc(stopTimeout, func() { cmd.Process.Kill() })
		onStatus(status)
	}
	fail := func() { terminate(GestureStatus{Type: "error", Code: "unavailable"}) }
	stop = func() { terminate(GestureStatus{Type: "stopped"}) }
	m.mu.Lock()
	m.cleanup = &stop
	m.mu.Unlock()
	onStatus(GestureStatus{Type: "starting"})
	startupTimer = time.AfterFunc(startup, fail)
	go func() {
		pending := []byte{}
		chunk := make([]byte, 65536)
		for {
			n, err := stdout.Read(chunk)
			if n > 0 {
				mu.Lock()
				live := active
				mu.Unlock()
				if !live {
					continue
				}
				pending = append(pending, chunk[:n]...)
				lines := bytes.Split(pending, []byte("\n"))
				pending = lines[len(lines)-1]
				if len(pending) > 4096 {
					fail()
					continue
				}
				for _, line := range lines[:len(lines)-1] {
					mu.Lock()
					live := active
					mu.Unlock()
					if !live {
						break
					}
					if len(line) > 4096 {
						fail()
						break
					}
					var message map[string]any
					if json.Unmarshal(line, &message) != nil || message == nil {
						continue
					}
					switch {
					case message["type"] == "ready":
						mu.Lock()
						first := !ready
						ready = true
						mu.Unlock()
						if first {
							startupTimer.Stop()
							onStatus(GestureStatus{Type: "ready"})
						}
					case message["type"] == "error" && (message["code"] == "permission-required" || message["code"] == "unavailable"):
						terminate(GestureStatus{Type: "error", Code: message["code"].(string)})
					default:
						mu.Lock()
						isReady := ready
						mu.Unlock()
						if isReady {
							if gesture := m.o.ParseGesture(message); gesture != nil {
								onGesture(*gesture)
							}
						}
					}
				}
			}
			if err != nil {
				if !errors.Is(err, io.EOF) && !errors.Is(err, fs.ErrClosed) {
					fail()
				}
				break
			}
		}
		cmd.Wait()
		mu.Lock()
		unexpected := active
		if active {
			dispose()
		}
		mu.Unlock()
		if unexpected {
			onStatus(GestureStatus{Type: "error", Code: "unavailable"})
		}
	}()
}

// Stops the running helper, reporting stopped
func (m *GestureMonitor[T]) Stop() {
	m.mu.Lock()
	cleanup := m.cleanup
	m.mu.Unlock()
	if cleanup != nil {
		(*cleanup)()
	}
}
