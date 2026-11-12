package dashboard

import (
	"testing"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
)

func TestGetApiEnvPassesProviderPrimaryThrough(t *testing.T) {
	c := newTestClient(t, dashboardTestApp(t))
	config.InvalidateEnvCache()
	env := appObject(t, c.request("GET", "/api/env", nil, sessionHeaders()...))
	field := func(name string) *pyjson.Object {
		v, ok := env.Get(name)
		if !ok {
			t.Fatalf("no %s in /api/env", name)
		}
		return v.(*pyjson.Object)
	}
	if v, _ := field("ALIBABA_CODING_PLAN_CN_API_KEY").Get("provider_primary"); v != true {
		t.Fatalf("ALIBABA_CODING_PLAN_CN_API_KEY provider_primary = %#v", v)
	}
	profiles, _ := field("DASHSCOPE_API_KEY").Get("provider_profiles")
	var cn *pyjson.Object
	for _, p := range profiles.([]any) {
		if obj := p.(*pyjson.Object); func() bool { v, _ := obj.Get("provider"); return v == "alibaba-coding-plan-cn" }() {
			cn = obj
			break
		}
	}
	if cn == nil {
		t.Fatalf("no alibaba-coding-plan-cn profile on DASHSCOPE_API_KEY: %#v", profiles)
	}
	if v, _ := cn.Get("primary"); v != false {
		t.Fatalf("DASHSCOPE_API_KEY is a fallback alias for alibaba-coding-plan-cn, primary = %#v", v)
	}
}
