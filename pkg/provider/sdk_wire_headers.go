package provider

import (
	"context"
	"crypto/tls"
	"net"
	"net/http"
	"net/url"
	"runtime"
	"strconv"
	"strings"
	"time"

	"github.com/nickheyer/icebath/pkg/internal/pyhttpx"
	"github.com/nickheyer/icebath/pkg/internal/pyplatform"
	"github.com/nickheyer/icebath/pkg/pm"
)

// Versions of the pinned openai SDK and httpx whose wire identity the model clients present
const (
	openAISDKVersion = "2.54.0"
	httpxVersion     = "0.28.1"
)

// Content codings the pinned httpx decodes on a default install, with neither brotli nor zstandard importable
var httpxDecoders = []string{"gzip", "deflate"}

// Default headers of an httpx.Client built with none of its own
func httpxClientHeaders() *pyhttpx.Headers {
	return pyhttpx.NewHeaders(
		pyhttpx.HeaderLine{Name: "Accept", Value: "*/*"},
		pyhttpx.HeaderLine{Name: "Accept-Encoding", Value: strings.Join(httpxDecoders, ", ")},
		pyhttpx.HeaderLine{Name: "Connection", Value: "keep-alive"},
		pyhttpx.HeaderLine{Name: "User-Agent", Value: "python-httpx/" + httpxVersion})
}

// Operating system label get_platform of a Stainless SDK reports for this host
func stainlessOS() string {
	switch runtime.GOOS {
	case "ios":
		return "iOS"
	case "darwin":
		return "MacOS"
	case "windows":
		return "Windows"
	case "android":
		return "Android"
	case "linux":
		return "Linux"
	}
	if label := strings.ToLower(pyplatform.Platform()); label != "" {
		return "Other:" + label
	}
	return "Unknown"
}

// Architecture label get_architecture of a Stainless SDK reports for this host
func stainlessArch() string {
	machine := strings.ToLower(pyplatform.Machine())
	switch {
	case machine == "arm64" || machine == "aarch64":
		return "arm64"
	case machine == "arm":
		return "arm"
	case machine == "x86_64":
		return "x64"
	case strconv.IntSize == 32:
		return "x32"
	case machine != "":
		return "other:" + machine
	}
	return "unknown"
}

// Patch release of the Python runtime the lock pins without its build tag, unknown when it pins none
func pinnedPythonRuntimeVersion() string {
	version, ok := pm.NewLockfile(pm.LockfilePath()).Version("python")
	if !ok || version == "" {
		return "unknown"
	}
	version, _, _ = strings.Cut(version, "+")
	return version
}

// X-Stainless platform headers of a Stainless SDK at the given version on the pinned CPython
func stainlessPlatformHeaders(version string) []pyhttpx.HeaderLine {
	return []pyhttpx.HeaderLine{
		{Name: "X-Stainless-Lang", Value: "python"},
		{Name: "X-Stainless-Package-Version", Value: version},
		{Name: "X-Stainless-OS", Value: stainlessOS()},
		{Name: "X-Stainless-Arch", Value: stainlessArch()},
		{Name: "X-Stainless-Runtime", Value: "CPython"},
		{Name: "X-Stainless-Runtime-Version", Value: pinnedPythonRuntimeVersion()},
	}
}

// Ordered str keyed dict with the case sensitive key identity of a Python dict, a removal standing for Omit
type sdkHeaderDict struct{ lines []pyhttpx.HeaderLine }

// Replaces the value of an existing key in place, else appends the key
func (d *sdkHeaderDict) set(name, value string) {
	for i, l := range d.lines {
		if l.Name == name {
			d.lines[i].Value = value
			return
		}
	}
	d.lines = append(d.lines, pyhttpx.HeaderLine{Name: name, Value: value})
}

// Removes the key the way an Omit value drops it from the merged mapping
func (d *sdkHeaderDict) omit(name string) {
	for i, l := range d.lines {
		if l.Name == name {
			d.lines = append(d.lines[:i], d.lines[i+1:]...)
			return
		}
	}
}

// Host header httpx writes, the netloc with the default port of the scheme dropped
func httpxHost(u *url.URL) string {
	if port := u.Port(); (port == "443" && u.Scheme == "https") || (port == "80" && u.Scheme == "http") {
		host := u.Hostname()
		if strings.Contains(host, ":") {
			return "[" + host + "]"
		}
		return host
	}
	return u.Host
}

// Points a transport at wire capable HTTP/1.1 connections routed by the environment proxy mounts httpx trust_env reads
func wireEnvDials(t *http.Transport, dial pyhttpx.DialFunc, cfg *tls.Config, connect time.Duration, proxies *pyhttpx.EnvProxies) {
	t.Proxy = func(req *http.Request) (*url.URL, error) {
		if req.URL.Scheme != "http" {
			return nil, nil
		}
		return pyhttpx.ForwardProxy(proxies.For(req.URL))
	}
	t.DialContext = func(ctx context.Context, network, address string) (net.Conn, error) {
		target, err := pyhttpx.RequestTarget(ctx)
		if err != nil {
			return nil, err
		}
		return pyhttpx.DialWire(ctx, dial, proxies.For(target), network, address)
	}
	t.DialTLSContext = func(ctx context.Context, network, address string) (net.Conn, error) {
		target, err := pyhttpx.RequestTarget(ctx)
		if err != nil {
			return nil, err
		}
		cctx, cancel := context.WithTimeout(ctx, connect)
		defer cancel()
		return pyhttpx.DialWireTLS(cctx, dial, proxies.For(target), network, address, cfg)
	}
}
