package tools

import "testing"

// The origin pattern matches at the start with a final newline allowed before the end like re.match
func TestIsOriginHTTPErrorPythonSemantics(t *testing.T) {
	cases := map[string]bool{
		"HTTP 404 from origin":       true,
		"HTTP 404 from origin\n":     true,
		"HTTP 404 from origin: gone": true,
		"HTTP ٤٠٤ from origin":       true,
		"HTTP 404 from origin\n\n":   false,
		"x HTTP 404 from origin":     false,
		"HTTP 404 from originx":      false,
		"":                           false,
	}
	for in, want := range cases {
		if got := isOriginHTTPError(in); got != want {
			t.Errorf("%q -> %v", in, got)
		}
	}
}

// Crawl tags end with an HTTP status or NOT_FOUND after a Unicode word boundary, trailing Unicode space allowed
func TestExaCrawlStatusPythonSemantics(t *testing.T) {
	cases := []struct {
		in   string
		code int
		ok   bool
	}{
		{"CRAWL_HTTP_429", 429, true},
		{"error: CRAWL_NOT_FOUND  ", 404, true},
		{"CRAWL_HTTP_٤٠٣", 403, true},
		{"éCRAWL_HTTP_500", 0, false},
		{"CRAWL_HTTP_500\n", 500, true},
		{"CRAWL_HTTP_500 x", 0, false},
	}
	for _, c := range cases {
		if code, ok := exaCrawlStatus(c.in); code != c.code || ok != c.ok {
			t.Errorf("%q -> %d %v", c.in, code, ok)
		}
	}
}

// Keenable bodies decode like json.loads and the message is str of any truthy value
func TestKeenableOriginErrorPythonSemantics(t *testing.T) {
	cases := []struct {
		status int
		body   string
		want   string
	}{
		{404, `{"message": "The page does not exist."}`, "HTTP 404 from origin: The page does not exist."},
		{502, `{"message": "  THE TARGET SERVER refused "}`, "HTTP 502 from origin: THE TARGET SERVER refused"},
		{404, `{"message": "the target servers"}`, ""},
		{404, `{"message": "x", "message": "the page does not exist"}`, "HTTP 404 from origin: the page does not exist"},
		{404, `{"message": NaN}`, ""},
		{404, `{"message": 0}`, ""},
		{404, `["the page does not exist"]`, ""},
		{404, ``, ""},
		{399, `{"message": "the page does not exist"}`, ""},
	}
	for _, c := range cases {
		if got, err := keenableOriginError(c.status, c.body); got != c.want || err != nil {
			t.Errorf("%d %s -> %q", c.status, c.body, got)
		}
	}
}

// Body and reason fall through Python truthiness and non strings take their str form
func TestVendorErrorEntryTruthiness(t *testing.T) {
	cases := []struct {
		status, reason, body any
		err, content         string
	}{
		{404, "http_error", "page body", "HTTP 404 from origin", "page body"},
		{404, nil, 0, "HTTP 404 from origin", ""},
		{nil, "timeout", "", "timeout", ""},
		{nil, "", []any{}, "extraction failed", ""},
		{nil, "tag", map[string]any{"a": 1}, "{'a': 1}", ""},
		{200, 0, float64(0), "extraction failed", ""},
		{nil, float64(3), nil, "3.0", ""},
	}
	for _, c := range cases {
		p, err := vendorErrorEntry("https://e.com", c.status, c.reason, c.body)
		if err != nil {
			t.Fatal(err)
		}
		if p.Error != c.err || p.Content != c.content || p.Metadata["sourceURL"] != "https://e.com" {
			t.Errorf("%v %v %v -> %+v", c.status, c.reason, c.body, p)
		}
	}
}
