package selfupdate

import (
	"context"
	"net/http"
	"net/url"
	"os"
	"runtime"
	"strings"

	"golang.org/x/net/http/httpproxy"

	"github.com/nickheyer/icebath/pkg/internal/pyurl"
)

type sourceEnvironKey struct{}

// Context whose source check git children, HTTP requests and revision read see environ in place of the process environment
func WithSourceCheckEnviron(ctx context.Context, environ []string) context.Context {
	return context.WithValue(ctx, sourceEnvironKey{}, append([]string(nil), environ...))
}

// Environment a source check runs under, the one the context carries else the process environment
func checkEnviron(ctx context.Context) []string {
	if ctx != nil {
		if environ, ok := ctx.Value(sourceEnvironKey{}).([]string); ok {
			return environ
		}
	}
	return os.Environ()
}

// One variable of the source check environment, the last assignment winning and names folding case on Windows
func sourceGetenv(ctx context.Context, key string) string {
	environ := checkEnviron(ctx)
	for i := len(environ) - 1; i >= 0; i-- {
		name, value, ok := strings.Cut(environ[i], "=")
		if ok && (name == key || (runtime.GOOS == "windows" && strings.EqualFold(name, key))) {
			return value
		}
	}
	return ""
}

// Source check environment as a map, nil when the context carries none so callers read the process environment
func sourceEnvironMap(ctx context.Context) map[string]string {
	if ctx == nil {
		return nil
	}
	environ, ok := ctx.Value(sourceEnvironKey{}).([]string)
	if !ok {
		return nil
	}
	out := make(map[string]string, len(environ))
	for _, kv := range environ {
		if name, value, ok := strings.Cut(kv, "="); ok {
			out[name] = value
		}
	}
	return out
}

// Proxy choice of httpproxy.FromEnvironment read from the source check environment
func sourceProxyFunc(ctx context.Context) func(*url.URL) (*url.URL, error) {
	first := func(names ...string) string {
		for _, name := range names {
			if value := sourceGetenv(ctx, name); value != "" {
				return value
			}
		}
		return ""
	}
	return (&httpproxy.Config{
		HTTPProxy:  first("HTTP_PROXY", "http_proxy"),
		HTTPSProxy: first("HTTPS_PROXY", "https_proxy"),
		NoProxy:    first("NO_PROXY", "no_proxy"),
		CGI:        sourceGetenv(ctx, "REQUEST_METHOD") != "",
	}).ProxyFunc()
}

// Urlopen proxy choice of a request read from the source check environment its context carries
func sourceUrlopenProxy(req *http.Request) (*url.URL, error) {
	return pyurl.EnvironUrlopenProxy(checkEnviron(req.Context()), req)
}
