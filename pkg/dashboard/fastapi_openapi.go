package dashboard

import (
	"context"
	"regexp"
	"slices"
	"strings"
	"sync"

	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
)

const refPrefix = "#/components/schemas/"

// Field order of the OpenAPI Schema model, the key order a dumped schema keeps
var schemaKeyOrder = []string{"$schema", "$vocabulary", "$id", "$anchor", "$dynamicAnchor", "$ref", "$dynamicRef", "$defs",
	"$comment", "allOf", "anyOf", "oneOf", "not", "if", "then", "else", "dependentSchemas", "prefixItems", "items", "contains",
	"properties", "patternProperties", "additionalProperties", "propertyNames", "unevaluatedItems", "unevaluatedProperties",
	"type", "enum", "const", "multipleOf", "maximum", "exclusiveMaximum", "minimum", "exclusiveMinimum", "maxLength",
	"minLength", "pattern", "maxItems", "minItems", "uniqueItems", "maxContains", "minContains", "maxProperties",
	"minProperties", "required", "dependentRequired", "format", "contentEncoding", "contentMediaType", "contentSchema",
	"title", "description", "default", "deprecated", "readOnly", "writeOnly", "examples", "discriminator", "xml",
	"externalDocs", "example"}

// Schema with its keys in Schema model order and None values dropped, extra keys last
func schemaObj(pairs ...any) *pyjson.Object {
	vals := map[string]any{}
	var extra []string
	for i := 0; i+1 < len(pairs); i += 2 {
		k := pairs[i].(string)
		if pairs[i+1] == nil {
			continue
		}
		vals[k] = pairs[i+1]
		if !slices.Contains(schemaKeyOrder, k) {
			extra = append(extra, k)
		}
	}
	out := pyjson.NewObject()
	for _, k := range append(slices.Clone(schemaKeyOrder), extra...) {
		if v, ok := vals[k]; ok {
			out.Set(k, v)
		}
	}
	return out
}

// Schema object with one more key placed in model order
func schemaWith(s *pyjson.Object, key string, v any) *pyjson.Object {
	pairs := make([]any, 0, 2*s.Len()+2)
	for _, k := range s.Keys {
		pairs = append(pairs, k, s.Vals[k])
	}
	return schemaObj(append(pairs, key, v)...)
}

// JSON schema of a field type in validation mode, model definitions collected into defs
func typeSchema(t FieldType, defs map[string]*pyjson.Object) *pyjson.Object {
	switch x := t.(type) {
	case strType:
		return schemaObj("type", "string")
	case strLenType:
		var minV, maxV any
		if x.min != nil {
			minV = *x.min
		}
		if x.max != nil {
			maxV = *x.max
		}
		return schemaObj("type", "string", "minLength", minV, "maxLength", maxV)
	case secretStrType:
		return schemaObj("type", "string", "format", "password", "writeOnly", true)
	case intType:
		var maxV, minV any
		if x.le != nil {
			maxV = *x.le
		}
		if x.ge != nil {
			minV = *x.ge
		}
		return schemaObj("type", "integer", "maximum", maxV, "minimum", minV)
	case floatType:
		return schemaObj("type", "number")
	case boolType:
		return schemaObj("type", "boolean")
	case anyType:
		return schemaObj()
	case optionalType:
		inner := typeSchema(x.inner, defs)
		if anyOf, ok := inner.Get("anyOf"); ok && inner.Len() == 1 {
			return schemaObj("anyOf", append(append([]any{}, anyOf.([]any)...), schemaObj("type", "null")))
		}
		return schemaObj("anyOf", []any{inner, schemaObj("type", "null")})
	case unionType:
		choices := make([]any, len(x.members))
		for i, m := range x.members {
			choices[i] = typeSchema(m, defs)
		}
		return schemaObj("anyOf", choices)
	case beforeValidatorType:
		return typeSchema(x.inner, defs)
	case listType:
		return schemaObj("items", typeSchema(x.item, defs), "type", "array")
	case dictType:
		var additional any = typeSchema(x.value, defs)
		if _, isAny := x.value.(anyType); isAny {
			additional = true
		}
		return schemaObj("additionalProperties", additional, "type", "object")
	case literalType:
		typ := literalJSONType(x.values)
		if len(x.values) == 1 {
			return schemaObj("type", typ, "const", x.values[0])
		}
		return schemaObj("type", typ, "enum", slices.Clone(x.values))
	case *Model:
		if _, done := defs[x.ClassName]; !done {
			defs[x.ClassName] = nil
			defs[x.ClassName] = x.componentSchema(defs)
		}
		return schemaObj("$ref", refPrefix+x.ClassName)
	}
	return schemaObj()
}

// JSON type of literal values when they share one, nil otherwise
func literalJSONType(values []any) any {
	var typ any
	for i, v := range values {
		var t string
		switch v.(type) {
		case string:
			t = "string"
		case bool:
			t = "boolean"
		case int64:
			t = "integer"
		default:
			return nil
		}
		if i > 0 && typ != t {
			return nil
		}
		typ = t
	}
	return typ
}

// Title pydantic derives from a field name, title cased with underscores as spaces
func fieldTitle(name string) string {
	return strings.ReplaceAll(pystr.Title(name), "_", " ")
}

// Component schema of a model, its properties, required names and title
func (m *Model) componentSchema(defs map[string]*pyjson.Object) *pyjson.Object {
	props := pyjson.NewObject()
	var required []any
	for _, f := range m.Fields {
		alias := f.validationAlias()
		s := typeSchema(f.Type, defs)
		if _, isModel := f.Type.(*Model); !isModel {
			s = schemaWith(s, "title", fieldTitle(alias))
		}
		if f.Required {
			required = append(required, alias)
		} else if f.Factory == nil && f.Default != nil {
			s = schemaWith(s, "default", jsonableEncoder(f.Default))
		}
		props.Set(alias, s)
	}
	var req any
	if len(required) > 0 {
		req = required
	}
	var description any
	if m.Doc != "" {
		description = cleandoc(m.Doc)
	}
	return schemaObj("properties", props, "type", "object", "required", req, "title", m.ClassName, "description", description)
}

// Static schema of one validation error entry
func validationErrorDefinition() *pyjson.Object {
	return schemaObj("properties", jsonObj(
		"loc", schemaObj("items", schemaObj("anyOf", []any{schemaObj("type", "string"), schemaObj("type", "integer")}), "type", "array", "title", "Location"),
		"msg", schemaObj("type", "string", "title", "Message"),
		"type", schemaObj("type", "string", "title", "Error Type"),
		"input", schemaObj("title", "Input"),
		"ctx", schemaObj("type", "object", "title", "Context"),
	), "type", "object", "required", []any{"loc", "msg", "type"}, "title", "ValidationError")
}

// Static schema of the 422 response body
func httpValidationErrorDefinition() *pyjson.Object {
	return schemaObj("properties", jsonObj("detail", schemaObj("items", schemaObj("$ref", refPrefix+"ValidationError"),
		"type", "array", "title", "Detail")), "type", "object", "title", "HTTPValidationError")
}

var nonWordRE = regexp.MustCompile(`\W`)

// Operation id of a route method, the name and path format with non word characters as underscores
func uniqueID(r *APIRoute) string {
	return nonWordRE.ReplaceAllString(r.Name+r.pathFormat, "_") + "_" + strings.ToLower(r.Methods[0])
}

// Operation object of one route method with its parameters, body and responses
func openAPIOperation(r *APIRoute, method string, defs map[string]*pyjson.Object) *pyjson.Object {
	ep := r.Endpoint
	op := pyjson.NewObject()
	if len(ep.Tags) > 0 {
		tags := make([]any, len(ep.Tags))
		for i, t := range ep.Tags {
			tags[i] = t
		}
		op.Set("tags", tags)
	}
	op.Set("summary", pystr.Title(strings.ReplaceAll(r.Name, "_", " ")))
	if desc := endpointDescription(ep.Doc); desc != "" {
		op.Set("description", desc)
	}
	op.Set("operationId", uniqueID(r))
	var params []any
	for _, in := range []string{"path", "query", "header", "cookie"} {
		for _, p := range ep.Params {
			if p.In != in {
				continue
			}
			name := p.validationAlias()
			if in == "header" && p.Alias == "" {
				name = strings.ReplaceAll(p.Name, "_", "-")
			}
			s := schemaWith(typeSchema(p.Type, defs), "title", fieldTitle(p.validationAlias()))
			if !p.Required && p.Default != nil {
				s = schemaWith(s, "default", jsonableEncoder(p.Default))
			}
			params = append(params, jsonObj("required", p.Required, "schema", s, "name", name, "in", in))
		}
	}
	if len(params) > 0 {
		op.Set("parameters", params)
	}
	if rb := formRequestBody(r, ep, defs); rb != nil {
		op.Set("requestBody", rb)
	} else if len(ep.Body) > 0 {
		var bodySchema *pyjson.Object
		required := false
		if !embedBodyFields(ep.Body) {
			p := ep.Body[0]
			bodySchema = typeSchema(p.Type, defs)
			if _, isModel := p.Type.(*Model); !isModel {
				bodySchema = schemaWith(bodySchema, "title", fieldTitle(p.Name))
			}
			required = p.Required
		} else {
			fields := make([]ModelField, 0, len(ep.Body))
			for _, p := range ep.Body {
				fields = append(fields, ModelField{Name: p.Name, Type: p.Type, Required: p.Required, Default: p.Default})
				required = required || p.Required
			}
			bodySchema = typeSchema(&Model{ClassName: "Body_" + uniqueID(r), Fields: fields}, defs)
		}
		rb := pyjson.NewObject()
		rb.Set("content", jsonObj("application/json", jsonObj("schema", bodySchema)))
		if required {
			rb.Set("required", true)
		}
		op.Set("requestBody", rb)
	}
	responses := jsonObj("200", jsonObj("description", "Successful Response",
		"content", jsonObj("application/json", jsonObj("schema", schemaObj()))))
	if len(ep.Params) > 0 || len(ep.Body) > 0 {
		responses.Set("422", jsonObj("description", "Validation Error",
			"content", jsonObj("application/json", jsonObj("schema", schemaObj("$ref", refPrefix+"HTTPValidationError")))))
		if _, ok := defs["ValidationError"]; !ok {
			defs["ValidationError"] = validationErrorDefinition()
			defs["HTTPValidationError"] = httpValidationErrorDefinition()
		}
	}
	op.Set("responses", responses)
	return op
}

// Description of an endpoint docstring, cleandoc text cut at a form feed
func endpointDescription(doc string) string {
	text := cleandoc(doc)
	if i := strings.Index(text, "\f"); i >= 0 {
		text = text[:i]
	}
	return pystr.Strip(text)
}

// Docstring with its common indentation removed and blank edge lines dropped, inspect.cleandoc
func cleandoc(doc string) string {
	lines := strings.Split(pystr.ExpandTabs(doc, 8), "\n")
	margin := -1
	for _, l := range lines[1:] {
		content := strings.TrimLeft(l, " ")
		if content != "" {
			if indent := len(l) - len(content); margin < 0 || indent < margin {
				margin = indent
			}
		}
	}
	lines[0] = strings.TrimLeft(lines[0], " ")
	if margin >= 0 {
		for i := 1; i < len(lines); i++ {
			if len(lines[i]) >= margin {
				lines[i] = lines[i][margin:]
			} else {
				lines[i] = strings.TrimLeft(lines[i], " ")
			}
		}
	}
	for len(lines) > 0 && lines[len(lines)-1] == "" {
		lines = lines[:len(lines)-1]
	}
	for len(lines) > 0 && lines[0] == "" {
		lines = lines[1:]
	}
	return strings.Join(lines, "\n")
}

// Path item method order of the OpenAPI PathItem model
var pathItemOrder = []string{"get", "put", "post", "delete", "options", "head", "patch", "trace"}

// OpenAPI document of every API route, paths in route order and component schemas sorted
func (a *FastAPI) buildOpenAPI() *pyjson.Object {
	defs := map[string]*pyjson.Object{}
	paths := pyjson.NewObject()
	for _, route := range a.Router.Routes {
		r, ok := route.(*APIRoute)
		if !ok {
			continue
		}
		ops := map[string]*pyjson.Object{}
		for _, m := range r.Methods {
			ops[strings.ToLower(m)] = openAPIOperation(r, m, defs)
		}
		item, ok := paths.Vals[r.pathFormat].(*pyjson.Object)
		if !ok {
			item = pyjson.NewObject()
		}
		merged := map[string]any{}
		for _, k := range item.Keys {
			merged[k] = item.Vals[k]
		}
		for k, v := range ops {
			merged[k] = v
		}
		ordered := pyjson.NewObject()
		for _, k := range pathItemOrder {
			if v, ok := merged[k]; ok {
				ordered.Set(k, v)
			}
		}
		paths.Set(r.pathFormat, ordered)
	}
	out := jsonObj("openapi", "3.1.0", "info", jsonObj("title", a.Title, "version", a.Version), "paths", paths)
	if len(defs) > 0 {
		names := make([]string, 0, len(defs))
		for k := range defs {
			names = append(names, k)
		}
		slices.Sort(names)
		schemas := pyjson.NewObject()
		for _, k := range names {
			schemas.Set(k, defs[k])
		}
		out.Set("components", jsonObj("schemas", schemas))
	}
	return out
}

// Plain Starlette route serving GET and HEAD with a request to response function
type starletteRoute struct {
	path      string
	name      string
	endpoint  func(ctx context.Context, req *Request) (ASGIResponse, error)
	pathRegex *regexp.Regexp
	convs     map[string]convertor
}

func newStarletteRoute(path, name string, endpoint func(ctx context.Context, req *Request) (ASGIResponse, error)) *starletteRoute {
	r := &starletteRoute{path: path, name: name, endpoint: endpoint}
	r.pathRegex, _, r.convs = compilePath(path)
	return r
}

func (r *starletteRoute) RoutePath() string { return r.path }

func (r *starletteRoute) Matches(s *Scope) (Match, *childScope) {
	if s.Type != "http" {
		return MatchNone, nil
	}
	params, ok := matchParams(r.pathRegex, r.convs, s, getRoutePath(s))
	if !ok {
		return MatchNone, nil
	}
	child := &childScope{endpoint: r.endpoint, pathParams: params}
	if s.Method != "GET" && s.Method != "HEAD" {
		return MatchPartial, child
	}
	return MatchFull, child
}

func (r *starletteRoute) Handle(ctx context.Context, s *Scope, receive Receive, send Send) error {
	if s.Method != "GET" && s.Method != "HEAD" {
		if s.App != nil {
			return NewHTTPException(405, nil, "Allow", "GET, HEAD")
		}
		return PlainTextResponse("Method Not Allowed", 405, "Allow", "GET, HEAD").Serve(ctx, s, receive, send)
	}
	req := NewRequest(s, receive, send)
	inner := func(ctx context.Context, s *Scope, receive Receive, send Send) error {
		resp, err := r.endpoint(ctx, req)
		if err != nil {
			return err
		}
		return resp.Serve(ctx, s, receive, send)
	}
	return wrapAppHandlingExceptions(inner, req, nil)(ctx, s, receive, send)
}

// JSON text safe inside a script tag, angle brackets and ampersands escaped
func htmlSafeJSON(v any) string {
	text, _ := pyjson.Dumps(v)
	return strings.NewReplacer("<", "\\u003c", ">", "\\u003e", "&", "\\u0026").Replace(text)
}

// Swagger UI page for the OpenAPI document at openapiURL
func swaggerUIHTML(openapiURL, title, oauth2RedirectURL string) string {
	html := strings.NewReplacer("{swagger_css_url}", "https://cdn.jsdelivr.net/npm/swagger-ui-dist@5/swagger-ui.css",
		"{swagger_favicon_url}", "https://fastapi.tiangolo.com/img/favicon.png", "{title}", title,
		"{swagger_js_url}", "https://cdn.jsdelivr.net/npm/swagger-ui-dist@5/swagger-ui-bundle.js",
		"{openapi_url}", openapiURL).Replace(swaggerUIHead)
	for _, kv := range [][2]any{{"dom_id", "#swagger-ui"}, {"layout", "BaseLayout"}, {"deepLinking", true},
		{"showExtensions", true}, {"showCommonExtensions", true}} {
		html += htmlSafeJSON(kv[0]) + ": " + htmlSafeJSON(kv[1]) + ",\n"
	}
	if oauth2RedirectURL != "" {
		html += "oauth2RedirectUrl: window.location.origin + '" + oauth2RedirectURL + "',"
	}
	return html + swaggerUIPresets + swaggerUIClose
}

// ReDoc page for the OpenAPI document at openapiURL
func redocHTML(openapiURL, title string) string {
	return strings.ReplaceAll(redocHead, "{title}", title) + redocFonts + strings.NewReplacer(
		"{redoc_favicon_url}", "https://fastapi.tiangolo.com/img/favicon.png", "{openapi_url}", openapiURL,
		"{redoc_js_url}", "https://cdn.jsdelivr.net/npm/redoc@2/bundles/redoc.standalone.js").Replace(redocTail)
}

// The openapi.json, docs, oauth2 redirect and redoc routes of the app, the schema built once
func openAPIRoutes(a *FastAPI) []BaseRoute {
	var once sync.Once
	var schema *pyjson.Object
	rootPath := func(req *Request) string { return strings.TrimRight(req.Scope.RootPath, "/") }
	return []BaseRoute{
		newStarletteRoute("/openapi.json", "openapi", func(_ context.Context, req *Request) (ASGIResponse, error) {
			once.Do(func() { schema = a.buildOpenAPI() })
			doc := schema
			if rp := rootPath(req); rp != "" {
				doc = schema.Copy()
				doc.Set("servers", []any{jsonObj("url", rp)})
				ordered := pyjson.NewObject()
				for _, k := range []string{"openapi", "info", "servers", "paths", "components"} {
					if v, ok := doc.Get(k); ok {
						ordered.Set(k, v)
					}
				}
				doc = ordered
			}
			return JSONResponse(doc, 200)
		}),
		newStarletteRoute("/docs", "swagger_ui_html", func(_ context.Context, req *Request) (ASGIResponse, error) {
			rp := rootPath(req)
			return HTMLResponse(swaggerUIHTML(rp+"/openapi.json", a.Title+" - Swagger UI", rp+"/docs/oauth2-redirect"), 200), nil
		}),
		newStarletteRoute("/docs/oauth2-redirect", "swagger_ui_redirect", func(context.Context, *Request) (ASGIResponse, error) {
			return HTMLResponse(swaggerOAuth2RedirectHTML, 200), nil
		}),
		newStarletteRoute("/redoc", "redoc_html", func(_ context.Context, req *Request) (ASGIResponse, error) {
			return HTMLResponse(redocHTML(rootPath(req)+"/openapi.json", a.Title+" - ReDoc"), 200), nil
		}),
	}
}
