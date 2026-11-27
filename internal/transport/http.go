package transport

import (
	"crypto/tls"
	"crypto/x509"
	"log/slog"
	"net"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/spf13/cast"

	"github.com/nickheyer/icebath/internal/config"
	providerconfig "github.com/nickheyer/icebath/internal/provider/config"
	transportconfig "github.com/nickheyer/icebath/internal/transport/config"
)

// HTTP transports of a wire, one for each connect limit and certificate setting of the providers it reaches, so connections are reused
type Pool struct {
	transports sync.Map
}

// Certificate settings of a provider's connections
type trust struct {
	connect  time.Duration
	ca       string
	insecure bool
}

// HTTP client of a provider: its dial and handshake bounded by the connect limit, its server checked as the provider's settings say, and
// redirects left to the caller when follow is false
func (p *Pool) Client(settings config.View, connect time.Duration, follow bool) *http.Client {
	key := trust{connect: connect, ca: strings.TrimSpace(settings.GetString(providerconfig.CACert)),
		insecure: settings.Exists() && !settings.GetBool(providerconfig.VerifyTLS)}
	pooled, ok := p.transports.Load(key)
	if !ok {
		pooled, _ = p.transports.LoadOrStore(key, &http.Transport{
			Proxy:                 http.ProxyFromEnvironment,
			DialContext:           (&net.Dialer{Timeout: connect, KeepAlive: 30 * time.Second}).DialContext,
			TLSClientConfig:       tlsConfig(key),
			TLSHandshakeTimeout:   connect,
			ForceAttemptHTTP2:     true,
			MaxIdleConns:          100,
			IdleConnTimeout:       90 * time.Second,
			ExpectContinueTimeout: time.Second,
		})
	}
	client := &http.Client{Transport: pooled.(*http.Transport)}
	if !follow {
		client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	}
	return client
}

// TLS settings of a provider: the system's authorities with its own added, or no check at all
func tlsConfig(t trust) *tls.Config {
	config := &tls.Config{MinVersion: tls.VersionTLS12, InsecureSkipVerify: t.insecure}
	if t.ca == "" {
		return config
	}
	pool, err := x509.SystemCertPool()
	if err != nil || pool == nil {
		pool = x509.NewCertPool()
	}
	raw, err := os.ReadFile(t.ca)
	if err != nil || !pool.AppendCertsFromPEM(raw) {
		slog.Warn("A provider's CA certificate file could not be used; its server is checked against the system's authorities", "file", t.ca,
			"error", err)
		return config
	}
	config.RootCAs = pool
	return config
}

// Sets the headers every request to the provider carries over those of the request: the extra headers of the transports, for the OpenAI
// APIs that take them, then the provider's own, and the session id under the provider's affinity header
func Extra(h http.Header, cfg *config.Config, settings config.View, session string, openai bool) {
	tables := []any{settings.Get(providerconfig.ExtraHeaders)}
	if openai {
		tables = append([]any{cfg.Get(transportconfig.DefaultHeaders)}, tables...)
	}
	for _, table := range tables {
		if headers, ok := table.(map[string]any); ok {
			for name, value := range headers {
				if value != nil {
					h.Set(name, cast.ToString(value))
				}
			}
		}
	}
	if name := strings.TrimSpace(settings.GetString(providerconfig.AffinityHeader)); name != "" && session != "" {
		h.Set(name, session)
	}
}
