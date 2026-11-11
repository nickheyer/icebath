package dashboard

import (
	"context"
	"strconv"
	"strings"
	"testing"
)

func TestAuthBodyLimitRefusesDeclaredAndStreamedOversizeBodies(t *testing.T) {
	app := NewFastAPI("t", "1", nil, nil)
	app.AddAPIRoute("/auth/password-login", []string{"POST"}, &Endpoint{Name: "password_login",
		Handle: func(ctx context.Context, c *Call) (any, error) {
			body, err := c.Request.Body(ctx)
			if err != nil {
				return nil, err
			}
			return jsonObj("size", len(body)), nil
		}})
	app.AddMiddleware(AuthBodyLimitMiddleware)
	c := newTestClient(t, app)
	big := []byte(strings.Repeat("x", AuthBodyLimit+1))
	c.request("POST", "/auth/password-login", big).expect(t, 413, `{"detail":"Request body too large"}`)
	c.request("POST", "/auth/password-login", []byte("{}"), "content-length", strconv.Itoa(AuthBodyLimit+1)).
		expect(t, 413, `{"detail":"Request body too large"}`)
	c.request("POST", "/auth/password-login", []byte("ok")).expect(t, 200, `{"size":2}`)
	if !pathIsPublic("/assets/x.css") || pathIsPublic("/assetsleak") || pathIsPublic("/api/status/extension") {
		t.Fatal("public path matching")
	}
}
