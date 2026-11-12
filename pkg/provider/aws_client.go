package provider

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"math/rand"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Botocore connect and read defaults
const (
	awsDefaultConnectTimeout = 60 * time.Second
	awsDefaultReadTimeout    = 60 * time.Second
)

// Signed client for one AWS service and region with botocore timeouts and retry policy
type awsServiceClient struct {
	service     string
	signingName string
	region      string
	endpoint    string
	creds       awsCredentialProvider
	bearer      string
	http        *http.Client
	readTimeout time.Duration
	retry       *awsRetryPolicy
}

// Settings one service client is built from
type awsClientConfig struct {
	service          string
	signingName      string
	region           string
	session          awsSessionOptions
	bearer           string
	readTimeout      time.Duration
	totalMaxAttempts int
}

// Endpoint prefix of each botocore client name
var awsEndpointPrefix = map[string]string{"bedrock-runtime": "bedrock-runtime", "bedrock": "bedrock", "sts": "sts", "sso": "portal.sso", "sso-oidc": "oidc"}

// Default regional endpoint honouring the FIPS and dualstack switches
func (s *awsSession) serviceEndpoint(service, region string) string {
	if ep := s.configuredEndpoint(service); ep != "" {
		return strings.TrimRight(ep, "/")
	}
	prefix := awsEndpointPrefix[service]
	if prefix == "" {
		prefix = service
	}
	fips := awsEnsureBool(s.setting("AWS_USE_FIPS_ENDPOINT", "use_fips_endpoint"))
	dual := awsEnsureBool(s.setting("AWS_USE_DUALSTACK_ENDPOINT", "use_dualstack_endpoint"))
	if fips {
		prefix += "-fips"
	}
	suffix := awsPartitionDNSSuffix(region)
	if dual {
		suffix = "api.aws"
		if strings.HasPrefix(region, "cn-") {
			suffix = "api.amazonwebservices.com.cn"
		}
	}
	return "https://" + prefix + "." + region + "." + suffix
}

// Builds a client with its own pooled transport, connect bounded at 60 seconds and reads at the read timeout
func newAWSServiceClient(cfg awsClientConfig) *awsServiceClient {
	s := newAWSSession(cfg.session)
	read := cfg.readTimeout
	if read <= 0 {
		read = awsDefaultReadTimeout
	}
	tr := &http.Transport{
		Proxy:                 http.ProxyFromEnvironment,
		DialContext:           (&net.Dialer{Timeout: awsDefaultConnectTimeout, KeepAlive: 30 * time.Second}).DialContext,
		TLSClientConfig:       PlatformTLSConfig(),
		TLSHandshakeTimeout:   awsDefaultConnectTimeout,
		ResponseHeaderTimeout: read,
		MaxIdleConnsPerHost:   10,
		IdleConnTimeout:       90 * time.Second,
		ForceAttemptHTTP2:     true,
	}
	signing := cfg.signingName
	if signing == "" {
		signing = cfg.service
	}
	return &awsServiceClient{
		service: cfg.service, signingName: signing, region: cfg.region, endpoint: s.serviceEndpoint(cfg.service, cfg.region),
		creds: &awsChainCredentials{session: s}, bearer: cfg.bearer, http: &http.Client{Transport: tr}, readTimeout: read,
		retry: newAWSRetryPolicy(s, cfg.totalMaxAttempts),
	}
}

// Closes pooled idle connections of the client
func (c *awsServiceClient) closeIdle() {
	if tr, ok := c.http.Transport.(*http.Transport); ok {
		tr.CloseIdleConnections()
	}
}

// AWSReadTimeoutError is a socket read that waited longer than the client read timeout
type AWSReadTimeoutError struct {
	Endpoint string
	Limit    time.Duration
}

func (e *AWSReadTimeoutError) Error() string {
	return fmt.Sprintf("Read timeout on endpoint URL: %q", e.Endpoint)
}

func (e *AWSReadTimeoutError) Timeout() bool   { return true }

// Python module of botocore's ReadTimeoutError
func (e *AWSReadTimeoutError) PyModule() string { return "botocore.exceptions" }
func (e *AWSReadTimeoutError) Temporary() bool { return true }

// Body whose reads fail once no byte arrives within the read timeout
type awsIdleTimeoutBody struct {
	body     io.ReadCloser
	timeout  time.Duration
	endpoint string
	cancel   context.CancelFunc
	timer    *time.Timer
	mu       sync.Mutex
	expired  bool
}

func newAWSIdleTimeoutBody(body io.ReadCloser, timeout time.Duration, endpoint string, cancel context.CancelFunc) *awsIdleTimeoutBody {
	b := &awsIdleTimeoutBody{body: body, timeout: timeout, endpoint: endpoint, cancel: cancel}
	b.timer = time.AfterFunc(timeout, b.expire)
	return b
}

func (b *awsIdleTimeoutBody) expire() {
	b.mu.Lock()
	b.expired = true
	b.mu.Unlock()
	b.cancel()
}

func (b *awsIdleTimeoutBody) Read(p []byte) (int, error) {
	n, err := b.body.Read(p)
	b.mu.Lock()
	expired := b.expired
	b.mu.Unlock()
	if expired {
		return n, &AWSReadTimeoutError{Endpoint: b.endpoint, Limit: b.timeout}
	}
	if n > 0 {
		b.timer.Reset(b.timeout)
	}
	return n, err
}

func (b *awsIdleTimeoutBody) Close() error {
	b.timer.Stop()
	err := b.body.Close()
	b.cancel()
	return err
}

// Authorizes the request with the bearer token when one is configured, else SigV4
func (c *awsServiceClient) authorize(ctx context.Context, req *http.Request, body []byte) error {
	if c.bearer != "" {
		req.Header.Set("Authorization", "Bearer "+c.bearer)
		return nil
	}
	creds, err := c.creds.Retrieve(ctx)
	if err != nil {
		return err
	}
	_, err = sigV4Sign(req, body, creds, c.signingName, c.region, awsNow())
	return err
}

// Sends one operation with the retry policy, returning the 2xx response or a typed AWS error
func (c *awsServiceClient) do(ctx context.Context, operation, method, path string, query url.Values, body []byte, headers http.Header) (*http.Response, error) {
	target := c.endpoint + path
	if len(query) > 0 {
		target += "?" + query.Encode()
	}
	for attempt := 1; ; attempt++ {
		if err := c.retry.beforeSend(ctx); err != nil {
			return nil, err
		}
		actx, cancel := context.WithCancel(ctx)
		req, err := http.NewRequestWithContext(actx, method, target, bytes.NewReader(body))
		if err != nil {
			cancel()
			return nil, err
		}
		if len(body) == 0 {
			req.Body, req.ContentLength = http.NoBody, 0
		}
		for k, vs := range headers {
			for _, v := range vs {
				req.Header.Add(k, v)
			}
		}
		if req.Header.Get("User-Agent") == "" {
			req.Header.Set("User-Agent", UserAgent("IcebathAgent"))
		}
		if err := c.authorize(actx, req, body); err != nil {
			cancel()
			return nil, err
		}
		resp, err := c.http.Do(req)
		if err != nil {
			cancel()
			if ctx.Err() != nil {
				return nil, ctx.Err()
			}
			var ne net.Error
			if errors.As(err, &ne) && ne.Timeout() {
				err = &AWSReadTimeoutError{Endpoint: target, Limit: c.readTimeout}
			}
			c.retry.afterResponse(0, "")
			if c.retry.shouldRetry(attempt, 0, "", true, isAWSTimeout(err)) {
				if werr := c.retry.wait(ctx, attempt); werr != nil {
					return nil, werr
				}
				continue
			}
			return nil, err
		}
		if resp.StatusCode >= 200 && resp.StatusCode < 300 {
			c.retry.afterResponse(resp.StatusCode, "")
			c.retry.succeeded()
			resp.Body = newAWSIdleTimeoutBody(resp.Body, c.readTimeout, target, cancel)
			return resp, nil
		}
		data, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		resp.Body.Close()
		cancel()
		apiErr := parseAWSJSONError(c.service, operation, resp, data)
		c.retry.afterResponse(resp.StatusCode, apiErr.Code)
		if c.retry.shouldRetry(attempt, resp.StatusCode, apiErr.Code, false, false) {
			if werr := c.retry.wait(ctx, attempt); werr != nil {
				return nil, werr
			}
			continue
		}
		return nil, apiErr
	}
}

// Posts a JSON operation and decodes the JSON answer into out
func (c *awsServiceClient) doJSON(ctx context.Context, operation, method, path string, query url.Values, in any, out any) error {
	var body []byte
	headers := http.Header{}
	if in != nil {
		data, err := json.Marshal(in)
		if err != nil {
			return err
		}
		body = data
		headers.Set("Content-Type", "application/json")
	}
	resp, err := c.do(ctx, operation, method, path, query, body, headers)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return err
	}
	if out == nil || len(bytes.TrimSpace(data)) == 0 {
		return nil
	}
	return json.Unmarshal(data, out)
}

func isAWSTimeout(err error) bool {
	var rt *AWSReadTimeoutError
	return errors.As(err, &rt)
}

// AWS JSON and rest-json error, code from x-amzn-ErrorType, then __type, then code, message from message
func parseAWSJSONError(service, operation string, resp *http.Response, data []byte) *AWSServiceError {
	e := &AWSServiceError{Service: service, Operation: operation, StatusCode: resp.StatusCode, RequestID: resp.Header.Get("x-amzn-RequestId"),
		Body: string(data), Headers: resp.Header.Clone()}
	var body map[string]any
	_ = json.Unmarshal(data, &body)
	if body != nil {
		e.Message = asStr(body["message"])
		if e.Message == "" {
			e.Message = asStr(body["Message"])
		}
	}
	code := ""
	switch {
	case resp.Header.Get("x-amzn-ErrorType") != "":
		code = resp.Header.Get("x-amzn-ErrorType")
	case body != nil && asStr(body["__type"]) != "":
		code = asStr(body["__type"])
	case body != nil && (asStr(body["code"]) != "" || asStr(body["Code"]) != ""):
		code = firstNonEmpty(asStr(body["code"]), asStr(body["Code"]))
	default:
		code = strconv.Itoa(resp.StatusCode)
	}
	code, _, _ = strings.Cut(code, ":")
	if i := strings.LastIndexByte(code, '#'); i >= 0 {
		code = code[i+1:]
	}
	e.Code = strings.TrimSpace(code)
	return e
}

// ---- retry policy ----

var awsThrottleCodes = map[string]bool{
	"Throttling": true, "ThrottlingException": true, "ThrottledException": true, "RequestThrottledException": true,
	"TooManyRequestsException": true, "ProvisionedThroughputExceededException": true, "TransactionInProgressException": true,
	"RequestLimitExceeded": true, "BandwidthLimitExceeded": true, "LimitExceededException": true, "RequestThrottled": true,
	"SlowDown": true, "PriorRequestNotComplete": true, "EC2ThrottledException": true,
}

var awsLegacyThrottleCodes = map[string]bool{"Throttling": true, "ThrottlingException": true, "ThrottledException": true, "RequestThrottledException": true}

var awsTransientCodes = map[string]bool{"RequestTimeout": true, "RequestTimeoutException": true, "PriorRequestNotComplete": true}

// Botocore legacy, standard or adaptive retry behaviour of one client
type awsRetryPolicy struct {
	mode        string
	maxAttempts int
	mu          sync.Mutex
	quota       int
	lastCost    int
	limiter     *awsClientRateLimiter
}

// Retry mode and attempts from the explicit total, AWS_MAX_ATTEMPTS and AWS_RETRY_MODE, then the profile
func newAWSRetryPolicy(s *awsSession, totalMaxAttempts int) *awsRetryPolicy {
	mode := strings.ToLower(strings.TrimSpace(s.setting("AWS_RETRY_MODE", "retry_mode")))
	if mode != "standard" && mode != "adaptive" {
		mode = "legacy"
	}
	p := &awsRetryPolicy{mode: mode, quota: 500}
	switch {
	case totalMaxAttempts > 0:
		p.maxAttempts = totalMaxAttempts
	default:
		if n, err := strconv.Atoi(strings.TrimSpace(s.setting("AWS_MAX_ATTEMPTS", "max_attempts"))); err == nil && n > 0 {
			p.maxAttempts = n
		} else if mode == "legacy" {
			p.maxAttempts = 5
		} else {
			p.maxAttempts = 3
		}
	}
	if mode == "adaptive" {
		p.limiter = newAWSClientRateLimiter(awsMonoSeconds)
	}
	return p
}

func (p *awsRetryPolicy) beforeSend(ctx context.Context) error {
	if p.limiter != nil {
		return p.limiter.onSendingRequest(ctx)
	}
	return nil
}

func (p *awsRetryPolicy) afterResponse(status int, code string) {
	if p.limiter != nil {
		p.limiter.onReceivingResponse(awsThrottleCodes[code] || status == 429)
	}
}

// Returns the retry cost to the quota after a successful retried call, else adds the no retry increment
func (p *awsRetryPolicy) succeeded() {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.lastCost > 0 {
		p.quota += p.lastCost
		p.lastCost = 0
	} else {
		p.quota++
	}
	if p.quota > 500 {
		p.quota = 500
	}
}

// Reports whether the failed attempt is retried under the mode's retryable set and the quota
func (p *awsRetryPolicy) shouldRetry(attempt, status int, code string, transportErr, timeout bool) bool {
	if attempt >= p.maxAttempts {
		return false
	}
	if p.mode == "legacy" {
		return transportErr || status == 500 || status == 502 || status == 503 || status == 504 || status == 509 || status == 429 || awsLegacyThrottleCodes[code]
	}
	retryable := transportErr || awsTransientCodes[code] || status == 500 || status == 502 || status == 503 || status == 504 || awsThrottleCodes[code]
	if !retryable {
		return false
	}
	cost := 5
	if timeout {
		cost = 10
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.quota < cost {
		return false
	}
	p.quota -= cost
	p.lastCost = cost
	return true
}

// Sleeps the random exponential backoff, capped at 20 seconds outside legacy mode
func (p *awsRetryPolicy) wait(ctx context.Context, attempt int) error {
	delay := rand.Float64() * math.Pow(2, float64(attempt-1))
	if p.mode != "legacy" && delay > 20 {
		delay = 20
	}
	t := time.NewTimer(time.Duration(delay * float64(time.Second)))
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

// ---- adaptive client side rate limiting ----

// Monotonic seconds clock the adaptive limiter reads, replaced by tests
var awsMonoSeconds = func() float64 { return float64(time.Now().UnixNano()) / 1e9 }

// CUBIC rate adjustment, token bucket and measured send rate of botocore adaptive mode
type awsClientRateLimiter struct {
	mu      sync.Mutex
	clock   func() float64
	enabled bool
	cubic   awsCubic
	clocker awsRateClocker
	bucket  *awsTokenBucket
}

func newAWSClientRateLimiter(clock func() float64) *awsClientRateLimiter {
	now := clock()
	l := &awsClientRateLimiter{clock: clock}
	l.cubic = awsCubic{wMax: 0.5, scale: 0.4, beta: 0.7, lastFail: now}
	l.cubic.k = l.cubic.zeroPoint()
	l.clocker = awsRateClocker{smoothing: 0.8, lastBucket: math.Floor(now), scale: 2}
	l.bucket = newAWSTokenBucket(1, clock)
	return l
}

func (l *awsClientRateLimiter) onSendingRequest(ctx context.Context) error {
	l.mu.Lock()
	enabled := l.enabled
	l.mu.Unlock()
	if enabled {
		return l.bucket.acquire(ctx)
	}
	return nil
}

func (l *awsClientRateLimiter) onReceivingResponse(throttled bool) {
	measured := l.clocker.record(l.clock())
	now := l.clock()
	l.mu.Lock()
	defer l.mu.Unlock()
	var newRate float64
	if !throttled {
		newRate = l.cubic.success(now)
	} else {
		rate := measured
		if l.enabled {
			rate = math.Min(measured, l.bucket.maxRate())
		}
		newRate = l.cubic.failure(rate, now)
		l.enabled = true
	}
	l.bucket.setMaxRate(math.Min(newRate, 2*measured))
}

type awsCubic struct {
	wMax, scale, beta, k, lastFail float64
}

func (c *awsCubic) zeroPoint() float64 {
	return math.Cbrt((c.wMax * (1 - c.beta)) / c.scale)
}

func (c *awsCubic) success(ts float64) float64 {
	return c.scale*math.Pow(ts-c.lastFail-c.k, 3) + c.wMax
}

func (c *awsCubic) failure(rate, ts float64) float64 {
	c.wMax = rate
	c.k = c.zeroPoint()
	c.lastFail = ts
	return rate * c.beta
}

type awsRateClocker struct {
	mu         sync.Mutex
	measured   float64
	smoothing  float64
	lastBucket float64
	scale      float64
	count      float64
}

func (r *awsRateClocker) record(t float64) float64 {
	r.mu.Lock()
	defer r.mu.Unlock()
	bucket := math.Floor(t*r.scale) / r.scale
	r.count++
	if bucket > r.lastBucket {
		current := r.count / (bucket - r.lastBucket)
		r.measured = current*r.smoothing + r.measured*(1-r.smoothing)
		r.count = 0
		r.lastBucket = bucket
	}
	return r.measured
}

type awsTokenBucket struct {
	mu       sync.Mutex
	clock    func() float64
	fillRate float64
	capacity float64
	current  float64
	last     float64
	haveLast bool
	rate     float64
	changed  chan struct{}
}

func newAWSTokenBucket(maxRate float64, clock func() float64) *awsTokenBucket {
	b := &awsTokenBucket{clock: clock, changed: make(chan struct{})}
	b.setMaxRate(maxRate)
	return b
}

func (b *awsTokenBucket) maxRate() float64 {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.rate
}

func (b *awsTokenBucket) refill() {
	now := b.clock()
	if !b.haveLast {
		b.last, b.haveLast = now, true
		return
	}
	b.current = math.Min(b.capacity, b.current+(now-b.last)*b.fillRate)
	b.last = now
}

func (b *awsTokenBucket) setMaxRate(v float64) {
	b.mu.Lock()
	b.refill()
	b.rate = v
	b.fillRate = math.Max(v, 0.5)
	b.capacity = 1
	if v >= 1 {
		b.capacity = v
	}
	b.current = math.Min(b.current, b.capacity)
	close(b.changed)
	b.changed = make(chan struct{})
	b.mu.Unlock()
}

// Takes one token, waiting for the refill or a rate change
func (b *awsTokenBucket) acquire(ctx context.Context) error {
	for {
		b.mu.Lock()
		b.refill()
		if b.current >= 1 {
			b.current--
			b.mu.Unlock()
			return nil
		}
		wait := time.Duration((1 - b.current) / b.fillRate * float64(time.Second))
		changed := b.changed
		b.mu.Unlock()
		t := time.NewTimer(wait)
		select {
		case <-ctx.Done():
			t.Stop()
			return ctx.Err()
		case <-changed:
			t.Stop()
		case <-t.C:
		}
	}
}
