package agent

import "github.com/nickheyer/icebath/pkg/logging"

// Tags the records the calling goroutine logs with the session id for icebath logs
func setLoggingSessionContext(sessionID string) { logging.SetSessionContext(sessionID) }
