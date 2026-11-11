package dashboard

import (
	"context"
	"strconv"
	"strings"

	"github.com/nickheyer/icebath/pkg/home"
)

// Body caps of the chat upload routes, the attachment or image cap plus multipart overhead
const (
	ChatFileBodyMaxBytes  = home.WebappAttachmentMaxBytes + 64*1024
	ChatImageMaxBytes     = 25 * 1024 * 1024
	ChatImageBodyMaxBytes = 4*((ChatImageMaxBytes+2)/3) + 64*1024
)

var uploadBodyMaxBytes = map[string]int64{
	"/api/chat/file-upload":  ChatFileBodyMaxBytes,
	"/api/chat/image-upload": ChatImageBodyMaxBytes,
}

// Refuses a chat upload past its route cap with 413 on the surfaces that serve browser files
func ChatFileUploadLimitMiddleware(app ASGIApp) ASGIApp {
	return func(ctx context.Context, scope *Scope, receive Receive, send Send) error {
		path, root := scope.Path, scope.RootPath
		if root != "" && strings.HasPrefix(path, root+"/") {
			path = path[len(root):]
		}
		bodyMax, capped := uploadBodyMaxBytes[path]
		if scope.Type != "http" || scope.Method != "POST" || !capped || !uploadPolicyServesFiles(scope) {
			return app(ctx, scope, receive, send)
		}
		rejection := MustJSONResponse(jsonObj("detail", "Upload request is too large"), 413)
		length := strings.TrimLeft(headersOfScope(scope).Get("content-length"), "0")
		if length == "" {
			length = "0"
		}
		limitText := strconv.FormatInt(bodyMax, 10)
		if isASCII(length) && allDecimal(length) {
			if len(length) > len(limitText) {
				return rejection.Serve(ctx, scope, receive, send)
			}
			if n, err := strconv.ParseInt(length, 10, 64); err == nil && n > bodyMax {
				return rejection.Serve(ctx, scope, receive, send)
			}
		}
		var received int64
		exceeded := false
		limited := func(ctx context.Context) (Message, error) {
			m, err := receive(ctx)
			if err != nil {
				return m, err
			}
			if m.Type == "http.request" {
				received += int64(len(m.Body))
				if received > bodyMax {
					exceeded = true
					return m, &MultiPartException{Message: "Upload request is too large"}
				}
			}
			return m, nil
		}
		limitedSend := func(ctx context.Context, m Message) error {
			if !exceeded {
				return send(ctx, m)
			}
			if m.Type == "http.response.start" {
				return rejection.Serve(ctx, scope, receive, send)
			}
			return nil
		}
		return app(ctx, scope, limited, limitedSend)
	}
}

// Whether the app serving the scope has a surface policy serving the browser file routes
func uploadPolicyServesFiles(scope *Scope) bool {
	state := NewState(nil)
	if scope.App != nil {
		state = scope.App.State
	}
	return PolicyOf(state).BrowserFiles
}

// Whether every byte is an ASCII decimal digit, str.isdecimal on ASCII text
func allDecimal(s string) bool {
	for i := 0; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return false
		}
	}
	return s != ""
}
