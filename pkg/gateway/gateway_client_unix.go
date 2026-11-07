//go:build !windows

package gateway

import (
	"bytes"
	"errors"
	"io"
	"time"
)

// Raw answer of one request line sent to the home control socket as this OS user
func privateControlQuery(home string, request []byte, timeout time.Duration) ([]byte, error) {
	deadline := time.Now().Add(timeout)
	peer, err := connectPrivate(home, timeout)
	if err != nil {
		return nil, err
	}
	defer peer.Close()
	if _, err := peer.Write(request); err != nil {
		return nil, err
	}
	var data []byte
	chunk := make([]byte, 4096)
	for !bytes.Contains(data, []byte("\n")) {
		budget := time.Until(deadline)
		if budget <= 0 {
			return nil, &GatewayClientError{Msg: "Gateway bootstrap timed out"}
		}
		if err := peer.SetReadDeadline(time.Now().Add(budget)); err != nil {
			return nil, err
		}
		n, err := peer.Read(chunk)
		if n == 0 && err != nil && !errors.Is(err, io.EOF) {
			return nil, err
		}
		if n == 0 || len(data)+n > 65536 {
			return nil, &GatewayClientError{Msg: "Invalid gateway bootstrap response"}
		}
		data = append(data, chunk[:n]...)
	}
	return data, nil
}
