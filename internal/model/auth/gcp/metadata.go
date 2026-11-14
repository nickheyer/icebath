package gcp

import (
	"context"
	"encoding/json"
	"log/slog"
	"mime"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/nickheyer/icebath/internal/httpclient"
)

const (
	metadataFlavor   = "Metadata-Flavor"
	metadataGoogle   = "Google"
	metadataPings    = 3
	metadataAttempts = 5
)

func metadataRoot() string {
	host := os.Getenv("GCE_METADATA_HOST")
	if host == "" {
		host = os.Getenv("GCE_METADATA_ROOT")
	}
	if host == "" {
		host = "metadata.google.internal"
	}
	return "http://" + host + "/computeMetadata/v1/"
}

func metadataAddress() string {
	ip := os.Getenv("GCE_METADATA_IP")
	if ip == "" {
		ip = "169.254.169.254"
	}
	return "http://" + ip
}

func metadataTimeout() time.Duration {
	if seconds, err := strconv.Atoi(os.Getenv("GCE_METADATA_TIMEOUT")); err == nil && seconds > 0 {
		return time.Duration(seconds) * time.Second
	}
	return 3 * time.Second
}

func (c *client) metadataPing(ctx context.Context) bool {
	wait := time.Second
	for attempt := 1; attempt <= metadataPings; attempt++ {
		if attempt > 1 {
			if sleep(ctx, jittered(wait)) != nil {
				return false
			}
			wait *= 2
		}
		status, header, _, err := c.send(ctx, c.direct, metadataTimeout(), http.MethodGet, metadataAddress(), http.Header{metadataFlavor: {metadataGoogle}}, nil)
		if err != nil {
			slog.Warn("Compute Engine Metadata server unavailable", "attempt", attempt, "of", metadataPings, "reason", httpclient.Message(err))
			continue
		}
		return status == http.StatusOK && header.Get(metadataFlavor) == metadataGoogle
	}
	return false
}

func (c *client) onComputeEngine(ctx context.Context) bool {
	return c.metadataPing(ctx) || computeProductName()
}

func (c *client) metadataGet(ctx context.Context, hc *http.Client, path string, query url.Values) (any, error) {
	target := metadataRoot() + path
	if len(query) > 0 {
		target += "?" + query.Encode()
	}
	wait := time.Second
	reason := ""
	for attempt := 1; attempt <= metadataAttempts; attempt++ {
		if attempt > 1 {
			if err := sleep(ctx, jittered(wait)); err != nil {
				return nil, err
			}
			wait *= 2
		}
		status, header, data, err := c.send(ctx, hc, metadataTimeout(), http.MethodGet, target, http.Header{metadataFlavor: {metadataGoogle}}, nil)
		if err != nil {
			reason = httpclient.Message(err)
			slog.Warn("Compute Engine Metadata server unavailable", "attempt", attempt, "of", metadataAttempts, "reason", reason)
			continue
		}
		switch status {
		case http.StatusInternalServerError, http.StatusServiceUnavailable, http.StatusRequestTimeout, http.StatusTooManyRequests:
			reason = "HTTP " + strconv.Itoa(status)
			slog.Warn("Compute Engine Metadata server unavailable", "attempt", attempt, "of", metadataAttempts, "status", status)
			continue
		}
		if status != http.StatusOK {
			return nil, &Error{Kind: kindCompute, Status: status, Message: "failed to retrieve " + path + " from the Google Compute Engine metadata service"}
		}
		if kind, _, _ := mime.ParseMediaType(header.Get("Content-Type")); kind == jsonType {
			var value any
			if err := json.Unmarshal(data, &value); err != nil {
				return nil, &Error{Kind: kindCompute, Message: "received invalid JSON from the Google Compute Engine metadata service"}
			}
			return value, nil
		}
		return string(data), nil
	}
	return nil, &Error{Kind: kindCompute, Retryable: true, Message: "failed to retrieve " + path + " from the Google Compute Engine metadata service, the server is unavailable: " + reason}
}

func (c *client) computeCredentials(scopes []string) *credentials {
	creds := &credentials{kind: kindCompute}
	account := "default"
	creds.mint = func(ctx context.Context) (string, time.Time, error) {
		requested := scopes
		info, err := c.metadataGet(ctx, c.api, "instance/service-accounts/"+account+"/", url.Values{"recursive": {"true"}})
		if err != nil {
			return "", time.Time{}, err
		}
		if fields, ok := info.(map[string]any); ok {
			if email, ok := fields["email"].(string); ok && email != "" {
				account = email
			}
			if scopes == nil {
				if listed, ok := fields["scopes"].([]any); ok {
					scopes = []string{}
					for _, item := range listed {
						if scope, ok := item.(string); ok {
							scopes = append(scopes, scope)
						}
					}
				}
			}
		}
		query := url.Values{}
		if len(requested) > 0 {
			query.Set("scopes", strings.Join(requested, ","))
		}
		reply, err := c.metadataGet(ctx, c.api, "instance/service-accounts/default/token", query)
		if err != nil {
			return "", time.Time{}, err
		}
		fields, _ := reply.(map[string]any)
		token, _ := fields["access_token"].(string)
		seconds, ok := fields["expires_in"].(float64)
		if token == "" || !ok {
			return "", time.Time{}, &Error{Kind: kindCompute, Message: "metadata service reply carried no access token and lifetime"}
		}
		return token, time.Now().UTC().Add(time.Duration(seconds * float64(time.Second))), nil
	}
	creds.allowed = c.allowedFor(kindCompute, func(ctx context.Context) (string, error) {
		if account == "default" {
			info, err := c.metadataGet(ctx, c.api, "instance/service-accounts/default/", url.Values{"recursive": {"true"}})
			if err != nil {
				return "", &Error{Kind: kindCompute, Message: "Failed to get service account email for trust boundary lookup: " + err.Error(), Err: errors.Join(errBoundaryRefresh, err)}
			}
			fields, _ := info.(map[string]any)
			email, _ := fields["email"].(string)
			if email == "" {
				return "", &Error{Kind: kindCompute, Message: "Unexpected response from metadata server: service account info is missing 'email' field.", Err: errBoundaryRefresh}
			}
			account = email
		}
		return fmt.Sprintf(serviceAccountBoundary, defaultUniverse, account), nil
	})
	universe := ""
	creds.universeLookup = func(ctx context.Context) (string, error) {
		if universe != "" {
			return universe, nil
		}
		value, err := c.metadataGet(ctx, c.api, "universe/universe-domain", nil)
		var failed *Error
		switch {
		case errors.As(err, &failed) && failed.Status == http.StatusNotFound:
			universe = defaultUniverse
		case err != nil:
			return "", err
		default:
			if universe, _ = value.(string); universe == "" {
				universe = defaultUniverse
			}
		}
		return universe, nil
	}
	return creds
}

func (c *client) computeDefault(ctx context.Context, scopes []string) (*credentials, string, bool) {
	if !c.onComputeEngine(ctx) {
		slog.Warn("Authentication failed using Compute Engine authentication due to unavailable metadata server.")
		return nil, "", false
	}
	project := ""
	if value, err := c.metadataGet(ctx, c.direct, "project/project-id", nil); err == nil {
		project, _ = value.(string)
	}
	return c.computeCredentials(scopes), project, true
}
