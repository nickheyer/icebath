package tuihost

import (
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
)

// Live WebSocket peer double carrying the identity the upgrade credential minted
type loginSocket struct {
	identity map[string]any
	closed   bool
}

func newLoginSocket(userID string) *loginSocket {
	return &loginSocket{identity: map[string]any{"provider": "basic", "user_id": userID}}
}

func (s *loginSocket) Write(frame *pyjson.Object) bool { return true }
func (s *loginSocket) Close()                          {}
func (s *loginSocket) AuthIdentity() map[string]any    { return s.identity }
func (s *loginSocket) Closed() bool                    { return s.closed }
