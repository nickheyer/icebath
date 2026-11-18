package tuiserver

import "github.com/nickheyer/icebath/pkg/tuihost"

// Withdraws the open server to client requests of one session, or of every session when sid is nil
func clearPending(sid *string) {
	reason := "shutdown"
	if sid != nil && *sid != "" {
		reason = "interrupted"
	}
	tuihost.Cancel(sid, reason, nil)
}
