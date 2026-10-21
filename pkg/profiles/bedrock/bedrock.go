// Amazon Bedrock endpoint profile with Signature Version 4 auth and model listing
package bedrock

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"strings"

	"github.com/nickheyer/icebath/pkg/profiles"
	"github.com/nickheyer/icebath/pkg/protocol"
	"github.com/nickheyer/icebath/pkg/provider"
	"github.com/nickheyer/icebath/pkg/provider/sigv4"

	_ "github.com/nickheyer/icebath/pkg/protocol/bedrock-converse-stream"
)

const (
	Vendor        = "bedrock"
	Name          = "bedrock"
	DefaultRegion = "us-east-1"
	// Environment variable overriding the base URL
	BaseURLEnv = "BEDROCK_BASE_URL"
	// Model used when none is chosen
	DefaultModel = "us.anthropic.claude-sonnet-4-5-20250929-v1:0"
	// Auth kind signing requests with AWS credentials
	AuthSigV4 profiles.AuthKind = "aws-sigv4"
)

// Base URL of the runtime API in a region
func Runtime(region string) string {
	return "https://bedrock-runtime." + region + ".amazonaws.com"
}

// Base URL of the control plane API in a region
func Control(region string) string {
	return "https://bedrock." + region + ".amazonaws.com"
}

// Profile for the runtime API in a region, signing with the AWS default credential chain
func Profile(region string) profiles.Profile {
	return profiles.Profile{Name: Name, Vendor: Vendor, Protocol: protocol.BedrockConverseStream, BaseURL: Runtime(region), Auth: AuthSigV4, Options: map[string]string{"region": region}, Models: []string{DefaultModel}, BaseURLEnv: BaseURLEnv}
}

func init() {
	profiles.RegisterAuth(AuthSigV4, Auth)
	profiles.Register(fromEnv(sigv4.Region()))
	profiles.RegisterVendor(profiles.Vendor{Name: Vendor, Models: Models})
}

// Profile for the region named by the environment, falling back to the default region when the value does not form a usable host
func fromEnv(region string) profiles.Profile {
	if region == "" {
		return Profile(DefaultRegion)
	}
	p := Profile(region)
	if err := p.Validate(); err != nil || regionOf(p.BaseURL) != region {
		return Profile(DefaultRegion)
	}
	return p
}

// Builds the signer from the profile options, the base URL and a key holding access, secret and optional token joined by colons
func Auth(p profiles.Profile, key string) (provider.Auth, error) {
	region := p.Options["region"]
	if region == "" {
		region = regionOf(p.BaseURL)
	}
	if region == "" {
		region = sigv4.Region()
	}
	if region == "" {
		return nil, fmt.Errorf("profile %s: region is required, set options.region or AWS_REGION", p.Name)
	}
	service := p.Options["service"]
	if service == "" {
		service = "bedrock"
	}
	var source sigv4.Source
	switch {
	case p.Options["profile"] != "":
		source = sigv4.Profile{Name: p.Options["profile"]}
	case key != "":
		parts := strings.SplitN(key, ":", 3)
		if len(parts) < 2 {
			return nil, fmt.Errorf("profile %s: key must look like ACCESS:SECRET or ACCESS:SECRET:TOKEN", p.Name)
		}
		creds := sigv4.Credentials{AccessKey: parts[0], SecretKey: parts[1]}
		if len(parts) == 3 {
			creds.SessionToken = parts[2]
		}
		source = sigv4.Static(creds)
	default:
		source = sigv4.Default()
	}
	return &sigv4.Auth{Region: region, Service: service, Source: source}, nil
}

// Reports whether the base URL points at an AWS host rather than an emulator or proxy
func isAWS(baseURL string) bool {
	u, err := url.Parse(baseURL)
	if err != nil {
		return false
	}
	host := strings.ToLower(u.Hostname())
	return host == "amazonaws.com" || strings.HasSuffix(host, ".amazonaws.com")
}

// Region the provider signs for, else the one in its host name, else the environment
func regionFor(p *provider.Provider) string {
	if a, ok := p.Auth.(*sigv4.Auth); ok && a.Region != "" {
		return a.Region
	}
	if region := regionOf(p.BaseURL); region != "" {
		return region
	}
	return sigv4.Region()
}

// Reads the region out of a regional AWS host name
func regionOf(baseURL string) string {
	u, err := url.Parse(baseURL)
	if err != nil {
		return ""
	}
	parts := strings.Split(u.Hostname(), ".")
	if len(parts) >= 4 && (parts[0] == "bedrock-runtime" || parts[0] == "bedrock") && strings.HasSuffix(u.Hostname(), ".amazonaws.com") {
		return parts[1]
	}
	return ""
}

type summaries struct {
	ModelSummaries []struct {
		ModelID                    string   `json:"modelId"`
		ModelName                  string   `json:"modelName"`
		ProviderName               string   `json:"providerName"`
		OutputModalities           []string `json:"outputModalities"`
		ResponseStreamingSupported bool     `json:"responseStreamingSupported"`
	} `json:"modelSummaries"`
}

type inferenceProfiles struct {
	Summaries []struct {
		ID     string `json:"inferenceProfileId"`
		Name   string `json:"inferenceProfileName"`
		Status string `json:"status"`
		Type   string `json:"type"`
	} `json:"inferenceProfileSummaries"`
	NextToken string `json:"nextToken"`
}

// Control plane provider for the signing region on AWS hosts, the same host elsewhere
func controlPlane(p *provider.Provider) (*provider.Provider, error) {
	if p == nil || p.Protocol == nil {
		return nil, errors.New("bedrock: provider is required")
	}
	control := *p
	if isAWS(p.BaseURL) {
		region := regionFor(p)
		if region == "" {
			return nil, fmt.Errorf("bedrock: no region known for %s, set options.region or AWS_REGION", p.BaseURL)
		}
		control.BaseURL = Control(region)
	}
	return &control, nil
}

// Lists the active cross region inference profiles, whose ids route a model id through several regions
func InferenceProfiles(ctx context.Context, p *provider.Provider) ([]profiles.Model, error) {
	control, err := controlPlane(p)
	if err != nil {
		return nil, err
	}
	var out []profiles.Model
	token := ""
	for {
		q := url.Values{"maxResults": {"1000"}}
		if token != "" {
			q.Set("nextToken", token)
		}
		var page inferenceProfiles
		if err := profiles.GetJSON(ctx, control, "/inference-profiles", q, &page); err != nil {
			return nil, err
		}
		for _, s := range page.Summaries {
			if s.Status != "" && s.Status != "ACTIVE" {
				continue
			}
			out = append(out, profiles.Model{ID: s.ID, Name: strings.TrimSpace(s.Name)})
		}
		if page.NextToken == "" || page.NextToken == token {
			return out, nil
		}
		token = page.NextToken
	}
}

// Lists streaming text models and inference profiles from the control plane of the signing region on AWS hosts, same host elsewhere
func Models(ctx context.Context, p *provider.Provider) ([]profiles.Model, error) {
	control, err := controlPlane(p)
	if err != nil {
		return nil, err
	}
	var list summaries
	if err := profiles.GetJSON(ctx, control, "/foundation-models", nil, &list); err != nil {
		return nil, err
	}
	var out []profiles.Model
	for _, m := range list.ModelSummaries {
		if !m.ResponseStreamingSupported || !contains(m.OutputModalities, "TEXT") {
			continue
		}
		out = append(out, profiles.Model{ID: m.ModelID, Name: strings.TrimSpace(m.ProviderName + " " + m.ModelName)})
	}
	inference, err := InferenceProfiles(ctx, control)
	if err != nil {
		return nil, err
	}
	return append(out, inference...), nil
}

func contains(list []string, want string) bool {
	for _, s := range list {
		if s == want {
			return true
		}
	}
	return false
}
