package dashboard

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/nickheyer/icebath/pkg/internal/pyjson"
)

// App with one GET item route, a POST model route and a query route
func layerApp() *FastAPI {
	app := NewFastAPI("Hermes Agent", "1.0", nil, nil)
	app.AddAPIRoute("/items", []string{"GET"}, &Endpoint{Name: "list_items",
		Handle: func(context.Context, *Call) (any, error) { return jsonObj("ok", true), nil }})
	app.AddAPIRoute("/items/{item_id}", []string{"GET"}, &Endpoint{Name: "get_item",
		Params: []Param{PathParam("item_id", Int), QueryParam("limit", IntRange(Bound(0), Bound(500)), int64(20))},
		Handle: func(_ context.Context, c *Call) (any, error) {
			return jsonObj("id", c.Int("item_id"), "limit", c.Int("limit")), nil
		}})
	model := &Model{ClassName: "ItemBody", Fields: []ModelField{Field("name", Str), FieldDefault("count", Int, int64(1))}}
	app.AddAPIRoute("/items", []string{"POST"}, &Endpoint{Name: "create_item", Body: []BodyParam{Body("body", model)},
		Handle: func(_ context.Context, c *Call) (any, error) { return c.Object("body"), nil }})
	app.AddAPIRoute("/boom", []string{"GET"}, &Endpoint{Name: "boom",
		Handle: func(context.Context, *Call) (any, error) { return nil, errors.New("kaboom") }})
	app.AddAPIRoute("/teapot", []string{"GET"}, &Endpoint{Name: "teapot",
		Handle: func(context.Context, *Call) (any, error) {
			return nil, NewHTTPException(418, jsonObj("why", "short and stout"), "X-Pot", "yes")
		}})
	return app
}

func TestNotFoundBodyMatchesFastAPI(t *testing.T) {
	r := newTestClient(t, layerApp()).get("/nowhere")
	r.expect(t, 404, `{"detail":"Not Found"}`)
	if got := r.Raw; len(got) != 2 || got[0] != (Header{"content-length", "22"}) || got[1] != (Header{"content-type", "application/json"}) {
		t.Fatalf("raw headers %v", got)
	}
}

func TestMethodNotAllowedCarriesAllowHeader(t *testing.T) {
	r := newTestClient(t, layerApp()).request("DELETE", "/items/3", nil)
	r.expect(t, 405, `{"detail":"Method Not Allowed"}`)
	if r.Headers.Get("allow") != "GET" || r.Raw[0][0] != "allow" {
		t.Fatalf("headers %v", r.Raw)
	}
}

func TestHeadOnAnAPIGetRouteIsNotAllowed(t *testing.T) {
	newTestClient(t, layerApp()).request("HEAD", "/items", nil).expect(t, 405, `{"detail":"Method Not Allowed"}`)
}

func TestTrailingSlashRedirectsToTheRoute(t *testing.T) {
	r := newTestClient(t, layerApp()).get("/items/")
	r.expect(t, 307, "")
	if r.Headers.Get("location") != "http://testserver/items" {
		t.Fatalf("location %q", r.Headers.Get("location"))
	}
}

func TestPathAndQueryParamsValidate(t *testing.T) {
	c := newTestClient(t, layerApp())
	c.get("/items/7?limit=5").expect(t, 200, `{"id":7,"limit":5}`)
	c.get("/items/7").expect(t, 200, `{"id":7,"limit":20}`)
	c.get("/items/7?limit=abc").expect(t, 422,
		`{"detail":[{"type":"int_parsing","loc":["query","limit"],"msg":"Input should be a valid integer, unable to parse string as an integer","input":"abc"}]}`)
	c.get("/items/7?limit=501").expect(t, 422,
		`{"detail":[{"type":"less_than_equal","loc":["query","limit"],"msg":"Input should be less than or equal to 500","input":"501","ctx":{"le":500}}]}`)
}

func TestBodyModelValidationErrors(t *testing.T) {
	c := newTestClient(t, layerApp())
	c.request("POST", "/items", []byte(`{"name":"a"}`), "content-type", "application/json").expect(t, 200, `{"name":"a","count":1}`)
	c.request("POST", "/items", nil).expect(t, 422, `{"detail":[{"type":"missing","loc":["body"],"msg":"Field required","input":null}]}`)
	c.request("POST", "/items", []byte(`{"count":"x"}`), "content-type", "application/json").expect(t, 422,
		`{"detail":[{"type":"missing","loc":["body","name"],"msg":"Field required","input":{"count":"x"}},{"type":"int_parsing","loc":["body","count"],"msg":"Input should be a valid integer, unable to parse string as an integer","input":"x"}]}`)
	c.request("POST", "/items", []byte(`{x`), "content-type", "application/json").expect(t, 422,
		`{"detail":[{"type":"json_invalid","loc":["body",1],"msg":"JSON decode error","input":{},"ctx":{"error":"Expecting property name enclosed in double quotes"}}]}`)
	c.request("POST", "/items", []byte(`[1]`), "content-type", "application/json").expect(t, 422,
		`{"detail":[{"type":"model_type","loc":["body"],"msg":"Input should be a valid dictionary or instance of ItemBody","input":[1],"ctx":{"class_name":"ItemBody"}}]}`)
}

func TestUnhandledErrorAnswersPlainServerError(t *testing.T) {
	r := newTestClient(t, layerApp()).get("/boom")
	r.expect(t, 500, "Internal Server Error")
	if r.Headers.Get("content-type") != "text/plain; charset=utf-8" || r.AppErr == nil || r.AppErr.Error() != "kaboom" {
		t.Fatalf("headers %v err %v", r.Raw, r.AppErr)
	}
}

func TestHTTPExceptionDetailAndHeaders(t *testing.T) {
	r := newTestClient(t, layerApp()).get("/teapot")
	r.expect(t, 418, `{"detail":{"why":"short and stout"}}`)
	if r.Raw[0] != (Header{"x-pot", "yes"}) {
		t.Fatalf("headers %v", r.Raw)
	}
}

func TestHTTPMiddlewareWrapsInRegistrationOrder(t *testing.T) {
	app := layerApp()
	var order []string
	tag := func(name string) DispatchFunc {
		return func(ctx context.Context, req *Request, next CallNext) (ASGIResponse, error) {
			order = append(order, name+">")
			resp, err := next(ctx, req)
			if err != nil {
				return nil, err
			}
			resp.Header().Append("x-mw", name)
			order = append(order, "<"+name)
			return resp, nil
		}
	}
	app.HTTPMiddleware(tag("first"))
	app.HTTPMiddleware(tag("second"))
	r := newTestClient(t, app).get("/items")
	r.expect(t, 200, `{"ok":true}`)
	if strings.Join(order, " ") != "second> first> <first <second" || strings.Join(r.Headers.GetList("x-mw"), ",") != "first,second" {
		t.Fatalf("order %v headers %v", order, r.Raw)
	}
	if app.AddMiddleware(func(a ASGIApp) ASGIApp { return a }) == nil {
		t.Fatal("middleware after start must be refused")
	}
}

func TestMiddlewareReadBodyIsReplayedDownstream(t *testing.T) {
	app := layerApp()
	app.HTTPMiddleware(func(ctx context.Context, req *Request, next CallNext) (ASGIResponse, error) {
		if _, err := req.Body(ctx); err != nil {
			return nil, err
		}
		return next(ctx, req)
	})
	newTestClient(t, app).request("POST", "/items", []byte(`{"name":"b","count":2}`), "content-type", "application/json").
		expect(t, 200, `{"name":"b","count":2}`)
}

func TestOpenAPIDocumentAndDocsPages(t *testing.T) {
	c := newTestClient(t, layerApp())
	doc := c.get("/openapi.json").JSON(t).(*pyjson.Object)
	if doc.Keys[0] != "openapi" || doc.Keys[2] != "paths" || doc.Keys[3] != "components" {
		t.Fatalf("keys %v", doc.Keys)
	}
	paths := doc.Vals["paths"].(*pyjson.Object)
	item := paths.Vals["/items"].(*pyjson.Object)
	if item.Keys[0] != "get" || item.Keys[1] != "post" {
		t.Fatalf("path item %v", item.Keys)
	}
	get := item.Vals["get"].(*pyjson.Object)
	if get.Vals["summary"] != "List Items" || get.Vals["operationId"] != "list_items_items_get" {
		t.Fatalf("operation %v", get.Vals)
	}
	docs := c.get("/docs")
	if docs.Status != 200 || !strings.Contains(string(docs.Body), "<title>Hermes Agent - Swagger UI</title>") ||
		!strings.Contains(string(docs.Body), "url: '/openapi.json',") {
		t.Fatalf("docs %d %q", docs.Status, docs.Body)
	}
	if c.request("HEAD", "/redoc", nil).Status != 200 {
		t.Fatal("redoc must answer HEAD")
	}
}

func TestFileResponseRangesAndHead(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "a.txt")
	os.WriteFile(p, []byte("0123456789"), 0o644)
	app := NewFastAPI("t", "1", nil, nil)
	app.AddAPIRoute("/f", []string{"GET", "HEAD"}, &Endpoint{Name: "f",
		Handle: func(context.Context, *Call) (any, error) { return NewFileResponse(p, 200, "", nil, ""), nil }})
	c := newTestClient(t, app)
	whole := c.get("/f")
	whole.expect(t, 200, "0123456789")
	if whole.Headers.Get("content-type") != "text/plain; charset=utf-8" || whole.Headers.Get("accept-ranges") != "bytes" ||
		whole.Headers.Get("content-length") != "10" || !strings.HasPrefix(whole.Headers.Get("etag"), "\"") {
		t.Fatalf("headers %v", whole.Raw)
	}
	part := c.get("/f", "range", "bytes=2-4")
	part.expect(t, 206, "234")
	if part.Headers.Get("content-range") != "bytes 2-4/10" {
		t.Fatalf("headers %v", part.Raw)
	}
	c.get("/f", "range", "bytes=20-").expect(t, 416, "")
	c.get("/f", "range", "lines=1-2").expect(t, 400, "Only support bytes range")
	c.request("HEAD", "/f", nil).expect(t, 200, "")
}

func TestSetHeaderReplacesFirstAndDropsDuplicates(t *testing.T) {
	h := HeadersOf("A", "1", "b", "2", "a", "3")
	h.Set("a", "9")
	if got := h.Raw(); len(got) != 2 || got[0] != (Header{"a", "9"}) || got[1] != (Header{"b", "2"}) {
		t.Fatalf("raw %v", got)
	}
	h.AddVaryHeader("Origin")
	h.AddVaryHeader("Cookie")
	if h.Get("vary") != "Origin, Cookie" {
		t.Fatalf("vary %q", h.Get("vary"))
	}
}

func TestURLOfScopeUsesValidHostHeaderElseServer(t *testing.T) {
	s := testScope("http", "GET", "/a b?x=1", []string{"host", "example.com:8080"})
	if got := URLOfScope(s).String(); got != "http://example.com:8080/a b?x=1" {
		t.Fatalf("url %q", got)
	}
	s = testScope("http", "GET", "/p", []string{"host", "bad host"})
	if got := URLOfScope(s).String(); got != "http://testserver/p" {
		t.Fatalf("url %q", got)
	}
}

func TestCookieParserUnquotesAndKeepsUnnamedChunks(t *testing.T) {
	got := cookieParser(`a=1; b="x\"y\101"; lone; =v`)
	if got["a"] != "1" || got["b"] != `x"yA` || got[""] != "v" || len(got) != 3 {
		t.Fatalf("cookies %v", got)
	}
}

func TestWebSocketRouteAcceptsEchoesAndRefusesUnrouted(t *testing.T) {
	app := NewFastAPI("t", "1", nil, nil)
	app.AddAPIWebSocketRoute("/ws/{name}", &WSEndpoint{Name: "ws", Handle: func(ctx context.Context, c *Call) error {
		if err := c.WebSocket.Accept(ctx, nil, nil); err != nil {
			return err
		}
		for {
			text, err := c.WebSocket.ReceiveText(ctx)
			if err != nil {
				var d *WebSocketDisconnect
				if errors.As(err, &d) {
					return nil
				}
				return err
			}
			if err := c.WebSocket.SendText(ctx, c.Conn().PathParams()["name"].(string)+":"+text); err != nil {
				return err
			}
		}
	}})
	c := newTestClient(t, app)
	ws, refused := c.websocketConnect("/ws/bob")
	if refused != nil {
		t.Fatalf("refused %v", refused)
	}
	ws.sendText("hi")
	if got, _ := ws.receiveText(); got != "bob:hi" {
		t.Fatalf("got %q", got)
	}
	if err := ws.close(1000); err != nil {
		t.Fatal(err)
	}
	if _, refused := c.websocketConnect("/nowhere"); refused == nil || refused.Code != 1000 {
		t.Fatalf("unrouted %v", refused)
	}
}
