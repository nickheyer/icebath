package bedrock

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/nickheyer/icebath/pkg/profiles"
	"github.com/nickheyer/icebath/pkg/provider/sigv4"
)

func TestProfileAndAuth(t *testing.T) {
	p, ok := profiles.Lookup(Name)
	if !ok || p.Vendor != Vendor || p.Auth != AuthSigV4 || !strings.HasPrefix(p.BaseURL, "https://bedrock-runtime.") || p.Options["region"] == "" {
		t.Fatalf("%+v %v", p, ok)
	}
	if Profile("eu-west-1").BaseURL != "https://bedrock-runtime.eu-west-1.amazonaws.com" || Control("eu-west-1") != "https://bedrock.eu-west-1.amazonaws.com" {
		t.Fatal(Profile("eu-west-1").BaseURL)
	}
	if regionOf("https://bedrock-runtime.us-west-2.amazonaws.com") != "us-west-2" || regionOf("https://bedrock.ap-south-1.amazonaws.com/") != "ap-south-1" || regionOf("http://localhost:4566") != "" || regionOf("::") != "" {
		t.Fatal("region parsing")
	}
	static := Profile("us-west-2")
	prov, err := static.Build("AK:SK:TOKEN")
	if err != nil {
		t.Fatal(err)
	}
	auth := prov.Auth.(*sigv4.Auth)
	if auth.Region != "us-west-2" || auth.Service != "bedrock" {
		t.Fatalf("%+v", auth)
	}
	creds, err := auth.Source.Credentials(context.Background())
	if err != nil || creds != (sigv4.Credentials{AccessKey: "AK", SecretKey: "SK", SessionToken: "TOKEN"}) {
		t.Fatalf("%v %+v", err, creds)
	}
	if _, err := static.Build("justone"); err == nil {
		t.Fatal("malformed key accepted")
	}
	named := Profile("us-east-2")
	named.Options["profile"] = "work"
	named.Options["service"] = "bedrock-custom"
	prov, _ = named.Build("")
	if a := prov.Auth.(*sigv4.Auth); a.Service != "bedrock-custom" || a.Source.(sigv4.Profile).Name != "work" {
		t.Fatalf("%+v", a)
	}
	derived := profiles.Profile{Name: "d", Protocol: static.Protocol, BaseURL: Runtime("ca-central-1"), Auth: AuthSigV4}
	prov, err = derived.Build("")
	if err != nil || prov.Auth.(*sigv4.Auth).Region != "ca-central-1" {
		t.Fatalf("%v", err)
	}
	t.Setenv("AWS_REGION", "")
	t.Setenv("AWS_DEFAULT_REGION", "")
	custom := profiles.Profile{Name: "c", Protocol: static.Protocol, BaseURL: "http://localhost:4566", Auth: AuthSigV4}
	if _, err := custom.Build(""); err == nil || !strings.Contains(err.Error(), "region is required") {
		t.Fatalf("%v", err)
	}
	t.Setenv("AWS_REGION", "sa-east-1")
	if prov, err := custom.Build(""); err != nil || prov.Auth.(*sigv4.Auth).Region != "sa-east-1" {
		t.Fatalf("%v", err)
	}
}

func TestModels(t *testing.T) {
	var seen []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen = append(seen, r.URL.Path+" "+r.Header.Get("Authorization")[:16])
		if r.URL.Path != "/foundation-models" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		io.WriteString(w, `{"modelSummaries":[
		 {"modelId":"anthropic.claude-sonnet","modelName":"Claude Sonnet","providerName":"Anthropic","outputModalities":["TEXT"],"responseStreamingSupported":true},
		 {"modelId":"amazon.titan-image","modelName":"Titan Image","providerName":"Amazon","outputModalities":["IMAGE"],"responseStreamingSupported":false},
		 {"modelId":"meta.llama","modelName":"Llama","providerName":"Meta","outputModalities":["TEXT"],"responseStreamingSupported":false}]}`)
	}))
	defer srv.Close()
	p := profiles.Profile{Name: "local", Vendor: Vendor, Protocol: Profile("x").Protocol, BaseURL: srv.URL, Auth: AuthSigV4, Options: map[string]string{"region": "us-east-1"}}
	prov, err := p.Build("AK:SK")
	if err != nil {
		t.Fatal(err)
	}
	models, err := p.ListModels(context.Background(), prov)
	if err != nil || len(models) != 1 || models[0].ID != "anthropic.claude-sonnet" || models[0].Name != "Anthropic Claude Sonnet" {
		t.Fatalf("%v %+v", err, models)
	}
	if len(seen) != 1 || seen[0] != "/foundation-models AWS4-HMAC-SHA256" {
		t.Fatalf("%v", seen)
	}
	if _, err := Models(context.Background(), nil); err == nil {
		t.Fatal("nil provider accepted")
	}
	prov.BaseURL = srv.URL + "/x"
	if _, err := Models(context.Background(), prov); err == nil {
		t.Fatal("http failure swallowed")
	}
}
