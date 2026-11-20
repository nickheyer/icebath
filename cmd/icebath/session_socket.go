package main

import (
	"context"
	"errors"

	"github.com/nickheyer/icebath/pkg/gatewayrun"
	"github.com/nickheyer/icebath/pkg/tuiserver"
)

// Gateway session socket seen through the surface the session handler serves
type servedSessionSocket struct{ gatewayrun.SessionSocket }

func (s servedSessionSocket) Client() *tuiserver.SocketPeer {
	host, port, known := s.Peer()
	if !known {
		return nil
	}
	return &tuiserver.SocketPeer{Host: host, Port: &port}
}

// Next text frame, the peer close reported as the disconnect the session handler ends on
func (s servedSessionSocket) ReceiveText(ctx context.Context) (string, error) {
	text, err := s.SessionSocket.ReceiveText(ctx)
	var closed *gatewayrun.SessionSocketClosed
	if errors.As(err, &closed) {
		return "", &tuiserver.SocketDisconnect{Code: int64(closed.Code), Reason: closed.Reason}
	}
	return text, err
}

// Session handler an admitted socket runs, replaced only where a case breaks it
var sessionHandleWS = tuiserver.HandleWS

// Runs the session of an admitted gateway socket
func serveSessionSocket(ctx context.Context, sock gatewayrun.SessionSocket, identity map[string]any, subprotocol *string, operator bool) error {
	return sessionHandleWS(ctx, servedSessionSocket{sock}, identity, subprotocol, operator)
}
