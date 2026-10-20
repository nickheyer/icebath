package sigv4

import (
	"context"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"
)

const stsVersion = "2011-06-15"

// Role a profile or the environment asks STS for
type roleRequest struct {
	RoleARN     string
	SessionName string
	ExternalID  string
	Duration    int
	Serial      string
	Code        string
}

// Credentials from AWS_WEB_IDENTITY_TOKEN_FILE and AWS_ROLE_ARN exchanged at STS
type WebIdentity struct {
	// Used for the STS call, a ten second client when nil
	Client *http.Client
}

func (w WebIdentity) Credentials(ctx context.Context) (Credentials, error) {
	file, role := os.Getenv("AWS_WEB_IDENTITY_TOKEN_FILE"), os.Getenv("AWS_ROLE_ARN")
	if file == "" || role == "" {
		return Credentials{}, errors.New("sigv4: AWS_WEB_IDENTITY_TOKEN_FILE and AWS_ROLE_ARN are not both set")
	}
	token, err := os.ReadFile(file)
	if err != nil {
		return Credentials{}, fmt.Errorf("sigv4: %w", err)
	}
	return assumeRoleWithWebIdentity(ctx, httpClient(w.Client, 10*time.Second), Region(), role, sessionName(os.Getenv("AWS_ROLE_SESSION_NAME")), strings.TrimSpace(string(token)), 0)
}

// Regional STS endpoint and signing region, the global endpoint in us-east-1 when no region is known
func stsEndpoint(region string) (string, string) {
	if region == "" {
		return "https://sts.amazonaws.com/", "us-east-1"
	}
	return "https://sts." + region + ".amazonaws.com/", region
}

// Calls AssumeRole signed with the source credentials
func assumeRole(ctx context.Context, client *http.Client, region string, req roleRequest, source Credentials) (Credentials, error) {
	form := url.Values{"Action": {"AssumeRole"}, "Version": {stsVersion}, "RoleArn": {req.RoleARN}, "RoleSessionName": {req.SessionName}}
	if req.ExternalID != "" {
		form.Set("ExternalId", req.ExternalID)
	}
	if req.Duration > 0 {
		form.Set("DurationSeconds", strconv.Itoa(req.Duration))
	}
	if req.Serial != "" {
		form.Set("SerialNumber", req.Serial)
		form.Set("TokenCode", req.Code)
	}
	return stsCall(ctx, client, region, form, &source)
}

// Calls AssumeRoleWithWebIdentity, which STS accepts unsigned
func assumeRoleWithWebIdentity(ctx context.Context, client *http.Client, region, roleARN, session, token string, durationSeconds int) (Credentials, error) {
	form := url.Values{"Action": {"AssumeRoleWithWebIdentity"}, "Version": {stsVersion}, "RoleArn": {roleARN}, "RoleSessionName": {session}, "WebIdentityToken": {token}}
	if durationSeconds > 0 {
		form.Set("DurationSeconds", strconv.Itoa(durationSeconds))
	}
	return stsCall(ctx, client, region, form, nil)
}

// Credentials element shared by every STS assume role response
type stsCredentials struct {
	AccessKeyId     string
	SecretAccessKey string
	SessionToken    string
	Expiration      time.Time
}

type stsError struct {
	Code    string `xml:"Error>Code"`
	Message string `xml:"Error>Message"`
}

// Posts a query API form to STS and reads the credentials out of whichever result element answers
func stsCall(ctx context.Context, client *http.Client, region string, form url.Values, sign *Credentials) (Credentials, error) {
	endpoint, signRegion := stsEndpoint(region)
	body := form.Encode()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, strings.NewReader(body))
	if err != nil {
		return Credentials{}, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded; charset=utf-8")
	req.Header.Set("Accept", "application/xml")
	if sign != nil {
		if err := Sign(req, *sign, signRegion, "sts", time.Now()); err != nil {
			return Credentials{}, err
		}
	}
	resp, err := client.Do(req)
	if err != nil {
		return Credentials{}, fmt.Errorf("sigv4: sts: %w", err)
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return Credentials{}, fmt.Errorf("sigv4: sts: %w", err)
	}
	if resp.StatusCode/100 != 2 {
		var failure stsError
		if xml.Unmarshal(data, &failure) == nil && failure.Code != "" {
			return Credentials{}, fmt.Errorf("sigv4: sts %s: %s", failure.Code, failure.Message)
		}
		return Credentials{}, fmt.Errorf("sigv4: sts http %d: %s", resp.StatusCode, strings.TrimSpace(string(data)))
	}
	creds, err := stsResult(data)
	if err != nil {
		return Credentials{}, fmt.Errorf("sigv4: sts %s: %w", form.Get("Action"), err)
	}
	return creds, nil
}

// Finds the Credentials element anywhere in an STS response and decodes it
func stsResult(data []byte) (Credentials, error) {
	dec := xml.NewDecoder(strings.NewReader(string(data)))
	for {
		tok, err := dec.Token()
		if err != nil {
			return Credentials{}, fmt.Errorf("response holds no credentials: %w", err)
		}
		start, ok := tok.(xml.StartElement)
		if !ok || start.Name.Local != "Credentials" {
			continue
		}
		var c stsCredentials
		if err := dec.DecodeElement(&c, &start); err != nil {
			return Credentials{}, err
		}
		out := Credentials{AccessKey: c.AccessKeyId, SecretKey: c.SecretAccessKey, SessionToken: c.SessionToken, Expires: c.Expiration}
		if !out.valid() || out.SessionToken == "" {
			return Credentials{}, errors.New("response credentials are incomplete")
		}
		return out, nil
	}
}
