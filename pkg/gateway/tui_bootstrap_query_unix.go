//go:build !windows

package gateway

import (
	"bytes"
	"errors"
	"io"
	"time"
)

// One response line of at most 65537 bytes from the private control socket, each receive bounded by timeout
func TUIBootstrapControlQuery(home string, request []byte, timeout time.Duration) ([]byte, error) {
	peer, err := connectPrivate(home, timeout)
	if err != nil {
		return nil, err
	}
	defer peer.Close()
	if err := peer.SetWriteDeadline(time.Now().Add(timeout)); err != nil {
		return nil, err
	}
	if _, err := peer.Write(request); err != nil {
		return nil, err
	}
	var line []byte
	chunk := make([]byte, 4096)
	for !bytes.Contains(line, []byte("\n")) && len(line) < 65537 {
		if err := peer.SetReadDeadline(time.Now().Add(timeout)); err != nil {
			return nil, err
		}
		n, err := peer.Read(chunk[:min(len(chunk), 65537-len(line))])
		line = append(line, chunk[:n]...)
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, err
		}
	}
	if i := bytes.IndexByte(line, '\n'); i >= 0 {
		line = line[:i+1]
	}
	return line, nil
}
