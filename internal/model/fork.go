package model

import (
	"context"
	"maps"
)

func (s *Session) Fork(ctx context.Context, o SessionOptions) (*Session, error) {
	f, err := s.r.Open(ctx, s.Route(), o)
	if err != nil {
		return nil, err
	}
	s.mu.Lock()
	f.route = s.route
	f.sessionID = s.sessionID
	f.echoOptIn = s.echoOptIn
	f.lmOptions = maps.Clone(s.lmOptions)
	f.ollamaThinking = maps.Clone(s.ollamaThinking)
	f.rejectedCarriers = maps.Clone(s.rejectedCarriers)
	f.carriersLoadedFor = s.carriersLoadedFor
	f.thinkingFor = s.thinkingFor
	f.thinkingBound = s.thinkingBound
	f.thinking = maps.Clone(s.thinking)
	f.imageRejecting = maps.Clone(s.imageRejecting)
	f.fastUntil = s.fastUntil
	f.fastUnavailable = maps.Clone(s.fastUnavailable)
	f.effortRejected = s.effortRejected
	f.disableRejected = s.disableRejected
	f.floorRequired = s.floorRequired
	if s.wireReasoning != nil {
		w := *s.wireReasoning
		w.Payload = append(w.Payload[:0:0], w.Payload...)
		f.wireReasoning = &w
	}
	f.portalDecided = s.portalDecided
	f.portalPending = s.portalPending
	f.portalUpstream = s.portalUpstream
	f.allTurnsRejected = maps.Clone(s.allTurnsRejected)
	f.drop1MBeta = s.drop1MBeta
	f.bufferToolInput = s.bufferToolInput
	s.mu.Unlock()
	s.exec.mu.Lock()
	f.exec.noStream = s.exec.noStream
	f.exec.noStreamOptions = s.exec.noStreamOptions
	s.exec.mu.Unlock()
	return f, nil
}
