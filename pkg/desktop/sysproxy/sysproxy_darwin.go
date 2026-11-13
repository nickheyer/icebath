//go:build darwin

package sysproxy

import (
	"errors"
	"fmt"
	"net/url"
	"runtime"
	"sync"

	"github.com/ebitengine/purego"
)

const (
	cfStringEncodingUTF8 = 0x08000100
	cfNumberSInt32Type   = 3
	pacRunLoopSeconds    = 30.0
)

// CFNetwork and CoreFoundation entry points, the system proxy resolver WKWebView requests go through
var cf struct {
	once          sync.Once
	err           error
	stringCreate  func(alloc uintptr, cstr string, encoding uint32) uintptr
	stringGet     func(str uintptr, buf *byte, size int, encoding uint32) bool
	stringLength  func(str uintptr) int
	urlCreate     func(alloc, str, base uintptr) uintptr
	urlString     func(u uintptr) uintptr
	systemProxies func() uintptr
	proxiesForURL func(u, settings uintptr) uintptr
	arrayCount    func(array uintptr) int
	arrayAt       func(array uintptr, index int) uintptr
	dictGet       func(dict, key uintptr) uintptr
	numberGet     func(number uintptr, kind int, out *int32) bool
	equal         func(a, b uintptr) bool
	retain        func(ref uintptr) uintptr
	release       func(ref uintptr)
	runLoop       func() uintptr
	addSource     func(loop, source, mode uintptr)
	removeSource  func(loop, source, mode uintptr)
	runInMode     func(mode uintptr, seconds float64, returnAfterSource bool) int32
	stop          func(loop uintptr)
	execPACURL    func(pac, target, callback, context uintptr) uintptr
	execPACScript func(script, target, callback, context uintptr) uintptr
	callback      uintptr
}

// One PAC evaluation at a time, its answer handed back through the shared callback
var pac struct {
	mu     sync.Mutex
	result uintptr
	failed bool
	done   bool
}

func loadCF() error {
	cf.once.Do(func() {
		foundation, err := purego.Dlopen("/System/Library/Frameworks/CoreFoundation.framework/CoreFoundation", purego.RTLD_NOW|purego.RTLD_GLOBAL)
		if err != nil {
			cf.err = err
			return
		}
		network, err := purego.Dlopen("/System/Library/Frameworks/CFNetwork.framework/CFNetwork", purego.RTLD_NOW|purego.RTLD_GLOBAL)
		if err != nil {
			cf.err = err
			return
		}
		purego.RegisterLibFunc(&cf.stringCreate, foundation, "CFStringCreateWithCString")
		purego.RegisterLibFunc(&cf.stringGet, foundation, "CFStringGetCString")
		purego.RegisterLibFunc(&cf.stringLength, foundation, "CFStringGetLength")
		purego.RegisterLibFunc(&cf.urlCreate, foundation, "CFURLCreateWithString")
		purego.RegisterLibFunc(&cf.urlString, foundation, "CFURLGetString")
		purego.RegisterLibFunc(&cf.arrayCount, foundation, "CFArrayGetCount")
		purego.RegisterLibFunc(&cf.arrayAt, foundation, "CFArrayGetValueAtIndex")
		purego.RegisterLibFunc(&cf.dictGet, foundation, "CFDictionaryGetValue")
		purego.RegisterLibFunc(&cf.numberGet, foundation, "CFNumberGetValue")
		purego.RegisterLibFunc(&cf.equal, foundation, "CFEqual")
		purego.RegisterLibFunc(&cf.retain, foundation, "CFRetain")
		purego.RegisterLibFunc(&cf.release, foundation, "CFRelease")
		purego.RegisterLibFunc(&cf.runLoop, foundation, "CFRunLoopGetCurrent")
		purego.RegisterLibFunc(&cf.addSource, foundation, "CFRunLoopAddSource")
		purego.RegisterLibFunc(&cf.removeSource, foundation, "CFRunLoopRemoveSource")
		purego.RegisterLibFunc(&cf.runInMode, foundation, "CFRunLoopRunInMode")
		purego.RegisterLibFunc(&cf.stop, foundation, "CFRunLoopStop")
		purego.RegisterLibFunc(&cf.systemProxies, network, "CFNetworkCopySystemProxySettings")
		purego.RegisterLibFunc(&cf.proxiesForURL, network, "CFNetworkCopyProxiesForURL")
		purego.RegisterLibFunc(&cf.execPACURL, network, "CFNetworkExecuteProxyAutoConfigurationURL")
		purego.RegisterLibFunc(&cf.execPACScript, network, "CFNetworkExecuteProxyAutoConfigurationScript")
		cf.callback = purego.NewCallback(func(_ uintptr, proxies uintptr, failure uintptr) {
			if failure != 0 || proxies == 0 {
				pac.failed = true
			} else {
				pac.result = cf.retain(proxies)
			}
			pac.done = true
			cf.stop(cf.runLoop())
		})
	})
	return cf.err
}

// CFString of a Go string, owned by the caller
func cfString(text string) uintptr {
	return cf.stringCreate(0, text, cfStringEncodingUTF8)
}

// Go string of a CFString, empty for a null reference
func goString(ref uintptr) string {
	if ref == 0 {
		return ""
	}
	buf := make([]byte, cf.stringLength(ref)*4+1)
	if !cf.stringGet(ref, &buf[0], len(buf), cfStringEncodingUTF8) {
		return ""
	}
	for i, b := range buf {
		if b == 0 {
			return string(buf[:i])
		}
	}
	return string(buf)
}

// Value of a dictionary entry whose key is the CFNetwork constant named key
func dictValue(dict uintptr, key string) uintptr {
	name := cfString(key)
	defer cf.release(name)
	return cf.dictGet(dict, name)
}

// Whether a CFString equals the CFNetwork constant named want
func isConstant(ref uintptr, want string) bool {
	name := cfString(want)
	defer cf.release(name)
	return ref != 0 && cf.equal(ref, name)
}

// Runs one PAC evaluation on a locked run loop and answers the retained proxy list it produced
func executePAC(start func(callback uintptr) uintptr) (uintptr, error) {
	pac.mu.Lock()
	defer pac.mu.Unlock()
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	pac.result, pac.failed, pac.done = 0, false, false
	source := start(cf.callback)
	if source == 0 {
		return 0, errors.New("CFNetwork could not start the proxy auto configuration")
	}
	defer cf.release(source)
	mode := cfString("ICEBATHProxyAutoConfiguration")
	defer cf.release(mode)
	loop := cf.runLoop()
	cf.addSource(loop, source, mode)
	defer cf.removeSource(loop, source, mode)
	cf.runInMode(mode, pacRunLoopSeconds, false)
	if !pac.done {
		return 0, errors.New("proxy auto configuration timed out")
	}
	if pac.failed {
		return 0, errors.New("proxy auto configuration failed")
	}
	return pac.result, nil
}

// PAC entries of a CFNetwork proxy list, auto configuration entries evaluated against target
func proxyListEntries(list, target uintptr, depth int) ([]string, error) {
	var entries []string
	for i := 0; i < cf.arrayCount(list); i++ {
		proxy := cf.arrayAt(list, i)
		kind := dictValue(proxy, "kCFProxyTypeKey")
		host := goString(dictValue(proxy, "kCFProxyHostNameKey"))
		var port int32
		if number := dictValue(proxy, "kCFProxyPortNumberKey"); number != 0 {
			cf.numberGet(number, cfNumberSInt32Type, &port)
		}
		hostPort := fmt.Sprintf("%s:%d", host, port)
		switch {
		case isConstant(kind, "kCFProxyTypeNone"):
			entries = append(entries, "DIRECT")
		case isConstant(kind, "kCFProxyTypeHTTP"):
			entries = append(entries, "PROXY "+hostPort)
		case isConstant(kind, "kCFProxyTypeHTTPS"):
			entries = append(entries, "PROXY "+hostPort)
		case isConstant(kind, "kCFProxyTypeSOCKS"):
			entries = append(entries, "SOCKS5 "+hostPort)
		case depth == 0 && isConstant(kind, "kCFProxyTypeAutoConfigurationURL"):
			pacURL := dictValue(proxy, "kCFProxyAutoConfigurationURLKey")
			evaluated, err := executePAC(func(callback uintptr) uintptr { return cf.execPACURL(pacURL, target, callback, 0) })
			if err != nil {
				return nil, err
			}
			nested, err := proxyListEntries(evaluated, target, depth+1)
			cf.release(evaluated)
			if err != nil {
				return nil, err
			}
			entries = append(entries, nested...)
		case depth == 0 && isConstant(kind, "kCFProxyTypeAutoConfigurationJavaScript"):
			script := dictValue(proxy, "kCFProxyAutoConfigurationJavaScriptKey")
			evaluated, err := executePAC(func(callback uintptr) uintptr { return cf.execPACScript(script, target, callback, 0) })
			if err != nil {
				return nil, err
			}
			nested, err := proxyListEntries(evaluated, target, depth+1)
			cf.release(evaluated)
			if err != nil {
				return nil, err
			}
			entries = append(entries, nested...)
		}
	}
	return entries, nil
}

// Proxies CFNetwork answers for target under the system settings, PAC files evaluated as Safari does
func resolve(target *url.URL) (string, error) {
	if err := loadCF(); err != nil {
		return "", err
	}
	text := cfString(target.String())
	defer cf.release(text)
	ref := cf.urlCreate(0, text, 0)
	if ref == 0 {
		return "", errors.New("CFNetwork rejected the URL " + target.String())
	}
	defer cf.release(ref)
	settings := cf.systemProxies()
	if settings == 0 {
		return "", errors.New("CFNetwork has no system proxy settings")
	}
	defer cf.release(settings)
	list := cf.proxiesForURL(ref, settings)
	if list == 0 {
		return "", errors.New("CFNetwork answered no proxies")
	}
	defer cf.release(list)
	entries, err := proxyListEntries(list, ref, 0)
	if err != nil {
		return "", err
	}
	return pacAnswer(entries), nil
}
