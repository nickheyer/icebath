package auth

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/nickheyer/icebath/internal/httpclient"
	"github.com/nickheyer/icebath/internal/model/wire"
)

const maxReplyBytes = 8 << 20

type reply struct {
	status int
	header http.Header
	body   []byte
}

func (r reply) object() (*doc, error) { return parseObject(r.body) }

func (r reply) value() (any, error) { return parseJSON(r.body) }

func (s *Store) exchange(ctx context.Context, client *http.Client, method, endpoint string, header map[string]string, body []byte, timeout time.Duration) (reply, error) {
	if client == nil {
		client = s.http
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	var reader io.Reader
	if body != nil {
		reader = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, endpoint, reader)
	if err != nil {
		return reply{}, err
	}
	for k, v := range header {
		req.Header.Set(k, v)
	}
	resp, err := client.Do(req)
	if err != nil {
		return reply{}, err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxReplyBytes))
	if err != nil {
		return reply{}, err
	}
	return reply{status: resp.StatusCode, header: resp.Header, body: data}, nil
}

func formOf(pairs ...string) *doc {
	d := newDoc()
	for i := 0; i+1 < len(pairs); i += 2 {
		d.set(pairs[i], pairs[i+1])
	}
	return d
}

func encodeForm(form *doc) string {
	var b strings.Builder
	for i, k := range form.keys {
		if i > 0 {
			b.WriteByte('&')
		}
		v, _ := form.vals[k].(string)
		b.WriteString(url.QueryEscape(k))
		b.WriteByte('=')
		b.WriteString(url.QueryEscape(v))
	}
	return b.String()
}

func (s *Store) postForm(ctx context.Context, client *http.Client, endpoint string, form *doc, header map[string]string, timeout time.Duration) (reply, error) {
	merged := map[string]string{"Content-Type": "application/x-www-form-urlencoded", "Accept": "application/json"}
	for k, v := range header {
		merged[k] = v
	}
	return s.exchange(ctx, client, http.MethodPost, endpoint, merged, []byte(encodeForm(form)), timeout)
}

func (s *Store) postJSON(ctx context.Context, client *http.Client, endpoint string, payload *doc, header map[string]string, timeout time.Duration) (reply, error) {
	raw, err := payload.MarshalJSON()
	if err != nil {
		return reply{}, err
	}
	merged := map[string]string{"Content-Type": "application/json", "Accept": "application/json"}
	for k, v := range header {
		merged[k] = v
	}
	return s.exchange(ctx, client, http.MethodPost, endpoint, merged, raw, timeout)
}

func (s *Store) getReply(ctx context.Context, client *http.Client, endpoint string, header map[string]string, timeout time.Duration) (reply, error) {
	return s.exchange(ctx, client, http.MethodGet, endpoint, header, nil, timeout)
}

func wireMask(text string) string { return wire.MaskCredentials(text) }

func transportFailure(err error) string {
	if err == nil {
		return ""
	}
	if IsNetworkError(err) {
		return httpclient.Message(err)
	}
	return fmt.Sprintf("%T", err)
}

type TLSState struct {
	Insecure bool
	CABundle string
}

func tlsStateOf(state *doc) TLSState {
	tls := state.obj("tls")
	out := TLSState{CABundle: tls.str("ca_bundle")}
	switch v := tls.value("insecure").(type) {
	case bool:
		out.Insecure = v
	case string:
		switch strings.ToLower(strings.TrimSpace(v)) {
		case "1", "true", "yes", "on":
			out.Insecure = true
		}
	default:
		out.Insecure = truthy(v)
	}
	return out
}

func (s *Store) VerifiedClient(ctx context.Context, insecure *bool, caBundle string, stored TLSState) (*http.Client, TLSState, error) {
	effective := TLSState{Insecure: stored.Insecure}
	if insecure != nil {
		effective.Insecure = *insecure
	}
	effective.CABundle = caBundle
	if effective.CABundle == "" {
		effective.CABundle = stored.CABundle
	}
	for _, name := range []string{"ICEBATH_CA_BUNDLE", "SSL_CERT_FILE", "REQUESTS_CA_BUNDLE"} {
		if effective.CABundle != "" {
			break
		}
		effective.CABundle = s.lookupEnv(ctx, name)
	}
	resolved := TLSState{Insecure: effective.Insecure}
	if effective.Insecure {
		t, err := httpclient.New(httpclient.Options{TLS: httpclient.TLSOptions{Insecure: true}})
		if err != nil {
			return nil, resolved, err
		}
		return t.Client(), resolved, nil
	}
	if effective.CABundle != "" {
		if fi, err := os.Stat(effective.CABundle); err != nil || !fi.Mode().IsRegular() {
			slog.Warn(fmt.Sprintf("CA bundle path does not exist: %s — falling back to default certificates", effective.CABundle))
			return s.http, resolved, nil
		}
		t, err := httpclient.New(httpclient.Options{TLS: httpclient.TLSOptions{RootCAFile: effective.CABundle}})
		if err != nil {
			return nil, resolved, err
		}
		return t.Client(), resolved, nil
	}
	return s.http, resolved, nil
}
