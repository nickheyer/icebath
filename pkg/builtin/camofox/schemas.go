package camofox

import "github.com/nickheyer/icebath/pkg/internal/pyjson"

// Schema of one tool from its name, description and parameter properties
func toolSchema(name, description string, properties *pyjson.Object, required ...string) *pyjson.Object {
	req := make([]any, 0, len(required))
	for _, r := range required {
		req = append(req, r)
	}
	return pyjson.ObjectOf(
		"name", name,
		"description", description,
		"parameters", pyjson.ObjectOf("type", "object", "properties", properties, "required", req),
	)
}

// Property of a string parameter with its description
func stringParam(description string) *pyjson.Object {
	return pyjson.ObjectOf("type", "string", "description", description)
}

var (
	browserNavigateSchema = toolSchema("browser_navigate",
		"Navigate to a URL in the browser. Initializes the session and loads the page. Must be called before other browser tools. For simple information retrieval, prefer a lightweight retrieval tool when one is available (faster, cheaper). For plain-text endpoints — URLs ending in .md, .txt, .json, .yaml, .yml, .csv, .xml, raw.githubusercontent.com, or any documented API endpoint — prefer an available text-extraction or terminal-fetch tool; the browser stack is overkill and much slower for these. Use browser tools when you need to interact with a page (click, fill forms, dynamic content). Returns a compact page snapshot with interactive elements and ref IDs — no need to call browser_snapshot separately after navigating.",
		pyjson.ObjectOf("url", stringParam("The URL to navigate to (e.g., 'https://example.com')")), "url")

	browserSnapshotSchema = toolSchema("browser_snapshot",
		"Get a text-based snapshot of the current page's accessibility tree. Returns interactive elements with ref IDs (like @e1, @e2) for browser_click and browser_type. Snapshots over 15000 chars are truncated; when that happens the complete snapshot is saved to a file and the output includes its path so you can page through the rest with read_file. Requires browser_navigate first. Note: browser_navigate already returns a snapshot — use this to refresh after interactions that change the page.",
		pyjson.ObjectOf())

	browserClickSchema = toolSchema("browser_click",
		"Click on an element identified by its ref ID from the snapshot (e.g., '@e5'). The ref IDs are shown in square brackets in the snapshot output. Requires browser_navigate and browser_snapshot to be called first.",
		pyjson.ObjectOf("ref", stringParam("The element reference from the snapshot (e.g., '@e5', '@e12')")), "ref")

	browserTypeSchema = toolSchema("browser_type",
		"Type text into an input field identified by its ref ID. Clears the field first, then types the new text. Requires browser_navigate and browser_snapshot to be called first.",
		pyjson.ObjectOf(
			"ref", stringParam("The element reference from the snapshot (e.g., '@e3')"),
			"text", stringParam("The text to type into the field"),
		), "ref", "text")

	browserScrollSchema = toolSchema("browser_scroll",
		"Scroll the page in a direction. Use this to reveal more content that may be below or above the current viewport. Requires browser_navigate to be called first.",
		pyjson.ObjectOf("direction", pyjson.ObjectOf("type", "string", "enum", []any{"up", "down"}, "description", "Direction to scroll")),
		"direction")

	browserBackSchema = toolSchema("browser_back",
		"Navigate back to the previous page in browser history. Requires browser_navigate to be called first.",
		pyjson.ObjectOf())

	browserPressSchema = toolSchema("browser_press",
		"Press a keyboard key. Useful for submitting forms (Enter), navigating (Tab), or keyboard shortcuts. Requires browser_navigate to be called first.",
		pyjson.ObjectOf("key", stringParam("Key to press (e.g., 'Enter', 'Tab', 'Escape', 'ArrowDown')")), "key")

	browserGetImagesSchema = toolSchema("browser_get_images",
		"Get a list of all images on the current page with their URLs and alt text. Useful for finding images to analyze with the vision tool. Requires browser_navigate to be called first.",
		pyjson.ObjectOf())

	browserVisionSchema = toolSchema("browser_vision",
		"Take a screenshot of the current page so you can inspect it visually. Use this when you need to understand what the page looks like - especially for CAPTCHAs, visual verification challenges, complex layouts, or cases where the text snapshot misses important visual information. An auxiliary vision model analyzes the screenshot and returns a text answer to your question. Includes a screenshot_path that you can share with the user by including MEDIA:<screenshot_path> in your response. Requires browser_navigate to be called first.",
		pyjson.ObjectOf("question", stringParam("What you want to know about the page visually. Be specific about what you're looking for.")),
		"question")

	browserConsoleSchema = toolSchema("browser_console",
		"Evaluate JavaScript in the page context and return the result when 'expression' is provided — use this for DOM inspection, reading page state, or extracting data programmatically. Console log and JavaScript error capture are not available with the Camofox backend, so a call without 'expression' returns an empty result. Requires browser_navigate to be called first.",
		pyjson.ObjectOf("expression", stringParam("JavaScript expression to evaluate in the page context. Runs in the browser like DevTools console — full access to DOM, window, document. Return values are serialized to JSON. Example: 'document.title' or 'document.querySelectorAll(\"a\").length'")))

	browserImportCookiesSchema = toolSchema("browser_import_cookies",
		"Import cookies from a Netscape-format cookies.txt file into the current browser session. Use to authenticate to sites without interactive login. Files must live under browser.camofox.cookies_dir (default ~/.camofox/cookies/). Requires CAMOFOX_API_KEY for bearer-token authentication.",
		pyjson.ObjectOf(
			"cookies_path", stringParam("Path to the Netscape cookies.txt file, relative to browser.camofox.cookies_dir (e.g., 'linkedin.txt')"),
			"domain_suffix", stringParam("Optional. Only import cookies whose domain ends with this suffix (e.g., '.linkedin.com')"),
		), "cookies_path")
)
