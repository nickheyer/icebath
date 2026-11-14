package azure

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/nickheyer/icebath/internal/httpclient"
)

const (
	imdsAuthority       = "http://169.254.169.254"
	imdsTokenPath       = "/metadata/identity/oauth2/token"
	imdsNoResponse      = "ManagedIdentityCredential authentication unavailable, no response from the IMDS endpoint."
	managedNoEndpoint   = "No managed identity endpoint found. \nThe Target Azure platform could not be determined from environment variables."
	arcKeyLimit         = 4096
	platformAppService  = "App Service managed identity"
	platformFabric      = "Service Fabric managed identity"
	platformArc         = "Azure Arc managed identity"
	platformML          = "Azure ML managed identity"
	platformCloudShell  = "Cloud Shell managed identity"
	platformIMDS        = "IMDS"
	longLivedThreshold  = 7200
	imdsRetries         = 5
	imdsBackoff         = 2 * time.Second
	platformRetries     = 3
	platformBackoff     = 800 * time.Millisecond
	platformBackoffCeil = 60 * time.Second
)

type managed struct {
	c        *client
	platform string
	clientID string
	cache    tokenCache

	mu        sync.Mutex
	reachable bool
}

func (s *managed) name() string { return nameManaged }

func (s *managed) String() string { return nameManaged + " on " + s.platform }

func (s *managed) GoString() string { return s.String() }

func (c *client) managedIdentity(clientID string, excludeWorkload bool) (source, error) {
	set := func(name string) bool { return os.Getenv(name) != "" }
	s := &managed{c: c, clientID: clientID}
	switch {
	case set(identityEndpoint) && set(identityHeader) && set(identityThumbprint):
		s.platform = platformFabric
	case set(identityEndpoint) && set(identityHeader):
		s.platform = platformAppService
	case set(identityEndpoint) && set(imdsEndpoint):
		s.platform = platformArc
	case set(identityEndpoint):
	case set(msiEndpoint) && set(msiSecret):
		s.platform = platformML
	case set(msiEndpoint):
		s.platform = platformCloudShell
	case set(authorityVariable) && set(tenantVariable) && set(tokenFileVariable) && !excludeWorkload:
		slog.Info("ManagedIdentityCredential will use workload identity")
		workloadClient := clientID
		if workloadClient == "" {
			workloadClient = os.Getenv(clientVariable)
		}
		if workloadClient == "" {
			return nil, &AuthError{Credential: nameManaged, Message: "Workload identity authentication requires a client_id or the AZURE_CLIENT_ID environment variable."}
		}
		return c.workloadIdentity(os.Getenv(tenantVariable), workloadClient, os.Getenv(tokenFileVariable), authorityHost(os.Getenv))
	default:
		s.platform = platformIMDS
		_, s.reachable = os.LookupEnv(podAuthorityHost)
	}
	if s.platform != "" {
		slog.Info("ManagedIdentityCredential will use " + s.platform)
	}
	return s, nil
}

func (s *managed) token(ctx context.Context, scope string, inChain bool) (accessToken, error) {
	if s.platform == "" {
		return accessToken{}, unavailable(nameManaged, managedNoEndpoint)
	}
	resource := resourceOf(scope)
	return s.cache.get(ctx, scope, func(ctx context.Context) (accessToken, error) {
		var token accessToken
		var err error
		if s.platform == platformIMDS {
			token, err = s.imds(ctx, resource, inChain)
		} else {
			token, err = s.platformToken(ctx, resource)
		}
		if err != nil {
			return accessToken{}, err
		}
		if now := time.Now().Unix(); token.refreshOn == 0 && token.expiresOn-now >= longLivedThreshold {
			token.refreshOn = now + (token.expiresOn-now)/2
		}
		return token, nil
	})
}

func retryable(status int, statuses []int) bool {
	for _, s := range statuses {
		if status == s {
			return true
		}
	}
	return status >= 500 && status < 600
}

func (c *client) getRetrying(ctx context.Context, hc *http.Client, method, target string, header http.Header, body []byte, statuses []int, retries int, backoff time.Duration) (int, http.Header, []byte, error) {
	wait := backoff
	for attempt := 0; ; attempt++ {
		status, replyHeader, data, err := c.send(ctx, hc, requestTimeout, method, target, header, body)
		if attempt >= retries || (err == nil && !retryable(status, statuses)) || ctx.Err() != nil {
			return status, replyHeader, data, err
		}
		delay := wait
		if err == nil {
			if after, ok := httpclient.RetryAfter(replyHeader, time.Now()); ok && after > delay {
				delay = after
			}
		}
		if delay > platformBackoffCeil {
			delay = platformBackoffCeil
		}
		timer := time.NewTimer(delay)
		select {
		case <-ctx.Done():
			timer.Stop()
			return 0, nil, nil, ctx.Err()
		case <-timer.C:
		}
		wait *= 2
	}
}

func (s *managed) imdsURL(resource string) string {
	host := imdsAuthority
	if value, ok := os.LookupEnv(podAuthorityHost); ok {
		host = value
	}
	query := []pair{{"api-version", "2018-02-01"}, {"resource", resource}}
	if s.clientID != "" {
		query = append(query, pair{"client_id", s.clientID})
	}
	return withQuery(strings.Trim(host, "/")+imdsTokenPath, query)
}

func gatewayUnreachable(status int, reply document) bool {
	return status == http.StatusForbidden && strings.Contains(reply.text("error")+" "+reply.text("error_description")+" "+reply.text("message"), "unreachable")
}

func replyDetail(reply document) string {
	if detail := reply.text("error_description"); detail != "" {
		return detail
	}
	if detail := reply.text("error"); detail != "" {
		return detail
	}
	return reply.text("message")
}

func (s *managed) imds(ctx context.Context, resource string, inChain bool) (accessToken, error) {
	target := s.imdsURL(resource)
	header := http.Header{"Metadata": {"true"}}
	s.mu.Lock()
	reachable := s.reachable
	s.mu.Unlock()
	if inChain && !reachable {
		status, _, data, err := s.c.send(ctx, s.c.probe, requestTimeout, http.MethodGet, target, header, nil)
		if err != nil {
			return accessToken{}, &UnavailableError{Credential: nameManaged, Message: imdsNoResponse, Err: err}
		}
		if reply, _ := parseDocument(data); gatewayUnreachable(status, reply) {
			return accessToken{}, unavailable(nameManaged, "ManagedIdentityCredential authentication unavailable. Error: "+replyDetail(reply))
		}
		s.mu.Lock()
		s.reachable = true
		s.mu.Unlock()
	}
	requestTime := time.Now().Unix()
	status, _, data, err := s.c.getRetrying(ctx, s.c.imds, http.MethodGet, target, header, nil, []int{http.StatusNotFound, http.StatusGone, http.StatusTooManyRequests}, imdsRetries, imdsBackoff)
	if err != nil {
		return accessToken{}, &UnavailableError{Credential: nameManaged, Message: imdsNoResponse, Err: err}
	}
	reply, ok := parseDocument(data)
	if !ok {
		return accessToken{}, unavailable(nameManaged, imdsNoResponse)
	}
	switch {
	case status == http.StatusOK:
		return tokenFromReply(nameManaged, status, data, requestTime)
	case status == http.StatusBadRequest:
		message := "ManagedIdentityCredential authentication unavailable. No identity has been assigned to this resource."
		if s.clientID != "" {
			message = "ManagedIdentityCredential authentication unavailable. The requested identity has not been assigned to this resource."
		}
		if detail := replyDetail(reply); detail != "" {
			message += " Error: " + detail
		}
		return accessToken{}, unavailable(nameManaged, message)
	case gatewayUnreachable(status, reply):
		return accessToken{}, unavailable(nameManaged, "ManagedIdentityCredential authentication unavailable. Error: "+replyDetail(reply))
	}
	return accessToken{}, &AuthError{Credential: nameManaged, Status: status, Code: reply.text("error"), Message: "Authentication failed: " + replyDetail(reply)}
}

func arcKeyDirectory() (string, error) {
	if dir := arcTokensDir(); dir != "" {
		return dir, nil
	}
	return "", errors.New("Azure Arc key directory is not defined on this system")
}

func (s *managed) arcSecret(header http.Header) (string, error) {
	challenge := strings.Split(header.Get("WWW-Authenticate"), "=")
	if len(challenge) != 2 || !strings.EqualFold(challenge[0], "Basic realm") {
		return "", unavailable(nameManaged, platformArc+" configuration not found in environment. Unrecognizable WWW-Authenticate header.")
	}
	keyPath := challenge[1]
	expected, err := arcKeyDirectory()
	if err != nil {
		return "", unavailable(nameManaged, platformArc+" configuration not found in environment. "+err.Error())
	}
	if filepath.Clean(filepath.Dir(keyPath)) != filepath.Clean(expected) || filepath.Ext(keyPath) != ".key" {
		return "", unavailable(nameManaged, platformArc+" configuration not found in environment. Unexpected file path from HTTP response: "+keyPath)
	}
	info, err := os.Stat(keyPath)
	if err != nil || !info.Mode().IsRegular() || info.Size() > arcKeyLimit {
		return "", unavailable(nameManaged, platformArc+" configuration not found in environment. Invalid key file: "+keyPath)
	}
	key, err := os.ReadFile(keyPath)
	if err != nil {
		return "", unavailable(nameManaged, platformArc+" configuration not found in environment. Invalid key file: "+keyPath)
	}
	return string(key), nil
}

func (s *managed) platformToken(ctx context.Context, resource string) (accessToken, error) {
	env := os.Getenv
	query := []pair{{"resource", resource}}
	header := http.Header{}
	method, endpoint, hc := http.MethodGet, env(identityEndpoint), s.c.api
	var body []byte
	fail := func(detail string) (accessToken, error) {
		return accessToken{}, unavailable(nameManaged, s.platform+" configuration not found in environment. "+detail)
	}
	withClient := func(parameter string) {
		if s.clientID != "" {
			query = append(query, pair{parameter, s.clientID})
		}
	}
	switch s.platform {
	case platformAppService:
		query = append([]pair{{"api-version", "2019-08-01"}}, query...)
		withClient("client_id")
		header["X-IDENTITY-HEADER"] = []string{env(identityHeader)}
		header["Metadata"] = []string{"true"}
	case platformFabric:
		query = append([]pair{{"api-version", "2019-07-01-preview"}}, query...)
		header["Secret"] = []string{env(identityHeader)}
		hc = s.c.loose
	case platformML:
		endpoint = env(msiEndpoint)
		query = append([]pair{{"api-version", "2017-09-01"}}, query...)
		if s.clientID != "" {
			query = append(query, pair{"clientid", s.clientID})
		} else if fallback := env("DEFAULT_IDENTITY_CLIENT_ID"); fallback != "" {
			query = append(query, pair{"clientid", fallback})
		}
		header["secret"] = []string{env(msiSecret)}
	case platformCloudShell:
		if s.clientID != "" {
			return fail("Cloud Shell does not support user-assigned managed identities.")
		}
		method, endpoint = http.MethodPost, env(msiEndpoint)
		body, query = encodeForm(query), nil
		header["Content-Type"] = []string{formType}
		header["Metadata"] = []string{"true"}
	case platformArc:
		if s.clientID != "" {
			return fail("Azure Arc supports only system-assigned managed identity.")
		}
		query = append([]pair{{"api-version", "2020-06-01"}}, query...)
		header["Metadata"] = []string{"true"}
	}
	target := withQuery(endpoint, query)
	requestTime := time.Now().Unix()
	status, replyHeader, data, err := s.c.getRetrying(ctx, hc, method, target, header, body, []int{http.StatusRequestTimeout, http.StatusTooManyRequests}, platformRetries, platformBackoff)
	if err != nil {
		return fail(httpclient.Message(err))
	}
	if s.platform == platformArc && status == http.StatusUnauthorized {
		secret, err := s.arcSecret(replyHeader)
		if err != nil {
			return accessToken{}, err
		}
		header = header.Clone()
		header["Authorization"] = []string{"Basic " + secret}
		requestTime = time.Now().Unix()
		if status, _, data, err = s.c.getRetrying(ctx, hc, method, target, header, nil, []int{http.StatusRequestTimeout, http.StatusTooManyRequests}, platformRetries, platformBackoff); err != nil {
			return fail(httpclient.Message(err))
		}
	}
	token, err := tokenFromReply(nameManaged, status, data, requestTime)
	if err != nil {
		reply, _ := parseDocument(data)
		detail := replyDetail(reply)
		if detail == "" {
			var authErr *AuthError
			if errors.As(err, &authErr) {
				detail = authErr.Message
			}
		}
		return fail(detail)
	}
	return token, nil
}
