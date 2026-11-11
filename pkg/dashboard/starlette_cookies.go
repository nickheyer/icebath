package dashboard

import (
	"fmt"
	"slices"
	"strconv"
	"strings"
	"time"
)

// Characters a cookie value keeps unquoted, the http.cookies legal set
const cookieLegalChars = "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789!#$%&'*+-.^_`|~:"

// Characters a quoted cookie value keeps unescaped
const cookieUnescapedChars = cookieLegalChars + " ()/<=>?@[]{}"

// Attributes of one Set-Cookie header, nil pointers leaving an attribute out
type CookieOptions struct {
	MaxAge   *int
	Expires  *int
	Path     *string
	Domain   *string
	Secure   bool
	HTTPOnly bool
	SameSite *string
}

// Value as http.cookies quotes it, bare when every character is legal and octal escaped in quotes otherwise
func cookieQuote(s string) string {
	if s != "" && strings.Trim(s, cookieLegalChars) == "" {
		return s
	}
	var b strings.Builder
	b.WriteByte('"')
	for _, r := range s {
		switch {
		case r == '"':
			b.WriteString(`\"`)
		case r == '\\':
			b.WriteString(`\\`)
		case r < 256 && !strings.ContainsRune(cookieUnescapedChars, r):
			fmt.Fprintf(&b, "\\%03o", r)
		default:
			b.WriteRune(r)
		}
	}
	b.WriteByte('"')
	return b.String()
}

// Expiry date http.cookies renders for an integer offset in seconds from now
func cookieDate(offset int) string {
	return time.Now().Add(time.Duration(offset) * time.Second).UTC().Format("Mon, 02 Jan 2006 15:04:05 GMT")
}

// Set-Cookie header value of a morsel, attributes in the sorted key order http.cookies outputs
func cookieHeaderValue(key, value string, o CookieOptions) string {
	parts := []string{key + "=" + cookieQuote(value)}
	attrs := map[string]string{}
	if o.Domain != nil && *o.Domain != "" {
		attrs["domain"] = "Domain=" + *o.Domain
	}
	if o.Expires != nil {
		attrs["expires"] = "expires=" + cookieDate(*o.Expires)
	}
	if o.HTTPOnly {
		attrs["httponly"] = "HttpOnly"
	}
	if o.MaxAge != nil {
		attrs["max-age"] = "Max-Age=" + strconv.Itoa(*o.MaxAge)
	}
	if o.Path != nil && *o.Path != "" {
		attrs["path"] = "Path=" + *o.Path
	}
	if o.SameSite != nil && *o.SameSite != "" {
		attrs["samesite"] = "SameSite=" + *o.SameSite
	}
	if o.Secure {
		attrs["secure"] = "Secure"
	}
	keys := make([]string, 0, len(attrs))
	for k := range attrs {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	for _, k := range keys {
		parts = append(parts, attrs[k])
	}
	return strings.Join(parts, "; ")
}

// Appends a Set-Cookie header to the response the way Starlette's set_cookie does
func SetCookie(r ASGIResponse, key, value string, o CookieOptions) {
	if o.SameSite != nil && !slices.Contains([]string{"strict", "lax", "none"}, strings.ToLower(*o.SameSite)) {
		panic("samesite must be either 'strict', 'lax' or 'none'")
	}
	r.Header().Append("set-cookie", cookieHeaderValue(key, value, o))
}

// Expires a cookie now through a zero Max-Age and an expiry of the current time
func DeleteCookie(r ASGIResponse, key string, o CookieOptions) {
	zero := 0
	o.MaxAge, o.Expires = &zero, &zero
	SetCookie(r, key, "", o)
}
