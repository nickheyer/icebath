//go:build !icebath_core

package tools

import (
	"context"
	"net/http"
	"sort"
	"strings"

	larkcore "github.com/larksuite/oapi-sdk-go/v3/core"

	"github.com/nickheyer/icebath/pkg/gateway/platforms/feishu/feishuclient"
)

// One query pair of a lark request, the value sent the way requests encodes params
type larkQuery struct {
	key   string
	value any
}

// Tenant token request for the method, URI, path params, query pairs and JSON body
func buildLarkRequest(method, uri string, paths map[string]string, queries []larkQuery, body any) *larkcore.ApiReq {
	httpMethod := http.MethodPost
	if method == "GET" {
		httpMethod = http.MethodGet
	}
	req := &larkcore.ApiReq{HttpMethod: httpMethod, ApiPath: uri, PathParams: larkcore.PathParams{}, QueryParams: larkcore.QueryParams{},
		SupportedAccessTokenTypes: []larkcore.AccessTokenType{larkcore.AccessTokenTypeTenant}}
	for k, v := range paths {
		req.PathParams.Set(k, v)
	}
	for _, q := range queries {
		for _, v := range requestsParamValues(q.value) {
			req.QueryParams.Add(q.key, v)
		}
	}
	if body != nil {
		req.Body = body
	}
	return req
}

// Query values requests sends for one params pair, iterables expanded and None items dropped
func requestsParamValues(v any) []string {
	items := []any{v}
	if _, isStr := v.(string); !isStr {
		if seq, ok := pyIterItems(v); ok {
			items = seq
		}
	}
	var out []string
	for _, item := range items {
		switch x := item.(type) {
		case nil:
		case string:
			out = append(out, x)
		default:
			seq, ok := pyIterItems(x)
			if !ok {
				out = append(out, anyutil.String(x))
				continue
			}
			for _, elt := range seq {
				out = append(out, anyutil.String(elt))
			}
		}
	}
	return out
}

// Items a Python for loop yields over a decoded JSON value, dict keys for a dict, false for a scalar
func pyIterItems(v any) ([]any, bool) {
	switch x := v.(type) {
	case []any:
		return x, true
	case *jsonutil.Object:
		keys := make([]any, x.Len())
		for i, k := range x.Keys() {
			keys[i] = k
		}
		return keys, true
	case map[string]any:
		names := make([]string, 0, len(x))
		for k := range x {
			names = append(names, k)
		}
		sort.Strings(names)
		keys := make([]any, len(names))
		for i, k := range names {
			keys[i] = k
		}
		return keys, true
	}
	return nil, false
}

// Response of a lark request with the code and msg the SDK response object carries, nil standing for None
type larkResponse struct {
	code any
	msg  any
	raw  *larkcore.ApiResp
}

// Code and msg of a raw response, the JSON envelope for a JSON body and code 0 for any other 2xx answer
func larkResponseOf(raw *larkcore.ApiResp) larkResponse {
	resp := larkResponse{raw: raw}
	if strings.HasPrefix(raw.Header.Get("Content-Type"), "application/json") {
		text, err := textutil.DecodeUTF8(raw.RawBody)
		if err != nil {
			panic(err)
		}
		parsed, err := jsonutil.Decode(text)
		if err != nil {
			panic(err)
		}
		switch d := parsed.(type) {
		case nil:
		case *jsonutil.Object:
			resp.code, _ = d.Get("code")
			resp.msg, _ = d.Get("msg")
		default:
			panic(pyAttributeError(d, "items"))
		}
		return resp
	}
	if raw.StatusCode >= 200 && raw.StatusCode < 300 {
		resp.code = int64(0)
	}
	return resp
}

// Sends a request through the lark client, a transport failure raised to the dispatcher
func larkRequest(ctx context.Context, client feishuclient.API, req *larkcore.ApiReq) larkResponse {
	raw, err := client.Do(ctx, req)
	if err != nil {
		panic(err)
	}
	return larkResponseOf(raw)
}

// Builds and sends a request returning its code, msg and data
func larkCall(ctx context.Context, client feishuclient.API, method, uri string, paths map[string]string, queries []larkQuery, body any) (any, any, any) {
	resp := larkRequest(ctx, client, buildLarkRequest(method, uri, paths, queries, body))
	return resp.code, resp.msg, larkResponseData(resp)
}

// Parsed JSON object of the raw body, nil when it is absent, unparseable or not a dict
func larkRawBody(resp larkResponse) *jsonutil.Object {
	parsed, err := jsonutil.Decode(resp.raw.RawBody)
	if err != nil {
		if _, isJSON := err.(*jsonutil.SyntaxError); isJSON {
			return nil
		}
		panic(err)
	}
	body, _ := parsed.(*jsonutil.Object)
	return body
}

// Data of a lark response from the raw JSON body, an empty dict when the body has none
func larkResponseData(resp larkResponse) any {
	body := larkRawBody(resp)
	if body == nil {
		return jsonutil.NewObject()
	}
	return body.ValueOr("data", jsonutil.NewObject())
}

// Python truth of code equal to zero, the success test of a lark answer
func larkCodeOK(code any) bool { return anyutil.Equal(code, int64(0)) }
