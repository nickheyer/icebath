package gatewayrun

import (
	"context"
	"strings"

	"github.com/nickheyer/icebath/pkg/gateway"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
)

// Raw text of a clarify reply, or its successful voice transcripts joined by blank lines
func (r *GatewayRunner) prepareClarifyReplyText(ctx context.Context, event *gateway.MessageEvent) string {
	if len(pendingEventAudioPaths(event)) == 0 {
		return pystr.Strip(event.Text)
	}
	empty := ""
	_, transcripts := r.transcribePendingAudioEventOnce(ctx, event, &empty)
	kept := []string{}
	for _, tx := range transcripts {
		if s := pystr.Strip(tx); s != "" {
			kept = append(kept, s)
		}
	}
	return strings.Join(kept, "\n\n")
}

// Native image paths queued for the session, emptied once taken
func (r *GatewayRunner) consumePendingNativeImagePaths(sessionKey string) []string {
	paths := []string{}
	r.withPeekedSessionState(sessionKey, func(s *SessionState) {
		paths = append(paths, s.persistent.nativeImagePaths...)
		if len(paths) > 0 {
			s.persistent.nativeImagePaths = []string{}
		}
	})
	return paths
}
