package httpclient

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/url"
	"runtime"
	"strconv"
	"time"
)

// Longest a PAC script may take to answer before its entry is dropped
const pacTimeout = 30 * time.Second

// Proxies the system settings give a target, exceptions applied and PAC or WPAD answers evaluated
func osProxies(ctx context.Context, target *url.URL) ([]*url.URL, error) {
	b := loadCF()
	if b.err != nil {
		return nil, b.err
	}
	settings := b.systemProxySettings()
	if settings == 0 {
		return nil, errors.New("CFNetwork returned no system proxy settings")
	}
	defer b.release(settings)
	text := b.str(target.String())
	defer b.release(text)
	ref := b.urlCreate(0, text, 0)
	if ref == 0 {
		return nil, fmt.Errorf("CFNetwork rejected the URL %s://%s", target.Scheme, target.Host)
	}
	defer b.release(ref)
	list := b.proxiesForURL(ref, settings)
	if list == 0 {
		return nil, errors.New("CFNetwork returned no proxy list")
	}
	defer b.release(list)
	return b.proxyRoutes(ctx, list, ref, 0), nil
}

// Routes of a CFNetwork proxy list in order, auto configuration entries replaced by their answers
func (b *cfBinding) proxyRoutes(ctx context.Context, list, target uintptr, depth int) []*url.URL {
	var routes []*url.URL
	for i := 0; i < b.arrayCount(list); i++ {
		entry := b.arrayAt(list, i)
		host := b.goString(b.value(entry, "kCFProxyHostNameKey"))
		port, _ := b.number(entry, "kCFProxyPortNumberKey")
		hostPort := net.JoinHostPort(host, strconv.Itoa(port))
		switch b.goString(b.value(entry, "kCFProxyTypeKey")) {
		case "kCFProxyTypeNone":
			routes = append(routes, nil)
		case "kCFProxyTypeHTTP", "kCFProxyTypeHTTPS":
			if r := osRoute("http", hostPort); r != nil {
				routes = append(routes, r)
			}
		case "kCFProxyTypeSOCKS":
			if r := osRoute("socks5", hostPort); r != nil {
				routes = append(routes, r)
			}
		case "kCFProxyTypeAutoConfigurationURL":
			if depth > 0 {
				continue
			}
			pac := b.value(entry, "kCFProxyAutoConfigurationURLKey")
			if answer, ok := b.evaluatePAC(ctx, func(cb uintptr, c *[5]uintptr) uintptr { return b.runPACURL(pac, target, cb, c) }); ok {
				routes = append(routes, b.proxyRoutes(ctx, answer, target, depth+1)...)
				b.release(answer)
			}
		case "kCFProxyTypeAutoConfigurationJavaScript":
			if depth > 0 {
				continue
			}
			script := b.value(entry, "kCFProxyAutoConfigurationJavaScriptKey")
			if answer, ok := b.evaluatePAC(ctx, func(cb uintptr, c *[5]uintptr) uintptr { return b.runPACScript(script, target, cb, c) }); ok {
				routes = append(routes, b.proxyRoutes(ctx, answer, target, depth+1)...)
				b.release(answer)
			}
		}
	}
	return routes
}

// Runs one PAC evaluation on this thread's run loop and returns the owned proxy list it produced
func (b *cfBinding) evaluatePAC(ctx context.Context, start func(callback uintptr, clientContext *[5]uintptr) uintptr) (uintptr, bool) {
	pacRun.mu.Lock()
	defer pacRun.mu.Unlock()
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	pacRun.list, pacRun.failed, pacRun.done = 0, false, false
	var clientContext [5]uintptr
	source := start(b.pacCallback, &clientContext)
	if source == 0 {
		return 0, false
	}
	defer b.release(source)
	mode := b.str("IcebathProxyAutoConfiguration")
	defer b.release(mode)
	loop := b.runLoopCurrent()
	b.runLoopAdd(loop, source, mode)
	defer b.runLoopRemove(loop, source, mode)
	wait := pacTimeout
	if deadline, ok := ctx.Deadline(); ok && time.Until(deadline) < wait {
		wait = time.Until(deadline)
	}
	if wait <= 0 {
		return 0, false
	}
	stopped := make(chan struct{})
	stop := context.AfterFunc(ctx, func() {
		b.runLoopStop(loop)
		close(stopped)
	})
	b.runLoopRun(mode, wait.Seconds(), false)
	if !stop() {
		<-stopped
	}
	runtime.KeepAlive(&clientContext)
	if !pacRun.done || pacRun.failed || pacRun.list == 0 {
		return 0, false
	}
	return pacRun.list, true
}
