package aws

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Regions a Bedrock runtime or Mantle host names
var (
	runtimeRegion = regexp.MustCompile(`bedrock-runtime(?:-fips)?\.([a-z0-9-]+)\.`)
	mantleRegion  = regexp.MustCompile(`(?i)^bedrock-mantle\.([a-z0-9-]+)\.api\.aws$`)
)

// Region a Bedrock runtime or Mantle address names, empty for any other
func EndpointRegion(address string) string {
	host := strings.ToLower(address)
	if i := strings.Index(host, "://"); i >= 0 {
		host = host[i+3:]
	}
	host, _, _ = strings.Cut(host, "/")
	host, _, _ = strings.Cut(host, ":")
	if m := runtimeRegion.FindStringSubmatch(host + "."); m != nil {
		return m[1]
	}
	if m := mantleRegion.FindStringSubmatch(host); m != nil {
		return m[1]
	}
	return ""
}

// Address of the Bedrock runtime of a region
func RuntimeURL(region string) string {
	return "https://bedrock-runtime." + region + ".amazonaws.com"
}

// Address of the Bedrock control plane of a region
func ControlURL(region string) string {
	return "https://bedrock." + region + ".amazonaws.com"
}

// Address of the OpenAI compatible Responses API Bedrock Mantle serves in a region
func MantleURL(region string) string {
	return "https://bedrock-mantle." + region + ".api.aws/openai/v1"
}

// Runtime path of an operation on a model, the model id percent encoded as the SDKs do it, every reserved byte but the colon
func ModelPath(model, operation string) string {
	var b strings.Builder
	for i := 0; i < len(model); i++ {
		c := model[i]
		switch {
		case c >= 'A' && c <= 'Z', c >= 'a' && c <= 'z', c >= '0' && c <= '9', c == '-', c == '_', c == '.', c == '~', c == ':':
			b.WriteByte(c)
		default:
			fmt.Fprintf(&b, "%%%02X", c)
		}
	}
	return "/model/" + b.String() + "/" + operation
}

// Code and message of an AWS JSON error answer: the code from x-amzn-ErrorType, else __type, else code, without its namespace
func ErrorOf(header http.Header, status int, body []byte) (string, string) {
	var payload map[string]any
	_ = json.Unmarshal(body, &payload)
	message, _ := payload["message"].(string)
	if message == "" {
		message, _ = payload["Message"].(string)
	}
	code := header.Get("x-amzn-ErrorType")
	for _, key := range []string{"__type", "code", "Code"} {
		if code != "" {
			break
		}
		code, _ = payload[key].(string)
	}
	if code == "" {
		code = strconv.Itoa(status)
	}
	code, _, _ = strings.Cut(code, ":")
	if i := strings.LastIndexByte(code, '#'); i >= 0 {
		code = code[i+1:]
	}
	return strings.TrimSpace(code), message
}

// Calls a JSON operation of an AWS service at the address, the request signed by sign, else carrying the bearer key, the answer decoded
// into out; an answer that isn't a success as a ServiceError
func Call(ctx context.Context, client *http.Client, operation, method, address string, in any, sign func(*http.Request, []byte) error, bearer string,
	out any) error {
	var body []byte
	if in != nil {
		data, err := json.Marshal(in)
		if err != nil {
			return err
		}
		body = data
	}
	request, err := http.NewRequestWithContext(ctx, method, address, bytes.NewReader(body))
	if err != nil {
		return err
	}
	if in != nil {
		request.Header.Set("Content-Type", "application/json")
	}
	request.Header.Set("Accept", "application/json")
	switch {
	case sign != nil:
		if err := sign(request, body); err != nil {
			return err
		}
	case bearer != "":
		request.Header.Set("Authorization", "Bearer "+bearer)
	}
	response, err := client.Do(request)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	data, err := io.ReadAll(io.LimitReader(response.Body, 16<<20))
	if err != nil {
		return err
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		code, message := ErrorOf(response.Header, response.StatusCode, data)
		return &ServiceError{Operation: operation, StatusCode: response.StatusCode, Code: code, Message: message}
	}
	if out == nil || len(bytes.TrimSpace(data)) == 0 {
		return nil
	}
	return json.Unmarshal(data, out)
}

var (
	applicationProfile = regexp.MustCompile(`:application-inference-profile/`)
	arnRegion          = regexp.MustCompile(`(?i)^arn:[^:]+:bedrock:([a-z0-9-]+):`)
)

// Whether the model id is an application inference profile, which wraps the model it serves
func ApplicationProfile(model string) bool {
	return applicationProfile.MatchString(model)
}

// Inference profiles resolved to the models they wrap, each looked up once
type Profiles struct {
	mu       sync.Mutex
	resolved map[string]string
}

// Model an application inference profile wraps, looked up with the control plane of its region; the profile itself when the lookup fails
// or the model isn't one
func (p *Profiles) Resolve(ctx context.Context, client *http.Client, model, region string, sign func(*http.Request, []byte) error, bearer string) string {
	if !ApplicationProfile(model) {
		return model
	}
	p.mu.Lock()
	if p.resolved == nil {
		p.resolved = map[string]string{}
	}
	if found, ok := p.resolved[model]; ok {
		p.mu.Unlock()
		return found
	}
	p.mu.Unlock()
	if m := arnRegion.FindStringSubmatch(model); m != nil {
		region = m[1]
	}
	var answer struct {
		Models []struct {
			ModelArn string `json:"modelArn"`
		} `json:"models"`
	}
	resolved := model
	address := ControlURL(region) + "/inference-profiles/" + strings.TrimPrefix(ModelPath(model, ""), "/model/")
	address = strings.TrimSuffix(address, "/")
	if err := Call(ctx, client, "GetInferenceProfile", http.MethodGet, address, nil, sign, bearer, &answer); err != nil {
		slog.Debug("Inference profile resolution skipped", "profile", model, "error", err)
	} else {
		for _, m := range answer.Models {
			if m.ModelArn != "" {
				resolved = m.ModelArn
				break
			}
		}
	}
	p.mu.Lock()
	p.resolved[model] = resolved
	p.mu.Unlock()
	return resolved
}

// Signs each request by SigV4 for the service with the provider's keys, in the region the request's Bedrock address names, else the one
// given
func Signer(keys Provider, service, region string) func(request *http.Request, body []byte) error {
	return func(request *http.Request, body []byte) error {
		current, err := keys.Retrieve(request.Context())
		if err != nil {
			return err
		}
		signing := region
		if named := EndpointRegion(request.URL.Host); named != "" {
			signing = named
		}
		return Sign(request, body, current, service, signing, time.Now())
	}
}
