package gcp

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/nickheyer/icebath/internal/home"
	"github.com/nickheyer/icebath/internal/httpclient"
)

const (
	stsGrant            = "urn:ietf:params:oauth:grant-type:token-exchange"
	awsSubjectTokenType = "urn:ietf:params:aws:token-type:aws4_request"
)

var workforceAudience = regexp.MustCompile(`^//iam\.googleapis\.com/locations/[^/]+/workforcePools/`)

type subjectSupplier func(ctx context.Context) (string, error)

func stsFailure(kind string, status int, data []byte) *Error {
	e := &Error{Kind: kind, Status: status, Message: "security token service refused the exchange"}
	reply, ok := parseDocument(data)
	if !ok || !reply.has("error") {
		return e
	}
	e.Code = reply.text("error")
	e.Message = "error code " + e.Code
	if desc := reply.text("error_description"); desc != "" {
		e.Message += ": " + desc
	}
	if uri := reply.text("error_uri"); uri != "" {
		e.Message += " - " + uri
	}
	return e
}

func (c *client) stsRequest(ctx context.Context, hc *http.Client, kind, endpoint, clientID, clientSecret string, body []byte) (document, error) {
	header := http.Header{"Content-Type": {formType}}
	if clientID != "" {
		header.Set("Authorization", "Basic "+base64.StdEncoding.EncodeToString([]byte(clientID+":"+clientSecret)))
	}
	status, _, data, err := c.send(ctx, hc, requestTimeout, http.MethodPost, endpoint, header, body)
	if err != nil {
		return nil, &Error{Kind: kind, Message: "security token service could not be reached: " + httpclient.Message(err), Retryable: true, Err: err}
	}
	if status != http.StatusOK {
		return nil, stsFailure(kind, status, data)
	}
	reply, ok := parseDocument(data)
	if !ok {
		return nil, &Error{Kind: kind, Status: status, Message: "security token service reply is not a JSON object"}
	}
	return reply, nil
}

func (c *client) externalAccount(info document, scopes []string) (*credentials, error) {
	fail := func(message string) (*credentials, error) {
		return nil, &Error{Kind: kindExternal, Message: message}
	}
	audience, subjectType := info.text("audience"), info.text("subject_token_type")
	source, ok := info.object("credential_source")
	if !ok {
		return fail("credential_source is not an object")
	}
	if audience == "" || subjectType == "" {
		return fail("file needs audience and subject_token_type")
	}
	creds := &credentials{kind: kindExternal, universe: info.text("universe_domain"), quotaProject: info.text("quota_project_id")}
	universe := creds.domain()
	tokenURL := info.text("token_url")
	workforceProject := info.text("workforce_pool_user_project")
	if workforceProject != "" && !workforceAudience.MatchString(audience) {
		return fail("workforce_pool_user_project should not be set for non-workforce pool credentials")
	}
	clientID, clientSecret := info.text("client_id"), info.text("client_secret")
	impersonationURL := info.text("service_account_impersonation_url")
	principal, hasPrincipal := principalOf(impersonationURL)
	lifetime := 0
	if options, ok := info.object("service_account_impersonation"); ok {
		if seconds, ok := options.number("token_lifetime_seconds"); ok {
			lifetime = int(seconds)
		}
	}
	var supplier subjectSupplier
	var err error
	sts := c.api
	switch {
	case subjectType == awsSubjectTokenType:
		supplier, err = c.awsSubject(source, audience)
	case source.has("executable") && string(source["executable"]) != "null":
		supplier, err = c.executableSubject(source, audience, subjectType, impersonationURL, principal)
	default:
		var mtls *http.Client
		supplier, mtls, err = c.poolSubject(source)
		if mtls != nil {
			sts = mtls
			if tokenURL == "" {
				tokenURL = "https://sts.mtls." + universe + "/v1/token"
			}
		}
	}
	if err != nil {
		return nil, err
	}
	if tokenURL == "" {
		tokenURL = "https://sts." + universe + "/v1/token"
	}
	exchange := func(ctx context.Context, scopes []string) (string, time.Time, error) {
		now := time.Now().UTC()
		subject, err := supplier(ctx)
		if err != nil {
			return "", time.Time{}, err
		}
		form := []pair{{"grant_type", stsGrant}, {"audience", audience}}
		if len(scopes) > 0 {
			form = append(form, pair{"scope", strings.Join(scopes, " ")})
		}
		form = append(form, pair{"requested_token_type", accessTokenType}, pair{"subject_token", subject}, pair{"subject_token_type", subjectType})
		if workforceProject != "" && clientID == "" {
			form = append(form, pair{"options", quotePath(string(spacedJSON(object{{"userProject", workforceProject}})))})
		}
		reply, err := c.stsRequest(ctx, sts, kindExternal, tokenURL, clientID, clientSecret, encodeForm(form...))
		if err != nil {
			return "", time.Time{}, err
		}
		seconds, ok := reply.number("expires_in")
		token := reply.text("access_token")
		if token == "" || !ok {
			return "", time.Time{}, &Error{Kind: kindExternal, Message: "security token service reply carried no access token and lifetime"}
		}
		return token, now.Add(time.Duration(seconds * float64(time.Second))), nil
	}
	creds.allowed = c.allowedFor(kindExternal, func(context.Context) (string, error) {
		if m := workloadPool.FindStringSubmatch(audience); m != nil {
			return fmt.Sprintf(workloadBoundary, universe, m[1], m[2]), nil
		}
		if m := workforcePool.FindStringSubmatch(audience); m != nil {
			return fmt.Sprintf(workforceBoundary, universe, m[1]), nil
		}
		return "", &Error{Kind: kindExternal, Message: "Invalid audience format."}
	})
	if impersonationURL != "" {
		if !hasPrincipal {
			return fail("unable to determine target principal from service account impersonation URL")
		}
		inner := &credentials{kind: kindExternal, universe: creds.universe, allowed: creds.allowed}
		inner.mint = func(ctx context.Context) (string, time.Time, error) { return exchange(ctx, []string{iamScope}) }
		impersonation := c.impersonated(inner, principal, nil, scopes, lifetime, impersonationURL)
		creds.mint = func(ctx context.Context) (string, time.Time, error) {
			inner.quotaProject = creds.quotaProject
			impersonation.mu.Lock()
			defer impersonation.mu.Unlock()
			if err := impersonation.refreshLocked(ctx); err != nil {
				return "", time.Time{}, err
			}
			return impersonation.token, impersonation.expiry, nil
		}
	} else {
		creds.mint = func(ctx context.Context) (string, time.Time, error) { return exchange(ctx, scopes) }
	}
	number := projectNumber(audience)
	if number == "" {
		number = workforceProject
	}
	creds.lookup = func(ctx context.Context) (string, error) {
		if number == "" || len(scopes) == 0 {
			return "", nil
		}
		header := http.Header{}
		if err := creds.authorize(ctx, header); err != nil {
			return "", err
		}
		endpoint := strings.Replace(resourceManager, defaultUniverse, universe, 1) + number
		status, _, data, err := c.send(ctx, c.api, requestTimeout, http.MethodGet, endpoint, header, nil)
		if err != nil {
			return "", &Error{Kind: kindExternal, Message: "project lookup could not be reached: " + httpclient.Message(err), Retryable: true, Err: err}
		}
		if status != http.StatusOK {
			return "", nil
		}
		reply, _ := parseDocument(data)
		return reply.text("projectId"), nil
	}
	return creds, nil
}

func projectNumber(audience string) string {
	parts := strings.Split(audience, "/")
	for i, part := range parts {
		if part == "projects" {
			if i+1 < len(parts) {
				return parts[i+1]
			}
			return ""
		}
	}
	return ""
}

func subjectFrom(payload []byte, format, field, location string) (string, error) {
	token := string(payload)
	if format != "text" {
		doc, ok := parseDocument(payload)
		if !ok || !doc.has(field) {
			return "", &Error{Kind: kindExternal, Message: "unable to read the subject token from " + location + " using key " + field}
		}
		var value any
		if json.Unmarshal(doc[field], &value) != nil {
			return "", &Error{Kind: kindExternal, Message: "unable to read the subject token from " + location + " using key " + field}
		}
		token, _ = value.(string)
	}
	if token == "" {
		return "", &Error{Kind: kindExternal, Message: "missing subject token in the credential source " + location}
	}
	return token, nil
}

func (c *client) poolSubject(source document) (subjectSupplier, *http.Client, error) {
	fail := func(message string) (subjectSupplier, *http.Client, error) {
		return nil, nil, &Error{Kind: kindExternal, Message: message}
	}
	if source.has("environment_id") {
		return fail("invalid identity pool credential_source field environment_id")
	}
	file, target := source.text("file"), source.text("url")
	certificate, hasCertificate := source.object("certificate")
	format, field := "text", ""
	if spec, ok := source.object("format"); ok {
		if kind := spec.text("type"); kind != "" {
			format = kind
		}
		field = spec.text("subject_token_field_name")
	}
	if format != "text" && format != "json" {
		return fail("invalid credential_source format " + format)
	}
	if format == "json" && field == "" {
		return fail("missing subject_token_field_name for JSON credential_source format")
	}
	set := 0
	for _, present := range []bool{file != "", target != "", hasCertificate} {
		if present {
			set++
		}
	}
	if set > 1 {
		return fail("ambiguous credential_source, file, url and certificate are mutually exclusive")
	}
	switch {
	case file != "":
		return func(context.Context) (string, error) {
			data, err := readBounded(file)
			if err != nil {
				return "", &Error{Kind: kindExternal, Message: "credential source file " + file + " could not be read", Err: err}
			}
			return subjectFrom(data, format, field, file)
		}, nil, nil
	case target != "":
		header := http.Header{}
		if headers, ok := source.object("headers"); ok {
			for name := range headers {
				header.Set(name, headers.text(name))
			}
		}
		return func(ctx context.Context) (string, error) {
			status, _, data, err := c.send(ctx, c.api, requestTimeout, http.MethodGet, target, header, nil)
			if err != nil {
				return "", &Error{Kind: kindExternal, Message: "credential source URL could not be reached: " + httpclient.Message(err), Retryable: true, Err: err}
			}
			if status != http.StatusOK {
				return "", &Error{Kind: kindExternal, Status: status, Message: "unable to retrieve the identity pool subject token"}
			}
			return subjectFrom(data, format, field, target)
		}, nil, nil
	case hasCertificate:
		return c.certificateSubject(certificate)
	}
	return fail("missing credential_source, a file, url or certificate must be provided")
}

func certificateConfigPath(location string) (string, error) {
	if location == "" {
		location = os.Getenv("GOOGLE_API_CERTIFICATE_CONFIG")
	}
	if location == "" {
		userHome, err := home.UserHome()
		if err != nil {
			return "", err
		}
		return filepath.Join(userHome, ".config", "gcloud", "certificate_config.json"), nil
	}
	return home.ExpandUser(location), nil
}

func (c *client) certificateSubject(spec document) (subjectSupplier, *http.Client, error) {
	fail := func(message string, err error) (subjectSupplier, *http.Client, error) {
		return nil, nil, &Error{Kind: kindExternal, Message: message, Err: err}
	}
	location := spec.text("certificate_config_location")
	var useDefault bool
	_ = json.Unmarshal(spec["use_default_certificate_config"], &useDefault)
	trustChain := spec.text("trust_chain_path")
	if location != "" && useDefault {
		return fail("invalid certificate configuration, certificate_config_location cannot be specified when use_default_certificate_config is true", nil)
	}
	if location == "" && !useDefault {
		return fail("invalid certificate configuration, use_default_certificate_config should be true if no certificate_config_location is provided", nil)
	}
	configPath, err := certificateConfigPath(location)
	if err != nil {
		return fail("certificate configuration could not be located", err)
	}
	raw, err := readBounded(configPath)
	if err != nil {
		return fail("certificate config file "+configPath+" could not be read", err)
	}
	var config struct {
		CertConfigs struct {
			Workload *struct {
				CertPath string `json:"cert_path"`
				KeyPath  string `json:"key_path"`
			} `json:"workload"`
		} `json:"cert_configs"`
	}
	if err := json.Unmarshal(raw, &config); err != nil || config.CertConfigs.Workload == nil {
		return fail("certificate config file "+configPath+" holds no workload certificate configuration", nil)
	}
	certPath, keyPath := config.CertConfigs.Workload.CertPath, config.CertConfigs.Workload.KeyPath
	if certPath == "" || keyPath == "" {
		return fail("certificate config file "+configPath+" needs cert_path and key_path", nil)
	}
	identity, err := httpclient.LoadClientCertificate(certPath, keyPath, "")
	if err != nil {
		return fail("workload certificate could not be loaded", err)
	}
	opts := c.http
	opts.TLS.Certificates = append(append([]tls.Certificate(nil), opts.TLS.Certificates...), identity)
	opts.Timeouts = httpclient.EachPhase(requestTimeout)
	transport, err := httpclient.New(opts)
	if err != nil {
		return fail("mutual TLS transport could not be built", err)
	}
	supplier := func(context.Context) (string, error) {
		certPEM, err := readBounded(certPath)
		if err != nil {
			return "", &Error{Kind: kindExternal, Message: "workload certificate " + certPath + " could not be read", Err: err}
		}
		block, _ := pem.Decode(certPEM)
		if block == nil || block.Type != "CERTIFICATE" {
			return "", &Error{Kind: kindExternal, Message: "workload certificate " + certPath + " is not a PEM certificate"}
		}
		leaf := base64.StdEncoding.EncodeToString(block.Bytes)
		chain := []string{leaf}
		if trustChain != "" {
			data, err := readBounded(trustChain)
			if err != nil {
				return "", &Error{Kind: kindExternal, Message: "trust chain file " + trustChain + " could not be read", Err: err}
			}
			index := 0
			for {
				data = bytes.TrimSpace(data)
				if len(data) == 0 {
					break
				}
				var entry *pem.Block
				entry, data = pem.Decode(data)
				if entry == nil || entry.Type != "CERTIFICATE" {
					return "", &Error{Kind: kindExternal, Message: "error loading PEM certificates from the trust chain file " + trustChain}
				}
				encoded := base64.StdEncoding.EncodeToString(entry.Bytes)
				switch {
				case encoded != leaf:
					chain = append(chain, encoded)
				case index != 0:
					return "", &Error{Kind: kindExternal, Message: "the leaf certificate must be at the top of the trust chain file"}
				}
				index++
			}
		}
		return string(spacedJSON(chain)), nil
	}
	return supplier, transport.Client(), nil
}
