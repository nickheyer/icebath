package wire

import (
	"context"
	"io"
	"log/slog"
	"time"
)

// Most bytes of a failed reply body kept for its diagnostic
const errorBodyMax = 64 * 1024

// Longest the whole read of a failed reply body may take
const errorBodyTimeout = 10 * time.Second

// Reads at most 64 KiB of a failed reply's body within ten seconds, closing it and keeping what arrived
func ReadErrorBody(ctx context.Context, body io.ReadCloser) []byte {
	chunks := make(chan []byte)
	done := make(chan struct{})
	go func() {
		defer close(chunks)
		buf := make([]byte, 8<<10)
		for {
			n, err := body.Read(buf)
			if n > 0 {
				select {
				case chunks <- append([]byte(nil), buf[:n]...):
				case <-done:
					return
				}
			}
			if err != nil {
				return
			}
		}
	}()
	timer := time.NewTimer(errorBodyTimeout)
	defer timer.Stop()
	var out []byte
	for open := true; open && len(out) < errorBodyMax; {
		var chunk []byte
		select {
		case chunk, open = <-chunks:
			out = append(out, chunk[:min(len(chunk), errorBodyMax-len(out))]...)
		case <-timer.C:
			open = false
		case <-ctx.Done():
			open = false
		}
	}
	close(done)
	if err := body.Close(); err != nil {
		slog.Debug("closing a failed reply body failed", "err", err)
	}
	return out
}
