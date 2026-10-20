package sigv4

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"
)

// Credential document the container and instance metadata endpoints return
type remoteCredentials struct {
	Code            string
	AccessKeyId     string
	SecretAccessKey string
	Token           string
	Expiration      time.Time
}

func (r remoteCredentials) credentials() (Credentials, error) {
	c := Credentials{AccessKey: r.AccessKeyId, SecretKey: r.SecretAccessKey, SessionToken: r.Token, Expires: r.Expiration}
	if !c.valid() {
		return Credentials{}, errors.New("document lacks a key pair")
	}
	return c, nil
}

// Credentials from the endpoint AWS_CONTAINER_CREDENTIALS_RELATIVE_URI or AWS_CONTAINER_CREDENTIALS_FULL_URI names
type Container struct {
	// A five second client when nil
	Client *http.Client
}

func (c Container) Credentials(ctx context.Context) (Credentials, error) {
	target := os.Getenv("AWS_CONTAINER_CREDENTIALS_FULL_URI")
	if rel := os.Getenv("AWS_CONTAINER_CREDENTIALS_RELATIVE_URI"); target == "" && rel != "" {
		target = "http://169.254.170.2" + rel
	}
	if target == "" {
		return Credentials{}, errors.New("sigv4: AWS_CONTAINER_CREDENTIALS_RELATIVE_URI and AWS_CONTAINER_CREDENTIALS_FULL_URI are not set")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
	if err != nil {
		return Credentials{}, err
	}
	token := os.Getenv("AWS_CONTAINER_AUTHORIZATION_TOKEN")
	if file := os.Getenv("AWS_CONTAINER_AUTHORIZATION_TOKEN_FILE"); file != "" {
		data, err := os.ReadFile(file)
		if err != nil {
			return Credentials{}, fmt.Errorf("sigv4: container: %w", err)
		}
		token = strings.TrimSpace(string(data))
	}
	if token != "" {
		req.Header.Set("Authorization", token)
	}
	resp, err := httpClient(c.Client, 5*time.Second).Do(req)
	if err != nil {
		return Credentials{}, fmt.Errorf("sigv4: container: %w", err)
	}
	var doc remoteCredentials
	if err := readJSON(resp, &doc); err != nil {
		return Credentials{}, fmt.Errorf("sigv4: container: %w", err)
	}
	creds, err := doc.credentials()
	if err != nil {
		return Credentials{}, fmt.Errorf("sigv4: container: %w", err)
	}
	return creds, nil
}

// Credentials of the instance role from the EC2 metadata service, version 2 with a session token
type IMDS struct {
	// A one second client when nil
	Client *http.Client
}

// Lifetime asked for the metadata session token
const imdsTokenTTL = "21600"

func (i IMDS) Credentials(ctx context.Context) (Credentials, error) {
	if strings.EqualFold(os.Getenv("AWS_EC2_METADATA_DISABLED"), "true") {
		return Credentials{}, errors.New("sigv4: instance metadata is disabled by AWS_EC2_METADATA_DISABLED")
	}
	base := strings.TrimRight(os.Getenv("AWS_EC2_METADATA_SERVICE_ENDPOINT"), "/")
	if base == "" {
		base = "http://169.254.169.254"
	}
	client := httpClient(i.Client, time.Second)
	token, err := i.text(ctx, client, http.MethodPut, base+"/latest/api/token", http.Header{"X-Aws-Ec2-Metadata-Token-Ttl-Seconds": {imdsTokenTTL}})
	if err != nil {
		return Credentials{}, fmt.Errorf("sigv4: imds token: %w", err)
	}
	auth := http.Header{"X-Aws-Ec2-Metadata-Token": {token}}
	roles, err := i.text(ctx, client, http.MethodGet, base+"/latest/meta-data/iam/security-credentials/", auth)
	if err != nil {
		return Credentials{}, fmt.Errorf("sigv4: imds role: %w", err)
	}
	role, _, _ := strings.Cut(strings.TrimSpace(roles), "\n")
	if role == "" {
		return Credentials{}, errors.New("sigv4: imds names no instance role")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, base+"/latest/meta-data/iam/security-credentials/"+role, nil)
	if err != nil {
		return Credentials{}, err
	}
	req.Header = auth
	resp, err := client.Do(req)
	if err != nil {
		return Credentials{}, fmt.Errorf("sigv4: imds: %w", err)
	}
	var doc remoteCredentials
	if err := readJSON(resp, &doc); err != nil {
		return Credentials{}, fmt.Errorf("sigv4: imds: %w", err)
	}
	if doc.Code != "" && doc.Code != "Success" {
		return Credentials{}, fmt.Errorf("sigv4: imds role %s: %s", role, doc.Code)
	}
	creds, err := doc.credentials()
	if err != nil {
		return Credentials{}, fmt.Errorf("sigv4: imds: %w", err)
	}
	return creds, nil
}

// Issues one metadata request and returns its body as text
func (IMDS) text(ctx context.Context, client *http.Client, method, target string, header http.Header) (string, error) {
	req, err := http.NewRequestWithContext(ctx, method, target, nil)
	if err != nil {
		return "", err
	}
	req.Header = header
	resp, err := client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 1<<16))
	if err != nil {
		return "", err
	}
	if resp.StatusCode/100 != 2 {
		return "", fmt.Errorf("http %d: %s", resp.StatusCode, strings.TrimSpace(string(data)))
	}
	return string(data), nil
}
