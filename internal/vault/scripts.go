package vault

import (
	"strings"

	"github.com/nickheyer/icebath/internal/fileutil"
)

// Page script that stamps every input with its nonce and index and reports the visible controls as JSON
const inspectionTemplate = `(() => {
  const nonce = __NONCE__;
  // Shadow-piercing collect: many logins (Home Assistant, custom-element SPAs) render their
  // form inside a shadow root, where document.querySelectorAll never sees it. Walk every
  // element (shadow hosts can be anything, not just inputs) and recurse into open shadow roots.
  // Forms are collected in the same walk: document.forms misses a <form> inside a shadow root, which
  // would leave its controls ungrouped (formIndex null) and merge split OTP boxes across forms.
  const elements = [];
  const forms = [];
  (function walk(root) {
    for (const e of root.querySelectorAll("*")) {
      if (e.tagName === "INPUT" || e.tagName === "SELECT") elements.push(e);
      else if (e.tagName === "FORM") forms.push(e);
      if (e.shadowRoot) walk(e.shadowRoot);
    }
  })(document);
  elements.forEach((element, index) => element.setAttribute("data-icebath-vault-slot", nonce + ":" + index));
  const out = elements.flatMap((element, index) => {
    if (element.disabled || element.readOnly) return [];
    if (["hidden", "submit", "button", "reset", "file", "image", "checkbox", "radio"].includes(element.type)) return [];
    const style = getComputedStyle(element);
    if (style.display === "none" || style.visibility === "hidden" || element.getClientRects().length === 0) return [];
    const labels = element.labels ? Array.from(element.labels, (l) => l.textContent || "") : [];
    const ariaText = (element.getAttribute("aria-labelledby") || "")
      .split(/\s+/).filter(Boolean)
      .map((id) => { const r = element.getRootNode(); const n = (r.getElementById ? r.getElementById(id) : null) || document.getElementById(id); return n ? (n.textContent || "") : ""; })
      .join(" ");
    const resolvedFormIndex = element.form ? forms.indexOf(element.form) : -1;
    return [{
      autocomplete: element.autocomplete || "",
      formIndex: resolvedFormIndex >= 0 ? resolvedFormIndex : null,
      index,
      maxLength: element.maxLength > 0 ? element.maxLength : null,
      label: [
        ...labels,
        element.getAttribute("aria-label") || "",
        ariaText,
        element.getAttribute("placeholder") || "",
        element.getAttribute("title") || "",
      ].join(" "),
      name: [element.name, element.id].join(" "),
      type: element.tagName === "SELECT" ? "select" : (element.type || ""),
    }];
  });
  return JSON.stringify(out);
})()`

// Page script writing the fills only while the page stays on the expected origin, reporting only a count
const fillTemplate = `(() => {
  const expectedOrigin = __EXPECTED_ORIGIN__;
  if (window.location.origin !== expectedOrigin) {
    return JSON.stringify({ refused: "origin_changed", found: window.location.origin });
  }
  const fills = __FILLS__;
  const nonce = __NONCE__;
  // Stamped controls may live inside a shadow root (see the inspection script's walk): collect the
  // document and every open shadow root once, then look each stamp up in those roots.
  const roots = [];
  (function walk(root) {
    roots.push(root);
    for (const e of root.querySelectorAll("*")) if (e.shadowRoot) walk(e.shadowRoot);
  })(document);
  const find = (selector) => roots.flatMap((r) => Array.from(r.querySelectorAll(selector)));
  let filled = 0;
  const norm = (t) => String(t || "").trim().toLowerCase();
  for (const f of fills) {
    const el = find('[data-icebath-vault-slot="' + nonce + ':' + f.index + '"]')[0];
    if (!el || (f.token === "current-password" && el.type !== "password")) continue;
    try {
      if (el.tagName === "SELECT") {
        const want = norm(f.value);
        const opt = Array.from(el.options).find((o) => [o.value, o.textContent].some((t) => norm(t) === want || norm(t) === want.replace(/^0/, "")));
        if (opt) { el.value = opt.value; el.dispatchEvent(new Event("change", { bubbles: true, composed: true })); filled += 1; }
        continue;
      }
      el.focus();
      const setter = Object.getOwnPropertyDescriptor(HTMLInputElement.prototype, "value");
      // one-time-code split into single-character boxes: f.value is the slice for THIS box (see build_otp_fills)
      if (setter && setter.set) { setter.set.call(el, f.value); } else { el.value = f.value; }
      // composed: a component listening on its shadow host must see the value change too.
      el.dispatchEvent(new InputEvent("input", { bubbles: true, composed: true, inputType: "insertText" }));
      el.dispatchEvent(new Event("change", { bubbles: true, composed: true }));
      if (el.value.length > 0) filled += 1;
    } catch (e) { /* skip */ }
  }
  find("[data-icebath-vault-slot]").forEach((n) => n.removeAttribute("data-icebath-vault-slot"));
  return JSON.stringify({ filled });
})()`

// One fill as the fill script reads it
type fillWire struct {
	Index int    `json:"index"`
	Token string `json:"token"`
	Value string `json:"value"`
}

// Inspection script bound to nonce, whose stamps the matching fill script resolves its targets by
func InspectionScript(nonce string) (string, error) {
	quoted, err := fileutil.EncodeJSON(nonce, referenceStyle)
	if err != nil {
		return "", err
	}
	return strings.ReplaceAll(inspectionTemplate, "__NONCE__", string(quoted)), nil
}

// Script filling controls the inspection under nonce stamped, values substituted in one pass and never rescanned
func FillScript(fills []Fill, expectedOrigin, nonce string) (string, error) {
	list := make([]fillWire, len(fills))
	for i, f := range fills {
		list[i] = fillWire{Index: f.Index, Token: f.Token, Value: f.text()}
	}
	values, err := fileutil.EncodeJSON(list, referenceStyle)
	if err != nil {
		return "", err
	}
	origin, err := fileutil.EncodeJSON(expectedOrigin, referenceStyle)
	if err != nil {
		return "", err
	}
	quoted, err := fileutil.EncodeJSON(nonce, referenceStyle)
	if err != nil {
		return "", err
	}
	return strings.NewReplacer("__EXPECTED_ORIGIN__", string(origin), "__FILLS__", string(values),
		"__NONCE__", string(quoted)).Replace(fillTemplate), nil
}
