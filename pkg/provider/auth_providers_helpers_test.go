package provider

import (
	"io"
	"net/http"
	"net/url"
	"testing"
	"time"
)

type authProvidersRT func(*http.Request) (*http.Response, error)

func (f authProvidersRT) RoundTrip(req *http.Request) (*http.Response, error) { return f(req) }

// Points a provider client seam at a scripted transport for the test
func scriptClient(t *testing.T, seam *func(time.Duration) *http.Client, fn func(*http.Request) (*http.Response, error)) {
	t.Helper()
	prev := *seam
	*seam = func(timeout time.Duration) *http.Client {
		return &http.Client{Timeout: timeout, Transport: authProvidersRT(fn)}
	}
	t.Cleanup(func() { *seam = prev })
}

func readForm(t *testing.T, req *http.Request) map[string]string {
	t.Helper()
	raw, _ := io.ReadAll(req.Body)
	values, err := url.ParseQuery(string(raw))
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]string{}
	for k := range values {
		out[k] = values.Get(k)
	}
	return out
}
