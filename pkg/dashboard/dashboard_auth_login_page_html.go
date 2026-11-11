package dashboard

const loginHTMLTemplate = "" +
	"<!doctype html>\n" +
	"<html lang=\"en\">\n" +
	"<head>\n" +
	"<meta charset=\"utf-8\">\n" +
	"<meta name=\"viewport\" content=\"width=device-width, initial-scale=1\">\n" +
	"<title>Sign in \u2014 Icebath Agent</title>\n" +
	"<style>\n" +
	"  /* Brand fonts shipped by @nous-research/ui \u2014 same files the SPA loads. */\n" +
	"  @font-face {{\n" +
	"    font-family: 'Collapse';\n" +
	"    font-style: normal;\n" +
	"    font-weight: 400;\n" +
	"    font-display: swap;\n" +
	"    src: url('/fonts/Collapse-Regular.woff2') format('woff2');\n" +
	"  }}\n" +
	"  @font-face {{\n" +
	"    font-family: 'Collapse';\n" +
	"    font-style: normal;\n" +
	"    font-weight: 700;\n" +
	"    font-display: swap;\n" +
	"    src: url('/fonts/Collapse-Bold.woff2') format('woff2');\n" +
	"  }}\n" +
	"  @font-face {{\n" +
	"    font-family: 'Rules Compressed';\n" +
	"    font-style: normal;\n" +
	"    font-weight: 400;\n" +
	"    font-display: swap;\n" +
	"    src: url('/fonts/RulesCompressed-Regular.woff2') format('woff2');\n" +
	"  }}\n" +
	"  @font-face {{\n" +
	"    font-family: 'Rules Compressed';\n" +
	"    font-style: normal;\n" +
	"    font-weight: 600;\n" +
	"    font-display: swap;\n" +
	"    src: url('/fonts/RulesCompressed-Medium.woff2') format('woff2');\n" +
	"  }}\n" +
	"\n" +
	"  :root {{\n" +
	"    --background-base: #170d02;\n" +
	"    --background: #170d02;\n" +
	"    --midground: #ffac02;\n" +
	"    --foreground: #ffffff;\n" +
	"    --hairline: color-mix(in srgb, #ffac02 18%, transparent);\n" +
	"    --hairline-strong: color-mix(in srgb, #ffac02 35%, transparent);\n" +
	"  }}\n" +
	"\n" +
	"  *, *::before, *::after {{ box-sizing: border-box; }}\n" +
	"\n" +
	"  html, body {{\n" +
	"    margin: 0;\n" +
	"    padding: 0;\n" +
	"    min-height: 100%;\n" +
	"    background: var(--background-base);\n" +
	"    color: var(--foreground);\n" +
	"    font-family: 'Collapse', system-ui, -apple-system, \"Segoe UI\", Roboto, sans-serif;\n" +
	"    font-size: 16px;\n" +
	"    line-height: 1.5;\n" +
	"    -webkit-font-smoothing: antialiased;\n" +
	"    -moz-osx-font-smoothing: grayscale;\n" +
	"  }}\n" +
	"\n" +
	"  /* Subtle dot-grid backdrop \u2014 DS idiom (see `.dither` in globals.css). */\n" +
	"  body {{\n" +
	"    background-image:\n" +
	"      radial-gradient(\n" +
	"        ellipse at top,\n" +
	"        color-mix(in srgb, var(--midground) 6%, transparent) 0%,\n" +
	"        transparent 55%\n" +
	"      ),\n" +
	"      repeating-conic-gradient(\n" +
	"        color-mix(in srgb, var(--midground) 4%, transparent) 0% 25%,\n" +
	"        transparent 0% 50%\n" +
	"      );\n" +
	"    background-size: auto, 3px 3px;\n" +
	"    background-attachment: fixed;\n" +
	"  }}\n" +
	"\n" +
	"  /* Layout: vertically center on tall screens, top-anchor on short. */\n" +
	"  body {{\n" +
	"    display: grid;\n" +
	"    place-items: center;\n" +
	"    padding: clamp(1.5rem, 6vh, 6rem) 1.25rem;\n" +
	"  }}\n" +
	"\n" +
	"  main {{\n" +
	"    width: 100%;\n" +
	"    max-width: 26rem;\n" +
	"    position: relative;\n" +
	"    animation: slide-up 0.6s ease-out both;\n" +
	"  }}\n" +
	"\n" +
	"  @keyframes slide-up {{\n" +
	"    from {{ opacity: 0; transform: translateY(6px); }}\n" +
	"    to   {{ opacity: 1; transform: translateY(0); }}\n" +
	"  }}\n" +
	"\n" +
	"  @media (prefers-reduced-motion: reduce) {{\n" +
	"    main {{ animation: none; }}\n" +
	"  }}\n" +
	"\n" +
	"  /* Brand wordmark above the card \u2014 same uppercase + wide-tracking\n" +
	"     idiom DS Buttons use. */\n" +
	"  .brand {{\n" +
	"    text-align: center;\n" +
	"    margin-bottom: 1.75rem;\n" +
	"    font-family: 'Rules Compressed', 'Collapse', sans-serif;\n" +
	"    font-weight: 600;\n" +
	"    font-size: 1.05rem;\n" +
	"    letter-spacing: 0.32em;\n" +
	"    text-transform: uppercase;\n" +
	"    color: var(--midground);\n" +
	"  }}\n" +
	"  .brand .dot {{\n" +
	"    display: inline-block;\n" +
	"    width: 6px;\n" +
	"    height: 6px;\n" +
	"    background: var(--midground);\n" +
	"    margin: 0 0.55em 0.18em;\n" +
	"    vertical-align: middle;\n" +
	"    border-radius: 1px;\n" +
	"  }}\n" +
	"\n" +
	"  .card {{\n" +
	"    position: relative;\n" +
	"    padding: 2.25rem 2rem 2rem;\n" +
	"    background: color-mix(in srgb, #ffffff 2%, var(--background-base));\n" +
	"    border: 1px solid var(--hairline);\n" +
	"    /* Hairline highlight + bevel shadow \u2014 matches DS Button SHADOW_DEFAULT\n" +
	"       (`inset -1px -1px 0 #00000080, inset 1px 1px 0 #ffffff80`) at panel scale. */\n" +
	"    box-shadow:\n" +
	"      inset 1px 1px 0 0 color-mix(in srgb, #ffffff 5%, transparent),\n" +
	"      inset -1px -1px 0 0 rgba(0, 0, 0, 0.4),\n" +
	"      0 24px 60px -20px rgba(0, 0, 0, 0.6);\n" +
	"  }}\n" +
	"\n" +
	"  h1 {{\n" +
	"    margin: 0 0 0.4rem;\n" +
	"    font-family: 'Rules Compressed', 'Collapse', sans-serif;\n" +
	"    font-weight: 600;\n" +
	"    font-size: 1.85rem;\n" +
	"    letter-spacing: 0.05em;\n" +
	"    text-transform: uppercase;\n" +
	"    color: var(--foreground);\n" +
	"  }}\n" +
	"\n" +
	"  .subtitle {{\n" +
	"    margin: 0 0 1.75rem;\n" +
	"    color: color-mix(in srgb, var(--foreground) 65%, transparent);\n" +
	"    font-size: 0.95rem;\n" +
	"  }}\n" +
	"\n" +
	"  .provider-list {{\n" +
	"    display: grid;\n" +
	"    gap: 0.75rem;\n" +
	"  }}\n" +
	"\n" +
	"  /* Provider button \u2014 mirrors DS Button (default variant):\n" +
	"     amber surface, dark text, uppercase + wide tracking, inset bevel. */\n" +
	"  .provider-btn {{\n" +
	"    display: block;\n" +
	"    width: 100%;\n" +
	"    box-sizing: border-box;\n" +
	"    padding: 0.95rem 1rem;\n" +
	"    text-align: center;\n" +
	"    background: var(--midground);\n" +
	"    color: var(--background-base);\n" +
	"    font-family: 'Collapse', sans-serif;\n" +
	"    font-weight: 700;\n" +
	"    font-size: 0.78rem;\n" +
	"    letter-spacing: 0.2em;\n" +
	"    text-transform: uppercase;\n" +
	"    text-decoration: none;\n" +
	"    border: 0;\n" +
	"    border-radius: 0;  /* DS Button is squared \u2014 no rounded corners. */\n" +
	"    cursor: pointer;\n" +
	"    box-shadow:\n" +
	"      inset 1px 1px 0 0 rgba(255, 255, 255, 0.5),\n" +
	"      inset -1px -1px 0 0 rgba(0, 0, 0, 0.5);\n" +
	"    transition: filter 0.12s ease-out;\n" +
	"  }}\n" +
	"  .provider-btn:hover {{\n" +
	"    filter: brightness(1.08);\n" +
	"  }}\n" +
	"  .provider-btn:active {{\n" +
	"    /* DS Button uses `active:invert` on the default surface. */\n" +
	"    filter: invert(1);\n" +
	"  }}\n" +
	"  .provider-btn:focus-visible {{\n" +
	"    outline: 2px solid var(--midground);\n" +
	"    outline-offset: 3px;\n" +
	"  }}\n" +
	"\n" +
	"  /* Password provider form \u2014 same visual language as the OAuth buttons:\n" +
	"     squared inputs, hairline borders, amber focus ring. */\n" +
	"  .provider-form {{\n" +
	"    display: grid;\n" +
	"    gap: 0.75rem;\n" +
	"    text-align: left;\n" +
	"  }}\n" +
	"  .form-title {{\n" +
	"    font-family: 'Rules Compressed', 'Collapse', sans-serif;\n" +
	"    font-weight: 600;\n" +
	"    font-size: 0.72rem;\n" +
	"    letter-spacing: 0.18em;\n" +
	"    text-transform: uppercase;\n" +
	"    color: color-mix(in srgb, var(--foreground) 70%, transparent);\n" +
	"  }}\n" +
	"  .field {{\n" +
	"    display: grid;\n" +
	"    gap: 0.3rem;\n" +
	"  }}\n" +
	"  .field-label {{\n" +
	"    font-size: 0.72rem;\n" +
	"    letter-spacing: 0.12em;\n" +
	"    text-transform: uppercase;\n" +
	"    color: color-mix(in srgb, var(--foreground) 55%, transparent);\n" +
	"  }}\n" +
	"  .field-input {{\n" +
	"    width: 100%;\n" +
	"    box-sizing: border-box;\n" +
	"    padding: 0.7rem 0.8rem;\n" +
	"    background: color-mix(in srgb, #000000 25%, var(--background-base));\n" +
	"    color: var(--foreground);\n" +
	"    border: 1px solid var(--hairline-strong);\n" +
	"    border-radius: 0;\n" +
	"    font-family: 'Collapse', sans-serif;\n" +
	"    font-size: 0.95rem;\n" +
	"  }}\n" +
	"  .field-input:focus-visible {{\n" +
	"    outline: none;\n" +
	"    border-color: var(--midground);\n" +
	"    box-shadow: 0 0 0 1px var(--midground);\n" +
	"  }}\n" +
	"  .form-error {{\n" +
	"    color: #ff6b6b;\n" +
	"    font-size: 0.82rem;\n" +
	"    letter-spacing: 0.02em;\n" +
	"  }}\n" +
	"  .provider-form .provider-btn {{\n" +
	"    margin-top: 0.25rem;\n" +
	"  }}\n" +
	"\n" +
	"  footer {{\n" +
	"    margin-top: 1.75rem;\n" +
	"    text-align: center;\n" +
	"    color: color-mix(in srgb, var(--foreground) 45%, transparent);\n" +
	"    font-size: 0.75rem;\n" +
	"    letter-spacing: 0.1em;\n" +
	"    text-transform: uppercase;\n" +
	"    line-height: 1.7;\n" +
	"  }}\n" +
	"  footer .sep {{\n" +
	"    display: inline-block;\n" +
	"    width: 1.5rem;\n" +
	"    height: 1px;\n" +
	"    background: var(--hairline-strong);\n" +
	"    vertical-align: middle;\n" +
	"    margin: 0 0.6em 0.2em;\n" +
	"  }}\n" +
	"\n" +
	"  /* Selection \u2014 DS uses midground bg + background text. */\n" +
	"  ::selection {{\n" +
	"    background: var(--midground);\n" +
	"    color: var(--background-base);\n" +
	"  }}\n" +
	"</style>\n" +
	"</head>\n" +
	"<body>\n" +
	"<main>\n" +
	"  <div class=\"brand\">Nous<span class=\"dot\"></span>Research</div>\n" +
	"  <div class=\"card\">\n" +
	"    <h1>Sign in</h1>\n" +
	"    <p class=\"subtitle\">Choose a sign-in method to continue to the Icebath Agent dashboard.</p>\n" +
	"    <div class=\"provider-list\">\n" +
	"{provider_buttons}\n" +
	"    </div>\n" +
	"  </div>\n" +
	"  <footer>\n" +
	"    <span class=\"sep\"></span>Public bind &middot; Auth required<span class=\"sep\"></span>\n" +
	"  </footer>\n" +
	"</main>\n" +
	"{password_script}\n" +
	"</body>\n" +
	"</html>\n"

const loginEmptyHTML = "" +
	"<!doctype html>\n" +
	"<html lang=\"en\">\n" +
	"<head>\n" +
	"<meta charset=\"utf-8\">\n" +
	"<meta name=\"viewport\" content=\"width=device-width, initial-scale=1\">\n" +
	"<title>Sign-in unavailable \u2014 Icebath Agent</title>\n" +
	"<style>\n" +
	"  @font-face {\n" +
	"    font-family: 'Collapse';\n" +
	"    font-style: normal;\n" +
	"    font-weight: 400;\n" +
	"    font-display: swap;\n" +
	"    src: url('/fonts/Collapse-Regular.woff2') format('woff2');\n" +
	"  }\n" +
	"  @font-face {\n" +
	"    font-family: 'Rules Compressed';\n" +
	"    font-style: normal;\n" +
	"    font-weight: 600;\n" +
	"    font-display: swap;\n" +
	"    src: url('/fonts/RulesCompressed-Medium.woff2') format('woff2');\n" +
	"  }\n" +
	"  :root {\n" +
	"    --background-base: #170d02;\n" +
	"    --midground: #ffac02;\n" +
	"    --foreground: #ffffff;\n" +
	"    --hairline: color-mix(in srgb, #ffac02 18%, transparent);\n" +
	"  }\n" +
	"  *, *::before, *::after { box-sizing: border-box; }\n" +
	"  html, body {\n" +
	"    margin: 0; padding: 0; min-height: 100%;\n" +
	"    background: var(--background-base);\n" +
	"    color: var(--foreground);\n" +
	"    font-family: 'Collapse', system-ui, -apple-system, \"Segoe UI\", Roboto, sans-serif;\n" +
	"    font-size: 16px; line-height: 1.5;\n" +
	"    -webkit-font-smoothing: antialiased;\n" +
	"  }\n" +
	"  body {\n" +
	"    display: grid; place-items: center;\n" +
	"    padding: clamp(1.5rem, 6vh, 6rem) 1.25rem;\n" +
	"  }\n" +
	"  main {\n" +
	"    width: 100%; max-width: 32rem;\n" +
	"    padding: 2.25rem 2rem;\n" +
	"    background: color-mix(in srgb, #ffffff 2%, var(--background-base));\n" +
	"    border: 1px solid var(--hairline);\n" +
	"    box-shadow:\n" +
	"      inset 1px 1px 0 0 color-mix(in srgb, #ffffff 5%, transparent),\n" +
	"      inset -1px -1px 0 0 rgba(0, 0, 0, 0.4),\n" +
	"      0 24px 60px -20px rgba(0, 0, 0, 0.6);\n" +
	"  }\n" +
	"  h1 {\n" +
	"    margin: 0 0 1rem;\n" +
	"    font-family: 'Rules Compressed', 'Collapse', sans-serif;\n" +
	"    font-weight: 600; font-size: 1.5rem;\n" +
	"    letter-spacing: 0.05em; text-transform: uppercase;\n" +
	"    color: var(--midground);\n" +
	"  }\n" +
	"  p { margin: 0 0 1rem; }\n" +
	"  code {\n" +
	"    background: var(--midground);\n" +
	"    color: var(--background-base);\n" +
	"    padding: 0.1em 0.35em;\n" +
	"    font-family: 'Courier New', monospace;\n" +
	"    font-size: 0.9em;\n" +
	"  }\n" +
	"  a { color: var(--midground); }\n" +
	"</style>\n" +
	"</head>\n" +
	"<body>\n" +
	"<main>\n" +
	"<h1>Sign-in unavailable</h1>\n" +
	"<p>This dashboard is bound to a non-loopback host but no authentication\n" +
	"providers are available.</p>\n" +
	"<p>Configure the bundled username/password provider or an OAuth provider.\n" +
	"See the <a href=\"https://hermes-agent.nousresearch.com/docs/user-guide/features/web-dashboard#authentication-gated-mode\">dashboard\n" +
	"authentication documentation</a> for setup instructions.</p>\n" +
	"<p>For auth-free local use, bind to <code>127.0.0.1</code> and connect through\n" +
	"an SSH tunnel or Tailscale.</p>\n" +
	"</main>\n" +
	"</body>\n" +
	"</html>\n"

const loginTokenModeHTML = "" +
	"<!doctype html>\n" +
	"<html lang=\"en\">\n" +
	"<head>\n" +
	"<meta charset=\"utf-8\">\n" +
	"<meta name=\"viewport\" content=\"width=device-width, initial-scale=1\">\n" +
	"<title>Token-gated dashboard \u2014 Icebath Agent</title>\n" +
	"<style>\n" +
	"  :root {\n" +
	"    --background-base: #170d02;\n" +
	"    --midground: #ffac02;\n" +
	"    --foreground: #ffffff;\n" +
	"    --hairline: color-mix(in srgb, #ffac02 18%, transparent);\n" +
	"  }\n" +
	"  *, *::before, *::after { box-sizing: border-box; }\n" +
	"  html, body {\n" +
	"    margin: 0; padding: 0; min-height: 100%;\n" +
	"    background: var(--background-base);\n" +
	"    color: var(--foreground);\n" +
	"    font-family: 'Collapse', system-ui, -apple-system, \"Segoe UI\", Roboto, sans-serif;\n" +
	"    font-size: 16px; line-height: 1.5;\n" +
	"    -webkit-font-smoothing: antialiased;\n" +
	"  }\n" +
	"  body {\n" +
	"    display: grid; place-items: center;\n" +
	"    padding: clamp(1.5rem, 6vh, 6rem) 1.25rem;\n" +
	"  }\n" +
	"  main {\n" +
	"    width: 100%; max-width: 32rem;\n" +
	"    padding: 2.25rem 2rem;\n" +
	"    background: color-mix(in srgb, #ffffff 2%, var(--background-base));\n" +
	"    border: 1px solid var(--hairline);\n" +
	"    box-shadow:\n" +
	"      inset 1px 1px 0 0 color-mix(in srgb, #ffffff 5%, transparent),\n" +
	"      inset -1px -1px 0 0 rgba(0, 0, 0, 0.4),\n" +
	"      0 24px 60px -20px rgba(0, 0, 0, 0.6);\n" +
	"  }\n" +
	"  h1 {\n" +
	"    margin: 0 0 1rem;\n" +
	"    font-family: 'Rules Compressed', 'Collapse', sans-serif;\n" +
	"    font-weight: 600; font-size: 1.5rem;\n" +
	"    letter-spacing: 0.05em; text-transform: uppercase;\n" +
	"    color: var(--midground);\n" +
	"  }\n" +
	"  p { margin: 0 0 1rem; }\n" +
	"  code {\n" +
	"    background: var(--midground);\n" +
	"    color: var(--background-base);\n" +
	"    padding: 0.1em 0.35em;\n" +
	"    font-family: 'Courier New', monospace;\n" +
	"    font-size: 0.9em;\n" +
	"  }\n" +
	"</style>\n" +
	"</head>\n" +
	"<body>\n" +
	"<main>\n" +
	"<h1>Token-gated dashboard</h1>\n" +
	"<p>This deployment authenticates with a provisioned credential\n" +
	"({provider_names}), not a browser sign-in.</p>\n" +
	"<p>{note}</p>\n" +
	"</main>\n" +
	"</body>\n" +
	"</html>\n"

const loginPasswordFormScript = "" +
	"<script>\n" +
	"(function () {\n" +
	"  var mainEl = document.querySelector('main');\n" +
	"  var prefix = (mainEl && mainEl.getAttribute('data-prefix')) || '';\n" +
	"  function handle(form) {\n" +
	"    form.addEventListener('submit', function (ev) {\n" +
	"      ev.preventDefault();\n" +
	"      var err = form.querySelector('.form-error');\n" +
	"      var btn = form.querySelector('button[type=submit]');\n" +
	"      if (err) { err.hidden = true; err.textContent = ''; }\n" +
	"      if (btn) { btn.disabled = true; }\n" +
	"      var body = {\n" +
	"        provider: form.getAttribute('data-provider') || '',\n" +
	"        username: (form.querySelector('input[name=username]') || {}).value || '',\n" +
	"        password: (form.querySelector('input[name=password]') || {}).value || '',\n" +
	"        next: (form.querySelector('input[name=next]') || {}).value || ''\n" +
	"      };\n" +
	"      fetch(prefix + '/auth/password-login', {\n" +
	"        method: 'POST',\n" +
	"        headers: { 'Content-Type': 'application/json' },\n" +
	"        body: JSON.stringify(body),\n" +
	"        credentials: 'same-origin'\n" +
	"      }).then(function (resp) {\n" +
	"        if (resp.ok) {\n" +
	"          return resp.json().then(function (data) {\n" +
	"            window.location.assign((data && data.next) || (prefix + '/'));\n" +
	"          });\n" +
	"        }\n" +
	"        var msg = resp.status === 429\n" +
	"          ? 'Too many attempts. Please wait and try again.'\n" +
	"          : (resp.status === 401 ? 'Invalid username or password.'\n" +
	"                                 : 'Sign-in failed. Please try again.');\n" +
	"        if (err) { err.textContent = msg; err.hidden = false; }\n" +
	"        if (btn) { btn.disabled = false; }\n" +
	"      }).catch(function () {\n" +
	"        if (err) { err.textContent = 'Network error. Please try again.'; err.hidden = false; }\n" +
	"        if (btn) { btn.disabled = false; }\n" +
	"      });\n" +
	"    });\n" +
	"  }\n" +
	"  var forms = document.querySelectorAll('form.provider-form');\n" +
	"  for (var i = 0; i < forms.length; i++) { handle(forms[i]); }\n" +
	"})();\n" +
	"</script>\n"
