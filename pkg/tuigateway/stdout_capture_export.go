package tuigateway

import (
	"io"

	"github.com/nickheyer/icebath/pkg/internal/pyjson"
)

// Publishes an open request with no frame and no contract check, the hermes cases that seed _open, returning its id
func OpenRequestForTests(sid, method string, params *pyjson.Object) string {
	req := newServerRequest(sid, method, params, nil, nil)
	requestsMu.Lock()
	openRequests = append(openRequests, req)
	requestsMu.Unlock()
	return req.ID
}

// Points the stdio frame stream at w for a case of another package and hands back the restore
func SwapRealStdoutForTests(w io.Writer) func() {
	stdoutMu.Lock()
	saved := realStdout
	realStdout = w
	stdoutMu.Unlock()
	return func() {
		stdoutMu.Lock()
		realStdout = saved
		stdoutMu.Unlock()
	}
}
