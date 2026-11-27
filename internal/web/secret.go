package web

import (
	"regexp"
	"strings"
	"unicode/utf8"
)

// Shapes of the API keys and tokens of known services, by the prefix they start with
var secretShapes = regexp.MustCompile("(?:" + strings.Join([]string{
	`sk-[A-Za-z0-9_-](?:\.?[A-Za-z0-9_-]){9,}`,
	`ctx7sk-[A-Za-z0-9_-]{10,}`,
	`ghp_[A-Za-z0-9]{10,}`, `github_pat_[A-Za-z0-9_]{10,}`, `gho_[A-Za-z0-9]{10,}`, `ghu_[A-Za-z0-9]{10,}`,
	`ghs_[A-Za-z0-9]{10,}`, `ghr_[A-Za-z0-9]{10,}`,
	`xapp-\d+-[A-Za-z0-9-]{10,}`, `xox[baprs]-[A-Za-z0-9-]{10,}`,
	`AIza[A-Za-z0-9_-]{30,}`, `pplx-[A-Za-z0-9]{10,}`, `fal_[A-Za-z0-9_-]{10,}`, `fc-[A-Za-z0-9]{10,}`,
	`bb_live_[A-Za-z0-9_-]{10,}`, `gAAAA[A-Za-z0-9_=-]{20,}`, `AKIA[A-Z0-9]{16}`,
	`sk_live_[A-Za-z0-9]{10,}`, `sk_test_[A-Za-z0-9]{10,}`, `rk_live_[A-Za-z0-9]{10,}`,
	`SG\.[A-Za-z0-9_-]{10,}`, `hf_[A-Za-z0-9]{10,}`, `r8_[A-Za-z0-9]{10,}`, `npm_[A-Za-z0-9]{10,}`,
	`pypi-[A-Za-z0-9_-]{10,}`, `dop_v1_[A-Za-z0-9]{10,}`, `doo_v1_[A-Za-z0-9]{10,}`,
	`am_(?:org_)?[A-Za-z0-9]{20,}`, `sk_[A-Za-z0-9_]{10,}`, `tvly-[A-Za-z0-9]{10,}`, `exa_[A-Za-z0-9]{10,}`,
	`gsk_[A-Za-z0-9]{10,}`, `syt_[A-Za-z0-9]{10,}`, `retaindb_[A-Za-z0-9]{10,}`, `hsk-[A-Za-z0-9]{10,}`,
	`mem0_[A-Za-z0-9]{10,}`, `brv_[A-Za-z0-9]{10,}`, `xai-[A-Za-z0-9]{30,}`, `ntn_[A-Za-z0-9]{10,}`,
	`fw-[A-Za-z0-9]{30,}`, `fw_[A-Za-z0-9]{30,}`, `fpk_[A-Za-z0-9]{30,}`,
	`glpat-[A-Za-z0-9_\-]{10,}`, `gloas-[A-Za-z0-9_\-]{10,}`, `gldt-[A-Za-z0-9_\-]{10,}`, `glrt-[A-Za-z0-9_.\-]{10,}`,
	`glrtr-[A-Za-z0-9_.\-]{10,}`, `glcbt-[A-Za-z0-9_\-]{10,}`, `glptt-[A-Za-z0-9_\-]{10,}`, `glft-[A-Za-z0-9_\-]{10,}`,
	`glimt-[A-Za-z0-9_\-]{10,}`, `glagent-[A-Za-z0-9_\-]{10,}`, `glsoat-[A-Za-z0-9_\-]{10,}`, `glffct-[A-Za-z0-9_\-]{10,}`,
	`glwt-[A-Za-z0-9_\-]{10,}`, `GR1348941[A-Za-z0-9_\-]{10,}`, `pk-lf-[A-Za-z0-9\-]{8,}`,
}, "|") + ")")

// Whether the text carries what looks like an API key or token: a known prefix and body, not glued to more identifier characters
func CarriesSecret(text string) bool {
	for at := 0; at <= len(text); {
		match := secretShapes.FindStringIndex(text[at:])
		if match == nil {
			return false
		}
		start, end := at+match[0], at+match[1]
		before, _ := utf8.DecodeLastRuneInString(text[:start])
		after, _ := utf8.DecodeRuneInString(text[end:])
		if !(start > 0 && token(before)) && !(end < len(text) && token(after)) {
			return true
		}
		_, size := utf8.DecodeRuneInString(text[start:])
		at = start + max(size, 1)
	}
	return false
}

func token(r rune) bool {
	return r == '_' || r == '-' || 'A' <= r && r <= 'Z' || 'a' <= r && r <= 'z' || '0' <= r && r <= '9'
}
