package dashboard

import (
	"context"
	"regexp"
	"strconv"
	"strings"
	"sync"

	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
)

// Python ClientDisconnect raised when the peer leaves while the body is read
type ClientDisconnect struct{}

func (ClientDisconnect) Error() string      { return "" }
func (ClientDisconnect) PyTypeName() string { return "ClientDisconnect" }

// Connection facts shared by requests and websockets, the Starlette HTTPConnection
type HTTPConnection struct {
	Scope   *Scope
	receive Receive
	send    Send
	stateMu sync.Mutex
	state   *State
}

// Application serving the connection
func (c *HTTPConnection) App() *FastAPI { return c.Scope.App }

// Full URL the connection addresses
func (c *HTTPConnection) URL() URL { return URLOfScope(c.Scope) }

// Root URL of the application with a trailing slash and no query
func (c *HTTPConnection) BaseURL() URL {
	s := c.Scope.Clone()
	appRoot, ok := s.Extra["app_root_path"].(string)
	if !ok {
		appRoot = s.RootPath
	}
	path := appRoot
	if !strings.HasSuffix(path, "/") {
		path += "/"
	}
	s.Path, s.QueryString, s.RootPath = path, "", appRoot
	return URLOfScope(s)
}

// Request headers backed by the scope
func (c *HTTPConnection) Headers() Headers { return headersOfScope(c.Scope) }

// Parsed query string
func (c *HTTPConnection) QueryParams() QueryParams { return ParseQueryParams(c.Scope.QueryString) }

// Converted path parameters of the matched route
func (c *HTTPConnection) PathParams() map[string]any {
	if c.Scope.PathParams == nil {
		return map[string]any{}
	}
	return c.Scope.PathParams
}

// Cookies of the cookie header, later duplicates winning
func (c *HTTPConnection) Cookies() map[string]string {
	if v := c.Headers().Get("cookie"); v != "" {
		return cookieParser(v)
	}
	return map[string]string{}
}

// Peer address, nil when the server reports none
func (c *HTTPConnection) Client() *Address { return c.Scope.Client }

// Per connection attribute bag backed by the scope state
func (c *HTTPConnection) State() *State {
	c.stateMu.Lock()
	defer c.stateMu.Unlock()
	if c.state == nil {
		if c.Scope.State == nil {
			c.Scope.State = map[string]any{}
		}
		c.state = NewState(c.Scope.State)
	}
	return c.state
}

var cookieUnquoteRE = regexp.MustCompile(`\\(?:([0-3][0-7][0-7])|(.))`)

// Cookie mapping of a cookie header split on semicolons, an unnamed chunk keyed by the empty name
func cookieParser(cookieString string) map[string]string {
	out := map[string]string{}
	for _, chunk := range strings.Split(cookieString, ";") {
		key, val, ok := strings.Cut(chunk, "=")
		if !ok {
			key, val = "", chunk
		}
		key, val = pystr.Strip(key), pystr.Strip(val)
		if key != "" || val != "" {
			out[key] = cookieUnquote(val)
		}
	}
	return out
}

// Value of a quoted cookie with its octal and backslash escapes undone, other values unchanged
func cookieUnquote(s string) string {
	if pystr.Len(s) < 2 || s[0] != '"' || s[len(s)-1] != '"' {
		return s
	}
	s = s[1 : len(s)-1]
	return cookieUnquoteRE.ReplaceAllStringFunc(s, func(m string) string {
		sub := cookieUnquoteRE.FindStringSubmatch(m)
		if sub[1] != "" {
			n, _ := strconv.ParseInt(sub[1], 8, 32)
			return string(rune(n))
		}
		return sub[2]
	})
}

// HTTP request with its body stream, the Starlette Request
type Request struct {
	HTTPConnection
	body           []byte
	haveBody       bool
	json           any
	haveJSON       bool
	streamConsumed bool
	disconnected   bool
	form           *FormData
}

// Request over an http scope and its receive and send callables
func NewRequest(scope *Scope, receive Receive, send Send) *Request {
	if receive == nil {
		receive = func(context.Context) (Message, error) {
			return Message{}, &RuntimeError{Msg: "Receive channel has not been made available"}
		}
	}
	return &Request{HTTPConnection: HTTPConnection{Scope: scope, receive: receive, send: send}}
}

// Request method
func (r *Request) Method() string { return r.Scope.Method }

// Receive callable of the request
func (r *Request) Receive() Receive { return r.receive }

// Body chunks as they arrive, the cached body when already read
func (r *Request) Stream(ctx context.Context, yield func([]byte) error) error {
	if r.haveBody {
		if err := yield(r.body); err != nil {
			return err
		}
		return yield([]byte{})
	}
	if r.streamConsumed {
		return &RuntimeError{Msg: "Stream consumed"}
	}
	for !r.streamConsumed {
		m, err := r.receive(ctx)
		if err != nil {
			return err
		}
		switch m.Type {
		case "http.request":
			if !m.MoreBody {
				r.streamConsumed = true
			}
			if len(m.Body) > 0 {
				if err := yield(m.Body); err != nil {
					return err
				}
			}
		case "http.disconnect":
			r.disconnected = true
			return ClientDisconnect{}
		}
	}
	return yield([]byte{})
}

// Whole body, read once and cached
func (r *Request) Body(ctx context.Context) ([]byte, error) {
	if !r.haveBody {
		var chunks []byte
		if err := r.Stream(ctx, func(b []byte) error { chunks = append(chunks, b...); return nil }); err != nil {
			return nil, err
		}
		if chunks == nil {
			chunks = []byte{}
		}
		r.body, r.haveBody = chunks, true
	}
	return r.body, nil
}

// Body decoded with json.loads, cached after the first call
func (r *Request) JSON(ctx context.Context) (any, error) {
	if !r.haveJSON {
		body, err := r.Body(ctx)
		if err != nil {
			return nil, err
		}
		v, err := pyjson.LoadsBytes(body)
		if err != nil {
			return nil, err
		}
		r.json, r.haveJSON = v, true
	}
	return r.json, nil
}

// Whether the client has gone, consuming an event that is ready without waiting for one
func (r *Request) IsDisconnected(ctx context.Context) bool {
	if !r.disconnected {
		cctx, cancel := context.WithCancel(ctx)
		cancel()
		if m, err := r.receive(cctx); err == nil && m.Type == "http.disconnect" {
			r.disconnected = true
		}
	}
	return r.disconnected
}
