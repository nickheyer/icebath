package transport

import (
	"cmp"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"net/http"
	"net/url"
	"slices"
	"strings"

	"github.com/nickheyer/icebath/internal/credentials/aws"
	"github.com/nickheyer/icebath/internal/provider"
	"github.com/nickheyer/icebath/internal/transport"
)

// Where a Messages request is served: Anthropic's API or an endpoint speaking it, Claude on Bedrock, or Claude on Vertex AI
const (
	direct    = "direct"
	onBedrock = "bedrock"
	onVertex  = "vertex"
)

// Versions of the API the cloud platforms take in the body
const (
	apiVersion     = "2023-06-01"
	bedrockVersion = "bedrock-2023-05-31"
	vertexVersion  = "vertex-2023-10-16"
)

// Betas of the requests to Anthropic's API and the endpoints that speak it
const (
	interleavedBeta = "interleaved-thinking-2025-05-14"
	streamingBeta   = "fine-grained-tool-streaming-2025-05-14"
	longContextBeta = "context-1m-2025-08-07"
	fastBeta        = "fast-mode-2026-02-01"
	fallbackBeta    = "server-side-fallback-2026-07-01"
)

// Field of a Bedrock reply that says a guardrail replaced it
const guardrailAction = "amazon-bedrock-guardrailAction"

// Platform the route's requests are served on
func platform(route provider.Route) string {
	switch {
	case route.Auth == provider.AuthAWSSigV4 || bedrockRuntime(route.BaseURL):
		return onBedrock
	case route.Auth == provider.AuthGCPOAuth || vertexEndpoint(route.BaseURL):
		return onVertex
	}
	return direct
}

// Betas the endpoint takes: interleaved thinking and tool streaming, the 1M window on Azure and Bedrock unless it was refused, neither of
// the last two on MiniMax
func betas(route provider.Route, where string, drop1M bool) []string {
	out := []string{interleavedBeta, streamingBeta}
	switch {
	case where == onBedrock:
		if !drop1M {
			out = append(out, longContextBeta)
		}
		return out
	case where == onVertex:
		return out
	case strings.Contains(strings.ToLower(route.BaseURL), "azure.com") && !drop1M:
		out = append(out, longContextBeta)
	}
	if minimax(route.BaseURL) {
		return slices.DeleteFunc(out, func(b string) bool { return b == streamingBeta || b == longContextBeta })
	}
	return out
}

// Address and body of a Messages request on the platform: the API's /v1/messages; Bedrock's InvokeModel of the model, the version and
// betas in the body; Vertex's rawPredict of the model under the project and region its address names
func (p *prepared) target(stream bool) (string, []byte, error) {
	body := maps.Clone(p.body)
	switch p.where {
	case onBedrock:
		body["anthropic_version"] = bedrockVersion
		if len(p.betas) > 0 {
			body["anthropic_beta"] = p.betas
		}
		delete(body, "model")
		delete(body, "stream")
		base := strings.TrimSpace(p.bedrockBase)
		base = cmp.Or(base, p.route.BaseURL)
		operation := "invoke"
		if stream {
			operation = "invoke-with-response-stream"
		}
		data, err := json.Marshal(body)
		return strings.TrimRight(base, "/") + aws.ModelPath(p.model, operation), data, err
	case onVertex:
		origin, project, region, err := vertexTarget(p.route.BaseURL)
		if err != nil {
			return "", nil, err
		}
		body["anthropic_version"] = vertexVersion
		delete(body, "model")
		method := "rawPredict"
		if stream {
			method = "streamRawPredict"
			body["stream"] = true
		} else {
			delete(body, "stream")
		}
		data, err := json.Marshal(body)
		return fmt.Sprintf("%s/v1/projects/%s/locations/%s/publishers/anthropic/models/%s:%s", origin, project, region, p.model, method), data, err
	}
	if stream {
		body["stream"] = true
	} else {
		delete(body, "stream")
	}
	base := normalized(p.route.BaseURL)
	base = cmp.Or(base, "https://api.anthropic.com")
	target, err := url.Parse(base)
	if err != nil || target.Scheme == "" || target.Host == "" {
		return "", nil, fmt.Errorf("The base URL must be absolute, as https://host: %s", p.route.BaseURL)
	}
	target.Path = strings.TrimRight(target.Path, "/") + "/v1/messages"
	if azureAnthropic(base) && !strings.Contains(base, "api-version") {
		query := target.Query()
		query.Set("api-version", "2025-04-15")
		target.RawQuery = query.Encode()
	}
	data, err := json.Marshal(body)
	return target.String(), data, err
}

// Origin, project and region of a Vertex address of the form projects/P/locations/R, the global region and its origin when it names none
func vertexTarget(base string) (string, string, string, error) {
	u, err := url.Parse(strings.TrimSpace(base))
	if err != nil || u.Scheme == "" || u.Host == "" {
		return "", "", "", fmt.Errorf("The Vertex AI address must be absolute: %s", base)
	}
	project, region := "", ""
	parts := strings.Split(strings.Trim(u.Path, "/"), "/")
	for i := 0; i+1 < len(parts); i++ {
		switch parts[i] {
		case "projects":
			project = parts[i+1]
		case "locations":
			region = parts[i+1]
		}
	}
	if project == "" {
		return "", "", "", errors.New("No Vertex AI project id resolves; set the provider's project or VERTEX_PROJECT_ID, or use credentials that embed one.")
	}
	region = cmp.Or(region, "global")
	origin := u.Scheme + "://" + u.Host
	if !strings.Contains(u.Host, "aiplatform") {
		switch region {
		case "global":
			origin = "https://aiplatform.googleapis.com"
		case "us", "eu":
			origin = "https://aiplatform." + region + ".rep.googleapis.com"
		default:
			origin = "https://" + region + "-aiplatform.googleapis.com"
		}
	}
	return origin, project, region, nil
}

// Next Messages event of a Bedrock stream, the Anthropic event inside each chunk frame as a frame of its own; an exception frame as the
// provider's error
func bedrockEvents(body io.Reader, header http.Header) func() (*transport.Frame, error) {
	stream := transport.NewEventStream(body)
	return func() (*transport.Frame, error) {
		for {
			m, err := stream.Next()
			if err != nil {
				return nil, err
			}
			if failure := m.Exception(header); failure != nil {
				return nil, failure
			}
			if m.Header(":event-type") != "chunk" {
				continue
			}
			var chunk struct {
				Bytes *string `json:"bytes"`
			}
			if json.Unmarshal(m.Payload, &chunk) != nil || chunk.Bytes == nil {
				return nil, transport.FrameError(string(m.Payload), header)
			}
			data, err := base64.StdEncoding.DecodeString(*chunk.Bytes)
			if err != nil {
				return nil, fmt.Errorf("A Bedrock stream chunk isn't base64: %w", err)
			}
			var payload map[string]any
			if json.Unmarshal(data, &payload) != nil {
				return nil, transport.FrameError(string(data), header)
			}
			event, _ := payload["type"].(string)
			if event == "ping" {
				continue
			}
			return &transport.Frame{Event: event, Data: string(data), Fields: map[string]string{}}, nil
		}
	}
}

// Whether the platform can't stream the request: a stream not supported, events out of order, or IAM denying Bedrock's streaming call
func streamless(err error, where string) bool {
	if err == nil {
		return false
	}
	if _, ok := errors.AsType[*orderError](err); ok {
		return true
	}
	text := strings.ToLower(err.Error())
	if failure, ok := errors.AsType[*provider.APIError](err); ok && failure.Err != nil {
		text += " " + strings.ToLower(failure.Err.Error())
	}
	if strings.Contains(text, "stream") && strings.Contains(text, "not supported") {
		return true
	}
	return where == onBedrock && strings.Contains(text, "invokemodelwithresponsestream") &&
		provider.Contains(text, "accessdeniedexception", "unauthorizedexception", "not authorized", "accessdenied")
}
