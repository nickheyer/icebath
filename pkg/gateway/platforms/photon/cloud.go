package photon

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"
)

// Spectrum cloud defaults the SDK resolves when no override is set
const (
	defaultSpectrumHost    = "https://spectrum.photon.codes"
	defaultSharedAddress   = "imessage.spectrum.photon.codes:443"
	dedicatedAddressSuffix = ".imsg.photon.codes:443"
	cloudRequestTimeout    = 30 * time.Second
)

// Base URL of the Spectrum cloud API, SPECTRUM_CLOUD_URL first then PHOTON_SPECTRUM_HOST
func spectrumHost() string {
	for _, key := range []string{"SPECTRUM_CLOUD_URL", "PHOTON_SPECTRUM_HOST"} {
		if v := strings.TrimSpace(os.Getenv(key)); v != "" {
			return strings.TrimRight(v, "/")
		}
	}
	return defaultSpectrumHost
}

// Shared line gRPC address, SPECTRUM_IMESSAGE_ADDRESS overriding the Photon default
func sharedLineAddress() string {
	if v := strings.TrimSpace(os.Getenv("SPECTRUM_IMESSAGE_ADDRESS")); v != "" {
		return v
	}
	return defaultSharedAddress
}

func basicAuth(projectID, projectSecret string) string {
	return "Basic " + base64.StdEncoding.EncodeToString([]byte(projectID+":"+projectSecret))
}

// Failed Spectrum cloud call with the HTTP status and the server error code
type cloudError struct {
	Status  int
	Code    string
	Message string
}

func (e *cloudError) Error() string {
	return fmt.Sprintf("Spectrum cloud request failed (HTTP %d, %s): %s", e.Status, e.Code, e.Message)
}

// Spectrum cloud client authenticated with the project id and secret
type cloudClient struct {
	host      string
	projectID string
	secret    string
	http      *http.Client
}

func newCloudClient(projectID, secret string) *cloudClient {
	return &cloudClient{host: spectrumHost(), projectID: projectID, secret: secret, http: &http.Client{Timeout: cloudRequestTimeout}}
}

// Decodes the data field of a succeed envelope into out
func (c *cloudClient) do(ctx context.Context, method, path string, out any) error {
	req, err := http.NewRequestWithContext(ctx, method, c.host+path, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", basicAuth(c.projectID, c.secret))
	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		var parsed struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		}
		if json.Unmarshal(body, &parsed) == nil && (parsed.Code != "" || parsed.Message != "") {
			return &cloudError{Status: resp.StatusCode, Code: parsed.Code, Message: parsed.Message}
		}
		text := strings.TrimSpace(string(body))
		if text == "" {
			text = http.StatusText(resp.StatusCode)
		}
		return &cloudError{Status: resp.StatusCode, Code: "UNKNOWN", Message: text}
	}
	var envelope struct {
		Succeed bool            `json:"succeed"`
		Data    json.RawMessage `json:"data"`
	}
	if err := json.Unmarshal(body, &envelope); err != nil {
		return fmt.Errorf("Spectrum cloud returned an unreadable response: %w", err)
	}
	if !envelope.Succeed {
		return &cloudError{Status: resp.StatusCode, Code: "UNKNOWN", Message: "Server returned succeed=false"}
	}
	if out == nil {
		return nil
	}
	return json.Unmarshal(envelope.Data, out)
}

// Project record whose profile carries the imessageSynced contact sharing switch
type projectData struct {
	Profile map[string]any `json:"profile"`
}

func (p *projectData) profileSynced() bool {
	if p == nil {
		return false
	}
	synced, _ := p.Profile["imessageSynced"].(bool)
	return synced
}

func (c *cloudClient) getProject(ctx context.Context) (*projectData, error) {
	var p projectData
	if err := c.do(ctx, http.MethodGet, "/projects/"+c.projectID+"/", &p); err != nil {
		return nil, err
	}
	return &p, nil
}

// iMessage bearer tokens, one shared token or one per dedicated instance with its phone number
type tokenData struct {
	Type      string             `json:"type"`
	Token     string             `json:"token"`
	ExpiresIn float64            `json:"expiresIn"`
	Auth      map[string]string  `json:"auth"`
	Numbers   map[string]*string `json:"numbers"`
}

func (c *cloudClient) issueTokens(ctx context.Context) (*tokenData, error) {
	var t tokenData
	if err := c.do(ctx, http.MethodPost, "/projects/"+c.projectID+"/imessage/tokens", &t); err != nil {
		return nil, err
	}
	switch t.Type {
	case "shared":
		if t.Token == "" {
			return nil, fmt.Errorf("Spectrum cloud issued a shared iMessage token payload without a token")
		}
	case "dedicated":
		if t.Auth == nil {
			return nil, fmt.Errorf("Spectrum cloud issued a dedicated iMessage token payload without auth")
		}
	default:
		return nil, fmt.Errorf("Spectrum cloud issued an iMessage token payload of unknown type %q", t.Type)
	}
	return &t, nil
}
