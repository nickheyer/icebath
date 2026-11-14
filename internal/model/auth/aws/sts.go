package aws

import (
	"bytes"
	"context"
	"encoding/xml"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/nickheyer/icebath/internal/httpclient"
)

var stsLegacyGlobalRegions = map[string]bool{
	"ap-northeast-1": true, "ap-south-1": true, "ap-southeast-1": true, "ap-southeast-2": true, "aws-global": true,
	"ca-central-1": true, "eu-central-1": true, "eu-north-1": true, "eu-west-1": true, "eu-west-2": true, "eu-west-3": true,
	"sa-east-1": true, "us-east-1": true, "us-east-2": true, "us-west-1": true, "us-west-2": true,
}

type stsKeys struct {
	AccessKeyID     string `xml:"AccessKeyId"`
	SecretAccessKey string `xml:"SecretAccessKey"`
	SessionToken    string `xml:"SessionToken"`
	Expiration      string `xml:"Expiration"`
}

type stsReply struct {
	AssumeRole  stsKeys `xml:"AssumeRoleResult>Credentials"`
	WebIdentity stsKeys `xml:"AssumeRoleWithWebIdentityResult>Credentials"`
}

type stsFault struct {
	Code    string `xml:"Error>Code"`
	Message string `xml:"Error>Message"`
}

func send(client *http.Client, req *http.Request, source string) (int, http.Header, []byte, error) {
	resp, err := client.Do(req)
	if err != nil {
		return 0, nil, nil, &Error{Source: source, Message: httpclient.Message(err), Err: err}
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, replyLimit))
	if err != nil {
		return 0, nil, nil, &Error{Source: source, Message: httpclient.Message(err), Err: err}
	}
	return resp.StatusCode, resp.Header, data, nil
}

func (r *resolver) stsEndpoint() (string, string) {
	region := strings.TrimSpace(r.c.opts.Region)
	if region == "" {
		region = strings.TrimSpace(r.s.region())
	}
	signing := region
	if region == "" || region == "aws-global" {
		signing = "us-east-1"
	}
	if ep := r.s.configuredEndpoint("sts"); ep != "" {
		return strings.TrimRight(ep, "/"), signing
	}
	mode := strings.ToLower(strings.TrimSpace(r.s.value("AWS_STS_REGIONAL_ENDPOINTS", "sts_regional_endpoints")))
	if region == "" || region == "aws-global" || (mode == "legacy" && stsLegacyGlobalRegions[region]) {
		return "https://sts.amazonaws.com", "us-east-1"
	}
	return r.s.endpoint("sts", "sts", region), signing
}

func (r *resolver) sts(ctx context.Context, action, source string, params url.Values, signer *Credentials) (Credentials, error) {
	endpoint, region := r.stsEndpoint()
	params.Set("Action", action)
	params.Set("Version", "2011-06-15")
	body := []byte(params.Encode())
	ctx, cancel := context.WithTimeout(ctx, callDeadline)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint+"/", bytes.NewReader(body))
	if err != nil {
		return Credentials{}, &Error{Source: source, Message: "the STS endpoint is not a usable URL", Err: err}
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded; charset=utf-8")
	if signer != nil {
		if err := signRequest(req, body, *signer, "sts", region, time.Now()); err != nil {
			return Credentials{}, err
		}
	}
	status, _, data, err := send(r.c.api, req, source)
	if err != nil {
		return Credentials{}, err
	}
	if status != http.StatusOK {
		var fault stsFault
		_ = xml.Unmarshal(data, &fault)
		return Credentials{}, &ServiceError{Service: "sts", Operation: action, Status: status, Code: fault.Code, Message: fault.Message}
	}
	var reply stsReply
	if err := xml.Unmarshal(data, &reply); err != nil {
		return Credentials{}, &Error{Source: source, Message: "the STS reply is not readable XML"}
	}
	keys := reply.AssumeRole
	if action == "AssumeRoleWithWebIdentity" {
		keys = reply.WebIdentity
	}
	creds := Credentials{AccessKeyID: keys.AccessKeyID, SecretAccessKey: keys.SecretAccessKey, SessionToken: keys.SessionToken, Source: source}
	if !creds.complete() {
		return Credentials{}, &Error{Source: source, Message: "the STS reply carried no credentials"}
	}
	if keys.Expiration != "" {
		t, err := parseTime(keys.Expiration)
		if err != nil {
			return Credentials{}, &Error{Source: source, Message: "the STS reply carried an unreadable expiration", Err: err}
		}
		creds.Expires = t
	}
	return creds, nil
}

func defaultSessionName() string {
	return fmt.Sprintf("botocore-session-%d", time.Now().Unix())
}

func hasAssumeRole(p section) bool {
	_, role := p["role_arn"]
	_, web := p["web_identity_token_file"]
	return role && !web
}

func (r *resolver) assumeRole(ctx context.Context, profile string) (provider, error) {
	if !hasAssumeRole(r.s.files.profiles[profile]) {
		return nil, nil
	}
	r.visited = []string{profile}
	return r.viaAssumeRole(ctx, profile)
}

func (r *resolver) viaAssumeRole(ctx context.Context, profile string) (provider, error) {
	const source = "assume-role"
	p := r.s.files.profiles[profile]
	sourceProfile, hasSource := p["source_profile"]
	credSource, hasCredSource := p["credential_source"]
	switch {
	case hasSource && hasCredSource:
		return nil, &Error{Source: source, Message: fmt.Sprintf("the profile %q contains both source_profile and credential_source", profile)}
	case !hasSource && !hasCredSource:
		return nil, partial(source, "source_profile or credential_source")
	case hasCredSource:
		if credSource != "Environment" && credSource != "EcsContainer" && credSource != "Ec2InstanceMetadata" {
			return nil, &Error{Source: source, Message: fmt.Sprintf("the credential source %q referenced in profile %q is not valid", credSource, profile)}
		}
	default:
		if err := r.validateSourceProfile(profile, sourceProfile); err != nil {
			return nil, err
		}
	}
	var from provider
	var err error
	if hasCredSource {
		from, err = r.fromCredentialSource(ctx, credSource, profile)
	} else {
		r.visited = append(r.visited, sourceProfile)
		from, err = r.fromProfile(ctx, sourceProfile)
	}
	if err != nil {
		return nil, err
	}
	base := url.Values{"RoleArn": {p["role_arn"]}}
	sessionName, ok := p["role_session_name"]
	if !ok || sessionName == "" {
		sessionName = defaultSessionName()
	}
	base.Set("RoleSessionName", sessionName)
	if v, ok := p["external_id"]; ok {
		base.Set("ExternalId", v)
	}
	serial, hasMFA := p["mfa_serial"]
	if hasMFA {
		base.Set("SerialNumber", serial)
	}
	if v, ok := p["duration_seconds"]; ok {
		if n, err := strconv.Atoi(strings.TrimSpace(v)); err == nil {
			base.Set("DurationSeconds", strconv.Itoa(n))
		}
	}
	prompt := r.c.opts.MFA
	calls := 0
	fetch := func(ctx context.Context) (Credentials, error) {
		if hasMFA && (calls > 0 || prompt == nil) {
			return Credentials{}, &Error{Source: source, Message: "the role needs an MFA code for " + serial, Err: ErrMFARequired}
		}
		calls++
		signer, err := from.retrieve(ctx)
		if err != nil {
			return Credentials{}, err
		}
		params := url.Values{}
		for k, v := range base {
			params[k] = append([]string(nil), v...)
		}
		if hasMFA {
			code, err := prompt(ctx, serial)
			if err != nil {
				return Credentials{}, &Error{Source: source, Message: "no MFA code was given for " + serial, Err: err}
			}
			params.Set("TokenCode", strings.TrimSpace(code))
		}
		return r.sts(ctx, "AssumeRole", source, params, &signer)
	}
	return newRefreshing(fetch, nil), nil
}

func (r *resolver) validateSourceProfile(parent, name string) error {
	sp, ok := r.s.files.profiles[name]
	if !ok {
		return &Error{Source: "assume-role", Message: fmt.Sprintf("the source_profile %q referenced in the profile %q does not exist", name, parent)}
	}
	seen := false
	for _, v := range r.visited {
		if v == name {
			seen = true
		}
	}
	if !seen {
		return nil
	}
	if name != parent || !hasStaticKeys(sp) {
		return &Error{Source: "assume-role", Message: fmt.Sprintf("infinite loop in credential configuration, profile %s was already visited (%s)", name, strings.Join(r.visited, ", "))}
	}
	return nil
}

func (r *resolver) fromProfile(ctx context.Context, profile string) (provider, error) {
	p := r.s.files.profiles[profile]
	if hasStaticKeys(p) || !hasAssumeRole(p) {
		for _, link := range r.profileLinks(profile, true) {
			found, err := link(ctx)
			if err != nil {
				return nil, err
			}
			if found != nil {
				return found, nil
			}
		}
		return nil, &Error{Source: "assume-role", Message: fmt.Sprintf("the source profile %q must have credentials", profile)}
	}
	return r.viaAssumeRole(ctx, profile)
}

func (r *resolver) fromCredentialSource(ctx context.Context, name, profile string) (provider, error) {
	var found provider
	var err error
	switch name {
	case "Environment":
		found, err = envProvider()
	case "EcsContainer":
		found, err = r.container(ctx)
	case "Ec2InstanceMetadata":
		found, err = r.instanceMetadata(ctx)
	}
	if err != nil {
		return nil, err
	}
	if found == nil {
		return nil, &Error{Source: name, Message: "no credentials found in credential_source referenced in profile " + profile}
	}
	return found, nil
}

func (r *resolver) webIdentity(profile string, envDisabled bool) (provider, error) {
	const source = "assume-role-with-web-identity"
	p := r.s.files.profiles[profile]
	get := func(key, envName string) (string, bool) {
		if !envDisabled {
			if v, ok := os.LookupEnv(envName); ok {
				return v, true
			}
		}
		v, ok := p[key]
		return v, ok
	}
	tokenFile, _ := get("web_identity_token_file", "AWS_WEB_IDENTITY_TOKEN_FILE")
	if tokenFile == "" {
		return nil, nil
	}
	roleARN, _ := get("role_arn", "AWS_ROLE_ARN")
	if roleARN == "" {
		return nil, &Error{Source: source, Message: "the profile or the environment is configured to assume a role with web identity but has no role ARN, set role_arn in the profile or the AWS_ROLE_ARN variable"}
	}
	sessionName, ok := get("role_session_name", "AWS_ROLE_SESSION_NAME")
	if !ok || sessionName == "" {
		sessionName = defaultSessionName()
	}
	fetch := func(ctx context.Context) (Credentials, error) {
		token, err := readBounded(tokenFile)
		if err != nil {
			return Credentials{}, &Error{Source: source, Message: "unable to read the web identity token file " + tokenFile, Err: err}
		}
		params := url.Values{"RoleArn": {roleARN}, "RoleSessionName": {sessionName}, "WebIdentityToken": {string(token)}}
		return r.sts(ctx, "AssumeRoleWithWebIdentity", source, params, nil)
	}
	return newRefreshing(fetch, nil), nil
}
