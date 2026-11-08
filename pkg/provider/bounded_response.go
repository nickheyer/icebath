package provider

import (
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"sync"
	"time"

	"github.com/nickheyer/icebath/pkg/internal/pycodecs"
)

// Byte cap that holds any real provider error envelope while rejecting pathological bodies
const DefaultErrorBodyMaxBytes = 64 * 1024

// Hard deadline for the whole read, past it the connection is closed and the partial bytes kept
const DefaultErrorBodyTimeout = 10 * time.Second

func boundedResponseLog() *slog.Logger {
	return slog.Default().With("logger", "agent.bounded_response")
}

// Reads a non OK streaming body under a byte cap and a hard deadline, partial text and never an error
func ReadStreamingErrorBody(response *http.Response, maxBytes int, timeout time.Duration) string {
	var mu sync.Mutex
	var chunks []byte
	truncated := false
	done := make(chan struct{})
	go func() {
		defer close(done)
		buf := make([]byte, 32*1024)
		total := 0
		for {
			n, err := response.Body.Read(buf)
			if n > 0 {
				chunk := buf[:n]
				mu.Lock()
				remaining := maxBytes - total
				if len(chunk) > remaining {
					if remaining > 0 {
						chunks = append(chunks, chunk[:remaining]...)
					}
					truncated = true
					mu.Unlock()
					return
				}
				chunks = append(chunks, chunk...)
				total += len(chunk)
				mu.Unlock()
			}
			if err != nil {
				if err != io.EOF {
					boundedResponseLog().Debug(fmt.Sprintf("bounded error-body read failed: %s", err))
				}
				return
			}
		}
	}()
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	select {
	case <-done:
	case <-timer.C:
		mu.Lock()
		soFar := len(chunks)
		mu.Unlock()
		boundedResponseLog().Debug(fmt.Sprintf("bounded error-body read: hard timeout after %.1fs (%d bytes so far)", timeout.Seconds(), soFar))
	}
	_ = response.Body.Close()
	mu.Lock()
	defer mu.Unlock()
	if truncated {
		boundedResponseLog().Debug(fmt.Sprintf("bounded error-body read: capped at %d bytes (max=%d)", len(chunks), maxBytes))
	}
	text, _ := pycodecs.DecodeReplace(chunks, "utf-8")
	return text
}
