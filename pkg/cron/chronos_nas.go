package cron

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/provider"
)

const (
	nasProvisionPath = "/api/agent-cron/provision"
	nasCancelPath    = "/api/agent-cron/cancel"
	nasListPath      = "/api/agent-cron/list"
)

// NasCronClientError is a failed NAS agent-cron call with the HTTP status and error code when one came back
type NasCronClientError struct {
	Msg       string
	Status    *int
	ErrorCode string
}

func (e *NasCronClientError) Error() string { return e.Msg }

// True only for the 403 invalid_client NAS returns when the Nous credential is not this agent identity
func (e *NasCronClientError) IdentityRejected() bool {
	return e.Status != nil && *e.Status == 403 && e.ErrorCode == "invalid_client"
}

// The NAS agent-cron calls the chronos provider makes
type chronosClient interface {
	Provision(jobID, fireAt, agentCallbackURL, dedupKey string) (any, error)
	Cancel(jobID string) (any, error)
	ListArmed() ([]any, error)
}

// Thin JSON client over the Portal agent-cron endpoints with a refreshed Nous bearer per request
type nasCronClient struct {
	portalURL string
	timeout   time.Duration
	token     func() (string, error)
	http      *http.Client
}

func newNASCronClient(s *Store, portalURL string) *nasCronClient {
	env := provider.PoolEnv{Store: provider.NewStore(s.home), Secrets: config.NewSecrets(s.home), Config: provider.ConfigLoader(s.home), Log: s.log}
	return &nasCronClient{
		portalURL: strings.TrimRight(portalURL, "/"),
		timeout:   15 * time.Second,
		token:     func() (string, error) { return provider.ResolveNousAccessToken(env, provider.NousAccessTokenOptions{}) },
	}
}

func (c *nasCronClient) request(method, path string, body any, params url.Values) (any, error) {
	fail := func(err error) error {
		return &NasCronClientError{Msg: fmt.Sprintf("%s %s failed: %v", method, path, err)}
	}
	token, err := c.token()
	if err != nil {
		return nil, fail(err)
	}
	target := c.portalURL + path
	if len(params) > 0 {
		target += "?" + params.Encode()
	}
	var reader io.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			return nil, fail(err)
		}
		reader = bytes.NewReader(raw)
	}
	req, err := http.NewRequest(method, target, reader)
	if err != nil {
		return nil, fail(err)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	hc := c.http
	if hc == nil {
		hc = &http.Client{Timeout: c.timeout}
	}
	resp, err := hc.Do(req)
	if err != nil {
		return nil, fail(err)
	}
	defer resp.Body.Close()
	content, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fail(err)
	}
	if resp.StatusCode/100 != 2 {
		code := ""
		if parsed, err := pyjson.LoadsBytes(content); err == nil {
			if obj, ok := parsed.(*pyjson.Object); ok {
				if v, _ := obj.Get("error"); pyTruthy(v) {
					code = pyrepr.Str(v)
				}
			}
		}
		text := []rune(string(bytes.ToValidUTF8(content, []byte("�"))))
		if len(text) > 200 {
			text = text[:200]
		}
		status := resp.StatusCode
		return nil, &NasCronClientError{Msg: fmt.Sprintf("%s %s returned %d: %s", method, path, status, string(text)), Status: &status, ErrorCode: code}
	}
	if len(content) == 0 {
		return pyjson.NewObject(), nil
	}
	parsed, err := pyjson.LoadsBytes(content)
	if err != nil {
		return pyjson.NewObject(), nil
	}
	return parsed, nil
}

func (c *nasCronClient) Provision(jobID, fireAt, agentCallbackURL, dedupKey string) (any, error) {
	return c.request(http.MethodPost, nasProvisionPath, map[string]string{"job_id": jobID, "fire_at": fireAt, "agent_callback_url": agentCallbackURL, "dedup_key": dedupKey}, nil)
}

func (c *nasCronClient) Cancel(jobID string) (any, error) {
	return c.request(http.MethodPost, nasCancelPath, map[string]string{"job_id": jobID}, nil)
}

// Armed one shots NAS reports, an empty list when the reply carries no armed list
func (c *nasCronClient) ListArmed() ([]any, error) {
	data, err := c.request(http.MethodGet, nasListPath, nil, url.Values{})
	if err != nil {
		return nil, err
	}
	if obj, ok := data.(*pyjson.Object); ok {
		if items, isList := obj.Get("armed"); isList {
			if list, ok := items.([]any); ok {
				return list, nil
			}
		}
	}
	return []any{}, nil
}
