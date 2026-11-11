package dashboard

import (
	"fmt"
	"maps"
	"math"
	"strconv"
	"strings"
)

// URL path a route name reverses to with the protocol and host that make it absolute
type URLPath struct {
	Path     string
	Protocol string
	Host     string
}

// Path with backslashes escaped and a leading double slash kept out of the authority
func newURLPath(path, protocol, host string) URLPath {
	path = strings.ReplaceAll(path, `\`, "%5C")
	if strings.HasPrefix(path, "//") {
		path = "/%2F" + path[2:]
	}
	return URLPath{Path: path, Protocol: protocol, Host: host}
}

// Absolute URL of the path below a base URL, its scheme following the base security for a protocol path
func (p URLPath) MakeAbsoluteURL(base URL) URL {
	scheme := base.Scheme()
	switch p.Protocol {
	case "http":
		scheme = map[bool]string{true: "https", false: "http"}[base.IsSecure()]
	case "websocket":
		scheme = map[bool]string{true: "wss", false: "ws"}[base.IsSecure()]
	}
	netloc := p.Host
	if netloc == "" {
		netloc = base.Netloc()
	}
	path := strings.TrimRight(base.Path(), "/") + p.Path
	return NewURL("").Replace(&scheme, &netloc, &path, nil)
}

// Raised when no route reverses a name with the given path parameters
type NoMatchFound struct {
	Name   string
	Params map[string]string
}

func (e *NoMatchFound) Error() string {
	keys := make([]string, 0, len(e.Params))
	for k := range e.Params {
		keys = append(keys, k)
	}
	return fmt.Sprintf(`No route exists for name "%s" and params "%s".`, e.Name, strings.Join(keys, ", "))
}

// Route that reverses its name into a URL path
type urlPathForRoute interface {
	urlPathFor(name string, params map[string]string) (URLPath, error)
}

// Path parameter value as its convertor writes it, an assertion error for a value the convertor refuses
func convertorToString(conv convertor, value string) (string, error) {
	switch conv.regex {
	case convertorTypes["str"].regex:
		if strings.Contains(value, "/") {
			return "", &RuntimeError{Msg: "May not contain path separators"}
		}
		if value == "" {
			return "", &RuntimeError{Msg: "Must not be empty"}
		}
	case convertorTypes["int"].regex:
		n, err := strconv.ParseInt(value, 10, 64)
		if err != nil {
			return "", err
		}
		if n < 0 {
			return "", &RuntimeError{Msg: "Negative integers are not supported"}
		}
		return strconv.FormatInt(n, 10), nil
	case convertorTypes["float"].regex:
		f, err := strconv.ParseFloat(value, 64)
		if err != nil {
			return "", err
		}
		switch {
		case !(f >= 0):
			return "", &RuntimeError{Msg: "Negative floats are not supported"}
		case math.IsInf(f, 0):
			return "", &RuntimeError{Msg: "Infinite values are not supported"}
		}
		text := strings.TrimRight(strings.TrimRight(strconv.FormatFloat(f, 'f', 20, 64), "0"), ".")
		return text, nil
	}
	return value, nil
}

// Path format with each parameter it names replaced, the parameters it does not name left over
func replaceParams(path string, convs map[string]convertor, params map[string]string) (string, map[string]string, error) {
	rest := maps.Clone(params)
	for key, value := range params {
		if !strings.Contains(path, "{"+key+"}") {
			continue
		}
		text, err := convertorToString(convs[key], value)
		if err != nil {
			return "", nil, err
		}
		path = strings.ReplaceAll(path, "{"+key+"}", text)
		delete(rest, key)
	}
	return path, rest, nil
}

// Reversed path of a named route whose parameters are exactly the given ones
func reverseRoute(routeName, path, protocol, name string, params map[string]string) (URLPath, error) {
	_, format, convs := compilePath(path)
	if name != routeName || len(params) != len(convs) {
		return URLPath{}, &NoMatchFound{Name: name, Params: params}
	}
	for k := range params {
		if _, ok := convs[k]; !ok {
			return URLPath{}, &NoMatchFound{Name: name, Params: params}
		}
	}
	out, _, err := replaceParams(format, convs, params)
	if err != nil {
		return URLPath{}, err
	}
	return newURLPath(out, protocol, ""), nil
}

func (r *APIRoute) urlPathFor(name string, params map[string]string) (URLPath, error) {
	return reverseRoute(r.Name, r.Path, "http", name, params)
}

func (r *APIWebSocketRoute) urlPathFor(name string, params map[string]string) (URLPath, error) {
	return reverseRoute(r.Name, r.Path, "websocket", name, params)
}

func (r *starletteRoute) urlPathFor(name string, params map[string]string) (URLPath, error) {
	return reverseRoute(r.name, r.path, "http", name, params)
}

// Mount path of its own name with the path parameter, a mounted callable holding no child routes
func (m *Mount) urlPathFor(name string, params map[string]string) (URLPath, error) {
	if m.Name != "" && name == m.Name {
		if p, ok := params["path"]; ok {
			given := maps.Clone(params)
			given["path"] = strings.TrimLeft(p, "/")
			_, format, convs := compilePath(m.Path + "/{path:path}")
			path, rest, err := replaceParams(format, convs, given)
			if err != nil {
				return URLPath{}, err
			}
			if len(rest) == 0 {
				return newURLPath(path, "", ""), nil
			}
		}
	}
	return URLPath{}, &NoMatchFound{Name: name, Params: params}
}

// Path of the first route reversing the name with the parameters
func (r *Router) URLPathFor(name string, params map[string]string) (URLPath, error) {
	for _, route := range r.Routes {
		rev, ok := route.(urlPathForRoute)
		if !ok {
			continue
		}
		out, err := rev.urlPathFor(name, params)
		if err == nil {
			return out, nil
		}
		if _, miss := err.(*NoMatchFound); !miss {
			return URLPath{}, err
		}
	}
	return URLPath{}, &NoMatchFound{Name: name, Params: params}
}

// Absolute URL of a named route below the connection base URL
func (c *HTTPConnection) URLFor(name string, params map[string]string) (URL, error) {
	router := c.Scope.Router
	if router == nil && c.Scope.App != nil {
		router = c.Scope.App.Router
	}
	if router == nil {
		return URL{}, &RuntimeError{Msg: "The `url_for` method can only be used inside a Starlette application or with a router."}
	}
	path, err := router.URLPathFor(name, params)
	if err != nil {
		return URL{}, err
	}
	return path.MakeAbsoluteURL(c.BaseURL()), nil
}
