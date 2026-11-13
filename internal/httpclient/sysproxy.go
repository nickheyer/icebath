package httpclient

import (
	"context"
	"net"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"
)

// How long an operating system proxy answer is reused before the settings are read again
const systemProxyTTL = 60 * time.Second

// Bound on remembered answers, expired ones are dropped first when it is reached
const systemProxyEntries = 256

// Operating system proxy answers per target origin, refreshed after the TTL
type systemCache struct {
	mu      sync.Mutex
	entries map[string]systemAnswer
}

type systemAnswer struct {
	routes []*url.URL
	until  time.Time
}

// Routes the OS settings give a target origin, one proxy for every scheme when anyScheme is set
func (c *systemCache) lookup(ctx context.Context, target *url.URL, anyScheme bool) ([]*url.URL, error) {
	host, port := targetHostPort(target)
	hostPort := net.JoinHostPort(host, strconv.Itoa(port))
	scheme := "http"
	if s := strings.ToLower(target.Scheme); s == "https" || s == "wss" {
		scheme = "https"
	}
	key := scheme + "://" + hostPort
	if anyScheme {
		key += "#any"
	}
	now := time.Now()
	c.mu.Lock()
	if answer, ok := c.entries[key]; ok && now.Before(answer.until) {
		c.mu.Unlock()
		return answer.routes, nil
	}
	c.mu.Unlock()
	routes, err := systemRoutes(ctx, scheme, hostPort, anyScheme)
	if err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.entries == nil {
		c.entries = map[string]systemAnswer{}
	}
	if len(c.entries) >= systemProxyEntries {
		for k, answer := range c.entries {
			if !now.Before(answer.until) {
				delete(c.entries, k)
			}
		}
		if len(c.entries) >= systemProxyEntries {
			clear(c.entries)
		}
	}
	c.entries[key] = systemAnswer{routes: routes, until: now.Add(systemProxyTTL)}
	return routes, nil
}

// OS routes for the origin, or with anyScheme the first proxied answer for its https then http form
func systemRoutes(ctx context.Context, scheme, hostPort string, anyScheme bool) ([]*url.URL, error) {
	if !anyScheme {
		return osProxies(ctx, &url.URL{Scheme: scheme, Host: hostPort, Path: "/"})
	}
	for _, form := range []string{"https", "http"} {
		routes, err := osProxies(ctx, &url.URL{Scheme: form, Host: hostPort, Path: "/"})
		if err != nil || hasProxy(routes) {
			return routes, err
		}
	}
	return nil, nil
}

// Reports whether any route goes through a proxy
func hasProxy(routes []*url.URL) bool {
	for _, r := range routes {
		if r != nil {
			return true
		}
	}
	return false
}

// Proxy URL for a host and port the OS names, nil when either part is unusable
func osRoute(scheme, hostPort string) *url.URL {
	host, port, err := net.SplitHostPort(hostPort)
	if err != nil || host == "" {
		return nil
	}
	if n, ok := portNumber(port); !ok || n == 0 {
		return nil
	}
	return &url.URL{Scheme: scheme, Host: net.JoinHostPort(host, port)}
}
