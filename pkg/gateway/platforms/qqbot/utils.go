package qqbot

import (
	"cmp"
	"fmt"
	"runtime"
	"strings"

	"github.com/nickheyer/icebath/pkg/gateway"
	"github.com/nickheyer/icebath/pkg/internal/pyplatform"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
	"github.com/nickheyer/icebath/pkg/selfupdate/updaterecord"
)

// Stamped icebath version, dev for an unstamped build
func icebathVersion() string {
	if v := updaterecord.BuildVersion(); v != "" {
		return v
	}
	return "dev"
}

// BuildUserAgent names the adapter version, the runtime, the OS and the icebath version
func BuildUserAgent() string {
	return fmt.Sprintf("QQBotAdapter/%s (Go/%s; %s; Icebath/%s)", QQBotVersion,
		strings.TrimPrefix(runtime.Version(), "go"), strings.ToLower(pyplatform.System()), icebathVersion())
}

// Kind of a connect or reconnect failure and the proxy's host when a proxy URL holds a secret the raw error text could repeat, empty otherwise: the
// token and gateway URL client reads the proxy environment itself while the websocket dials the proxy given
func describeConnectProxyError(err error, websocketProxy string) string {
	return cmp.Or(gateway.DescribeProxyError(err, gateway.EnvProxyForRedaction(TokenURL, APIBase)), gateway.DescribeProxyError(err, websocketProxy))
}

// Kind of a failed standalone request and the proxy's host when the proxy URL its client reads from the environment holds a secret the raw error text
// could repeat, empty otherwise
func describeSendProxyError(err error) string {
	return gateway.DescribeProxyError(err, gateway.EnvProxyForRedaction("https://bots.qq.com", "https://api.sgroup.qq.com"))
}

// GetAPIHeaders returns the JSON content headers q.qq.com needs to skip its anti bot page
func GetAPIHeaders() map[string]string {
	return map[string]string{"Content-Type": "application/json", "Accept": "application/json", "User-Agent": BuildUserAgent()}
}

// CoerceList turns a comma separated string, a list or a scalar into trimmed non empty strings
func CoerceList(value any) []string {
	var items []any
	switch v := value.(type) {
	case nil:
		return []string{}
	case string:
		for _, s := range strings.Split(v, ",") {
			items = append(items, s)
		}
	case []any:
		items = v
	case []string:
		for _, s := range v {
			items = append(items, s)
		}
	default:
		items = []any{v}
	}
	out := []string{}
	for _, item := range items {
		if s := pystr.Strip(pyrepr.Str(item)); s != "" {
			out = append(out, s)
		}
	}
	return out
}
