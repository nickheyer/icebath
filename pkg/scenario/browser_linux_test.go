package scenario

import (
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/nickheyer/icebath/pkg/internal/fakellm"
)

const iceShopPage = `<!DOCTYPE html><html><head><title>Ice Shop</title></head><body>
<h1>Ice Shop</h1><ul id="items"><li>Alpha</li><li>Beta</li><li>Gamma</li></ul><input id="q">
</body></html>`

// One turn runs browser_exec code that loops over a loopback page with several helpers, then code whose helper raises
func TestScenarioBrowserExecRunsPythonAgainstALoopbackPage(t *testing.T) {
	reading, failing := map[string]any{}, map[string]any{}
	s := Start(t, Setup{Model: []any{
		fakellm.ToolCall{Name: "browser_exec", Args: reading},
		fakellm.ToolCall{Name: "browser_exec", Args: failing},
		fakellm.Text{Text: "The shop lists three items."},
	}, Extra: "browser:\n  executable_path: /usr/bin/chromium\n"})
	shop := s.Peer("shop", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write([]byte(iceShopPage))
	}))
	reading["code"] = "# Reading the ice shop items\n" +
		"new_tab(\"" + shop.URL + "/shop\")\n" +
		"wait_for_load()\n" +
		"names = []\n" +
		"for i in range(js(\"document.querySelectorAll('#items li').length\")):\n" +
		"    names.append(js(f\"document.querySelectorAll('#items li')[{i}].textContent\"))\n" +
		"fill_input(\"#q\", \"frost\")\n" +
		"print(\"heading:\", js(\"document.querySelector('h1').textContent\"))\n" +
		"print(\"items:\", \", \".join(names))\n" +
		"print(\"typed:\", js(\"document.querySelector('#q').value\"))\n" +
		"print(\"url:\", page_info()[\"url\"])\n"
	failing["code"] = "# Typing into a field the page lacks\nprint(\"before\")\nfill_input(\"#missing\", \"x\")\n"
	s.RunStep(Step{Args: []string{"chat", "-q", "List the ice shop items", "-Q"}, Timeout: 3 * time.Minute}).Exit(0).Has("The shop lists three items.")
	var results []map[string]any
	for _, m := range s.LastMessages() {
		if Dig(m, "role") == "tool" {
			content, _ := Dig(m, "content").(string)
			start, end := strings.Index(content, "{"), strings.LastIndex(content, "}")
			if !strings.HasPrefix(content, `<untrusted_tool_result source="browser_exec">`) || start < 0 || end < start {
				s.Failf("browser_exec result is not wrapped as untrusted data: %s", clip(content))
			}
			parsed, ok := s.value(content[start : end+1]).(map[string]any)
			if !ok {
				s.Failf("tool result is not a JSON object: %s", clip(content))
			}
			results = append(results, parsed)
		}
	}
	if len(results) != 2 {
		s.Failf("the last request carries %d tool results, want 2", len(results))
	}
	s.HasJSON(results[0], `{"success": true, "exit_code": 0, "output": "heading: Ice Shop\nitems: Alpha, Beta, Gamma\ntyped: frost\nurl: ${PEER_shop}/shop\n"}`)
	if workspace, _ := results[0]["workspace"].(string); !strings.HasPrefix(workspace, s.Home) {
		s.Failf("the workspace is not under the product home: %v", results[0])
	}
	s.HasJSON(results[1], `{"success": false, "exit_code": 1, "output": "before\n"}`)
	if stderr, _ := results[1]["stderr"].(string); !strings.HasPrefix(stderr, "Traceback (most recent call last):") ||
		!strings.HasSuffix(stderr, "RuntimeError: fill_input: element not found: '#missing'") {
		s.Failf("the failing call does not carry the helper's traceback: %v", results[1])
	}
	pages := 0
	for _, req := range shop.Requests() {
		if strings.HasPrefix(req.Path, "/shop") {
			pages++
		}
	}
	if pages == 0 {
		s.Failf("the browser never loaded the shop page")
	}
}
