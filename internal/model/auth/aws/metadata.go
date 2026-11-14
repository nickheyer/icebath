package aws

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"math/rand/v2"
	"net"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/nickheyer/icebath/internal/httpclient"
)

const (
	containerTimeout  = 2 * time.Second
	containerAttempts = 3
	containerPause    = time.Second
	containerHost     = "169.254.170.2"
	imdsTokenTTL      = "21600"
	imdsRolePath      = "latest/meta-data/iam/security-credentials/"
)

var containerAllowedHosts = []string{containerHost, "169.254.170.23", "fd00:ec2::23", "localhost"}

type metadataKeys struct {
	AccessKeyID     *string `json:"AccessKeyId"`
	SecretAccessKey *string `json:"SecretAccessKey"`
	Token           *string `json:"Token"`
	Expiration      *string `json:"Expiration"`
}

func (r *resolver) container(ctx context.Context) (provider, error) {
	const source = "container-role"
	relative, hasRelative := os.LookupEnv("AWS_CONTAINER_CREDENTIALS_RELATIVE_URI")
	full, hasFull := os.LookupEnv("AWS_CONTAINER_CREDENTIALS_FULL_URI")
	if !hasRelative && !hasFull {
		return nil, nil
	}
	target := full
	if hasRelative {
		target = "http://" + containerHost + relative
	} else {
		u, err := url.Parse(full)
		if err != nil {
			return nil, &Error{Source: source, Message: "AWS_CONTAINER_CREDENTIALS_FULL_URI is not a URL"}
		}
		host := u.Hostname()
		allowed := false
		if ip := net.ParseIP(host); ip != nil && ip.IsLoopback() {
			allowed = true
		}
		for _, h := range containerAllowedHosts {
			if h == host {
				allowed = true
			}
		}
		if !allowed {
			return nil, &Error{Source: source, Message: fmt.Sprintf("unsupported host %q, container metadata is read only from a loopback address or one of %s", host, strings.Join(containerAllowedHosts, ", "))}
		}
	}
	client := r.c.direct
	fetch := func(ctx context.Context) (Credentials, error) {
		token, hasToken := "", false
		if path, ok := os.LookupEnv("AWS_CONTAINER_AUTHORIZATION_TOKEN_FILE"); ok {
			data, err := readBounded(path)
			if err != nil {
				return Credentials{}, &Error{Source: source, Message: "unable to read the container authorization token file", Err: err}
			}
			token, hasToken = string(data), true
		} else if v, ok := os.LookupEnv("AWS_CONTAINER_AUTHORIZATION_TOKEN"); ok {
			token, hasToken = v, true
		}
		if hasToken && strings.ContainsAny(token, "\r\n") {
			return Credentials{}, &Error{Source: source, Message: "the container authorization token is not a legal header value"}
		}
		var last error
		for attempt := 0; attempt < containerAttempts; attempt++ {
			if attempt > 0 {
				select {
				case <-ctx.Done():
					return Credentials{}, &Error{Source: source, Message: "cancelled while reading container metadata", Err: ctx.Err()}
				case <-time.After(containerPause):
				}
			}
			creds, err := containerFetch(ctx, client, target, token, hasToken)
			if err == nil {
				return creds, nil
			}
			last = err
		}
		return Credentials{}, &Error{Source: source, Message: last.Error(), Err: last}
	}
	creds, err := fetch(ctx)
	if err != nil {
		return nil, err
	}
	return newRefreshing(fetch, &creds), nil
}

func containerFetch(ctx context.Context, client *http.Client, target, token string, hasToken bool) (Credentials, error) {
	const source = "container-role"
	ctx, cancel := context.WithTimeout(ctx, 3*containerTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
	if err != nil {
		return Credentials{}, errors.New("the container metadata address is not a usable URL")
	}
	req.Header.Set("Accept", "application/json")
	if hasToken {
		req.Header.Set("Authorization", token)
	}
	status, _, data, err := send(client, req, source)
	if err != nil {
		return Credentials{}, fmt.Errorf("error when retrieving container metadata: %w", err)
	}
	if status != http.StatusOK {
		return Credentials{}, fmt.Errorf("received non 200 response %d from container metadata", status)
	}
	var keys metadataKeys
	if err := json.Unmarshal(data, &keys); err != nil {
		return Credentials{}, errors.New("unable to parse JSON returned from container metadata services")
	}
	if keys.AccessKeyID == nil || keys.SecretAccessKey == nil || keys.Token == nil || keys.Expiration == nil {
		return Credentials{}, errors.New("container metadata carried no complete credentials")
	}
	expires, err := parseTime(*keys.Expiration)
	if err != nil {
		return Credentials{}, errors.New("container metadata carried an unreadable expiration")
	}
	return Credentials{AccessKeyID: *keys.AccessKeyID, SecretAccessKey: *keys.SecretAccessKey, SessionToken: *keys.Token, Expires: expires, Source: source}, nil
}

type imds struct {
	client     *http.Client
	endpoint   string
	timeout    time.Duration
	attempts   int
	v1Disabled bool
}

func (r *resolver) imdsConfig() (*imds, bool, error) {
	if r.c.opts.NoInstanceMetadata || strings.EqualFold(os.Getenv("AWS_EC2_METADATA_DISABLED"), "true") {
		return nil, false, nil
	}
	m := &imds{endpoint: "http://169.254.169.254/", timeout: time.Second, attempts: 1}
	if v := r.s.value("AWS_EC2_METADATA_SERVICE_ENDPOINT", "ec2_metadata_service_endpoint"); v != "" {
		m.endpoint = strings.TrimRight(v, "/") + "/"
	} else if strings.EqualFold(r.s.value("AWS_EC2_METADATA_SERVICE_ENDPOINT_MODE", "ec2_metadata_service_endpoint_mode"), "ipv6") || isTrue(r.s.value("AWS_IMDS_USE_IPV6", "imds_use_ipv6")) {
		m.endpoint = "http://[fd00:ec2::254]/"
	}
	if v := r.s.value("AWS_METADATA_SERVICE_TIMEOUT", "metadata_service_timeout"); v != "" {
		if f, err := strconv.ParseFloat(strings.TrimSpace(v), 64); err == nil && f > 0 {
			m.timeout = time.Duration(f * float64(time.Second))
		}
	}
	if v := r.s.value("AWS_METADATA_SERVICE_NUM_ATTEMPTS", "metadata_service_num_attempts"); v != "" {
		if n, err := strconv.Atoi(strings.TrimSpace(v)); err == nil && n > 0 {
			m.attempts = n
		}
	}
	m.v1Disabled = isTrue(r.s.value("AWS_EC2_METADATA_V1_DISABLED", "ec2_metadata_v1_disabled"))
	opts := r.c.opts.HTTP
	opts.Timeouts = httpclient.EachPhase(m.timeout)
	transport, err := httpclient.New(opts)
	if err != nil {
		return nil, false, err
	}
	m.client = transport.Client()
	return m, true, nil
}

func (r *resolver) instanceMetadata(ctx context.Context) (provider, error) {
	m, enabled, err := r.imdsConfig()
	if err != nil {
		return nil, err
	}
	if !enabled {
		return nil, nil
	}
	creds, ok := m.fetch(ctx)
	if !ok {
		return nil, nil
	}
	refresh := func(ctx context.Context) (Credentials, error) {
		fresh, ok := m.fetch(ctx)
		if !ok {
			return Credentials{}, &Error{Source: "iam-role", Message: "credential refresh failed, instance metadata returned no credentials", Err: errIncomplete}
		}
		return fresh, nil
	}
	return newRefreshing(refresh, &creds), nil
}

func (m *imds) token(ctx context.Context) (string, bool) {
	for i := 0; i < m.attempts; i++ {
		rctx, cancel := context.WithTimeout(ctx, 3*m.timeout)
		req, err := http.NewRequestWithContext(rctx, http.MethodPut, m.endpoint+"latest/api/token", nil)
		if err != nil {
			cancel()
			return "", false
		}
		req.Header.Set("x-aws-ec2-metadata-token-ttl-seconds", imdsTokenTTL)
		status, _, data, err := send(m.client, req, "iam-role")
		cancel()
		if err != nil {
			var phase *httpclient.Error
			if errors.As(err, &phase) && phase.Timeout() && phase.Phase >= httpclient.PhaseHeader {
				return "", true
			}
			continue
		}
		switch status {
		case http.StatusOK:
			return string(data), true
		case http.StatusNotFound, http.StatusForbidden, http.StatusMethodNotAllowed:
			return "", true
		case http.StatusBadRequest:
			return "", false
		}
	}
	return "", true
}

func (m *imds) get(ctx context.Context, path, token string, usable func(data []byte) bool) ([]byte, bool) {
	for i := 0; i < m.attempts; i++ {
		rctx, cancel := context.WithTimeout(ctx, 3*m.timeout)
		req, err := http.NewRequestWithContext(rctx, http.MethodGet, m.endpoint+path, nil)
		if err != nil {
			cancel()
			return nil, false
		}
		if token != "" {
			req.Header.Set("x-aws-ec2-metadata-token", token)
		}
		status, _, data, err := send(m.client, req, "iam-role")
		cancel()
		if err != nil {
			continue
		}
		if status == http.StatusOK && len(data) > 0 && usable(data) {
			return data, true
		}
	}
	return nil, false
}

func (m *imds) fetch(ctx context.Context) (Credentials, bool) {
	token, proceed := m.token(ctx)
	if !proceed || (token == "" && m.v1Disabled) {
		return Credentials{}, false
	}
	roleData, ok := m.get(ctx, imdsRolePath, token, func([]byte) bool { return true })
	if !ok {
		return Credentials{}, false
	}
	role := strings.TrimSpace(string(roleData))
	data, ok := m.get(ctx, imdsRolePath+role, token, func(data []byte) bool { return json.Valid(data) })
	if !ok {
		return Credentials{}, false
	}
	var keys metadataKeys
	if err := json.Unmarshal(data, &keys); err != nil || keys.AccessKeyID == nil || keys.SecretAccessKey == nil || keys.Token == nil || keys.Expiration == nil {
		return Credentials{}, false
	}
	creds := Credentials{AccessKeyID: *keys.AccessKeyID, SecretAccessKey: *keys.SecretAccessKey, SessionToken: *keys.Token, Source: "iam-role"}
	expires, err := time.Parse("2006-01-02T15:04:05Z", *keys.Expiration)
	if err != nil {
		expires, err = parseTime(*keys.Expiration)
		if err != nil {
			return Credentials{}, false
		}
		creds.Expires = expires
		return creds, true
	}
	offset := 10*time.Minute + time.Duration(120+rand.IntN(481))*time.Second
	now := time.Now().UTC()
	if !now.Before(expires.Add(-offset)) {
		slog.Info("extending instance credential expiration, the credential service returned credentials near expiry", "retry_within", offset.String())
		expires = now.Add(offset).Truncate(time.Second)
	}
	creds.Expires = expires
	return creds, true
}
