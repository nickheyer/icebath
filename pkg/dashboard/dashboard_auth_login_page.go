package dashboard

import (
	"context"
	"strings"

	"github.com/nickheyer/icebath/pkg/gateway"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pyurl"
)

var (
	htmlEscapeQuote   = strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;", `"`, "&quot;", "'", "&#x27;")
	htmlEscapeNoQuote = strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;")
)

// Text escaped like html.escape, quotes included when quote is set
func pyHTMLEscape(s string, quote bool) string {
	if quote {
		return htmlEscapeQuote.Replace(s)
	}
	return htmlEscapeNoQuote.Replace(s)
}

// Login page with a button or password form per interactive provider, the token or empty page when none
func renderLoginHTML(ctx context.Context, nextPath, prefix string) (string, error) {
	providers := gateway.ListSessionDashboardAuthProviders(ctx)
	if len(providers) == 0 {
		return loginEmptyHTML, nil
	}
	nextQS := ""
	if nextPath != "" {
		nextQS = "&next=" + pyHTMLEscape(pyurl.Quote(nextPath, ""), true)
	}
	interactive := gateway.ListInteractiveDashboardAuthProviders(ctx)
	if len(interactive) == 0 {
		names := make([]string, len(providers))
		for i, p := range providers {
			names[i] = pyHTMLEscape(p.DisplayName(), true)
		}
		return strings.ReplaceAll(strings.ReplaceAll(loginTokenModeHTML,
			"{provider_names}", strings.Join(names, ", ")),
			"{note}", "Clients authenticate with the deployment&rsquo;s provisioned "+
				"credential (<code>Authorization: Bearer</code>, or "+
				"<code>?token=</code> on the /api/ws upgrade) &mdash; e.g. Icebath "+
				"Desktop&rsquo;s &ldquo;Session token&rdquo; remote-gateway field. "+
				"There is no browser sign-in flow."), nil
	}
	buttons := make([]string, len(interactive))
	needsPasswordScript := false
	for i, p := range interactive {
		if gateway.DashboardAuthSupportsPassword(p) {
			buttons[i] = renderPasswordForm(p, nextPath)
			needsPasswordScript = true
			continue
		}
		buttons[i] = `      <a class="provider-btn" href="` + prefix + "/auth/login?provider=" + pyHTMLEscape(p.Name(), true) +
			nextQS + `">Sign in with ` + pyHTMLEscape(p.DisplayName(), true) + "</a>"
	}
	script := ""
	if needsPasswordScript {
		script = loginPasswordFormScript
	}
	out, err := pyrepr.FormatMap(loginHTMLTemplate, map[string]any{"provider_buttons": strings.Join(buttons, "\n"), "password_script": script})
	if err != nil {
		return "", err
	}
	return applyProxyPrefix(out, prefix), nil
}

// Rendered login page with the main element tagged with the prefix and font URLs moved under it
func applyProxyPrefix(htmlOut, prefix string) string {
	if prefix == "" {
		return htmlOut
	}
	htmlOut = strings.Replace(htmlOut, "<main>", `<main data-prefix="`+prefix+`">`, 1)
	return strings.ReplaceAll(htmlOut, "url('/fonts/", "url('"+prefix+"/fonts/")
}

// Provider picker re-entering native authorize with the same desktop PKCE inputs and an explicit provider
func renderNativeProviderChoiceHTML(providers []gateway.DashboardAuthProvider, authorizePath, codeChallenge,
	codeChallengeMethod, redirectURI, state, prefix string) (string, error) {
	var buttons []string
	for _, p := range providers {
		query := pyurl.URLEncode([][2]string{{"code_challenge", codeChallenge}, {"code_challenge_method", codeChallengeMethod},
			{"redirect_uri", redirectURI}, {"state", state}, {"provider", p.Name()}})
		href := pyHTMLEscape(authorizePath+"?"+query, true)
		buttons = append(buttons, `      <a class="provider-btn" href="`+href+`">Sign in with `+pyHTMLEscape(p.DisplayName(), true)+"</a>")
	}
	if len(buttons) == 0 {
		return loginEmptyHTML, nil
	}
	out, err := pyrepr.FormatMap(loginHTMLTemplate, map[string]any{"provider_buttons": strings.Join(buttons, "\n"), "password_script": ""})
	if err != nil {
		return "", err
	}
	return applyProxyPrefix(out, prefix), nil
}

// Username and password form of a password provider with the next path in a hidden field
func renderPasswordForm(provider gateway.DashboardAuthProvider, nextPath string) string {
	pname := pyHTMLEscape(provider.Name(), true)
	plabel := pyHTMLEscape(provider.DisplayName(), true)
	safeNext := ""
	if nextPath != "" {
		safeNext = pyHTMLEscape(nextPath, true)
	}
	return `      <form class="provider-form" data-provider="` + pname + `" ` +
		"autocomplete=\"on\">\n" +
		`        <div class="form-title">Sign in with ` + plabel + "</div>\n" +
		`        <input type="hidden" name="next" value="` + safeNext + "\">\n" +
		"        <label class=\"field\">\n" +
		"          <span class=\"field-label\">Username</span>\n" +
		`          <input class="field-input" type="text" name="username" ` +
		`autocomplete="username" autocapitalize="none" ` +
		"autocorrect=\"off\" spellcheck=\"false\" required>\n" +
		"        </label>\n" +
		"        <label class=\"field\">\n" +
		"          <span class=\"field-label\">Password</span>\n" +
		`          <input class="field-input" type="password" name="password" ` +
		"autocomplete=\"current-password\" required>\n" +
		"        </label>\n" +
		"        <div class=\"form-error\" role=\"alert\" hidden></div>\n" +
		"        <button class=\"provider-btn\" type=\"submit\">Sign in</button>\n" +
		"      </form>"
}
