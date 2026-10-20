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
	return profiles.Profile{Name: Name, Vendor: Vendor, Protocol: protocol.BedrockConverseStream, BaseURL: Runtime(region), Auth: AuthSigV4, Options: map[string]string{"region": region}}
}

func init() {
	region := sigv4.Region()
	if region == "" {
		region = DefaultRegion
	}
	profiles.RegisterAuth(AuthSigV4, Auth)
	profiles.Register(Profile(region))
	profiles.RegisterVendor(profiles.Vendor{Name: Vendor, Models: Models})
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

// Lists streaming text models from the control plane of the signing region on AWS hosts, same host elsewhere
func Models(ctx context.Context, p *provider.Provider) ([]profiles.Model, error) {
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
	var list summaries
	if err := profiles.GetJSON(ctx, &control, "/foundation-models", nil, &list); err != nil {
		return nil, err
	}
	var out []profiles.Model
	for _, m := range list.ModelSummaries {
		if !m.ResponseStreamingSupported || !contains(m.OutputModalities, "TEXT") {
			continue
		}
		out = append(out, profiles.Model{ID: m.ModelID, Name: strings.TrimSpace(m.ProviderName + " " + m.ModelName)})
	}
	return out, nil
}

func contains(list []string, want string) bool {
	for _, s := range list {
		if s == want {
			return true
		}
	}
	return false
}
