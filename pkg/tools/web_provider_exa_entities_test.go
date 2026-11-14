package tools

import (
	"context"
	"net/http"
	"testing"
)

func TestExaEntitiesRaiseLikeTheSDKParser(t *testing.T) {
	env := sdkProviderEnv(t, "EXA_API_KEY=exa-key\n", map[string]any{"web.keyless_fallback": false})
	var entities sharedText
	srv, _ := fakeVendor(t, func(call vendorCall, w http.ResponseWriter) {
		writeVendorJSON(w, 200, `{"results":[{"url":"https://t.test/a","title":"A","text":"body","highlights":["h"],"entities":`+entities.get()+`}]}`)
	})
	prev := exaBaseURL
	exaBaseURL = srv.URL
	t.Cleanup(func() { exaBaseURL = prev })
	cases := []struct{ entities, want string }{
		{`[{"type":"company"}]`, "'id'"},
		{`[{"type":"person","id":1}]`, "'version'"},
		{`"x"`, "'str' object has no attribute 'get'"},
		{`7`, "'int' object is not iterable"},
		{`[{"type":"company","id":1,"version":1,"properties":null}]`, "'NoneType' object has no attribute 'get'"},
		{`[{"type":"company","id":1,"version":1,"properties":{"workforce":[1]}}]`, "'list' object has no attribute 'get'"},
		{`[{"type":"company","id":1,"version":1,"properties":{"financials":{"fundingLatestRound":"r"}}}]`, "'str' object has no attribute 'get'"},
		{`[{"type":"company","id":1,"version":1,"properties":{"webTraffic":true}}]`, "'bool' object has no attribute 'get'"},
		{`[{"type":"person","id":1,"version":1,"properties":{"workHistory":5}}]`, "'int' object is not iterable"},
		{`[{"type":"person","id":1,"version":1,"properties":{"educationHistory":[{"institution":"u"}]}}]`, "'str' object has no attribute 'get'"},
		{`[{"type":"person","id":1,"version":1,"properties":{"workHistory":[{"dates":["2020"]}]}}]`, "'list' object has no attribute 'get'"},
		{`[{"type":"person","id":1,"version":1,"properties":{"research":{"notableWorks":[1]}}}]`, "'int' object has no attribute 'get'"},
	}
	for _, c := range cases {
		entities.set(c.entities)
		out, _ := exaP().Extract(context.Background(), env, []string{"https://t.test/a"}, "")
		if len(out) != 1 || out[0].Error != "Exa extract failed: "+c.want {
			t.Fatalf("%s: %+v", c.entities, out)
		}
	}
	for _, ok := range []string{`null`, `[]`, `""`, `[{"type":"other"}]`, `[{"type":"person","id":1,"version":1}]`,
		`[{"type":"company","id":1,"version":1,"properties":{"workforce":{},"headquarters":0,"financials":{"fundingLatestRound":{}},"webTraffic":""}}]`,
		`[{"type":"person","id":1,"version":1,"properties":{"workHistory":null,"educationHistory":[],"research":{"notableWorks":null}}}]`} {
		entities.set(ok)
		out, _ := exaP().Extract(context.Background(), env, []string{"https://t.test/a"}, "")
		if len(out) != 1 || out[0].Error != "" || out[0].Content != "body" {
			t.Fatalf("%s: %+v", ok, out)
		}
	}
	entities.set(`[{"type":"company","id":1}]`)
	if resp, _ := exaP().Search(context.Background(), env, "q", 5); resp.Error != "Exa search failed: 'version'" {
		t.Fatalf("%+v", resp)
	}
}
