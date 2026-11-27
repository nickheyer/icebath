package credentials

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/nickheyer/icebath/internal/credentials"
	azurefoundryconfig "github.com/nickheyer/icebath/internal/plugins/defaults/modelproviders/azurefoundry/config"
)

// Sign-in of an Azure Foundry account with Microsoft Entra ID: a service principal the account keeps, or the Azure sign-ins this machine
// already has (az login, azd, Azure PowerShell, a managed identity, the AZURE_ variables), checked by minting a token
var Entra = credentials.SignIn{Spec: azurefoundryconfig.Name, Title: "Microsoft Entra ID", Type: credentials.OAuth, Run: signInEntra}

func signInEntra(p *credentials.Prompter) (credentials.Signed, error) {
	ask := func(label string, secret bool) (string, error) {
		typed, ok := p.Ask(label, secret)
		if !ok {
			return "", credentials.ErrCancelled
		}
		return strings.TrimSpace(typed), nil
	}
	fields := map[string]any{azurefoundryconfig.AuthMode: azurefoundryconfig.EntraID}
	tenant, err := ask("Tenant ID of a service principal (empty for the Azure sign-ins of this machine: az login, managed identity, AZURE_ variables): ", false)
	if err != nil {
		return credentials.Signed{}, err
	}
	if tenant != "" {
		if err := validTenant(tenant); err != nil {
			return credentials.Signed{}, err
		}
		client, err := ask("Client ID: ", false)
		if err != nil {
			return credentials.Signed{}, err
		}
		if client == "" {
			return credentials.Signed{}, errors.New("A service principal needs its client ID.")
		}
		fields[azurefoundryconfig.TenantID], fields[azurefoundryconfig.ClientID] = tenant, client
		secret, err := ask("Client secret (empty to use a certificate): ", true)
		if err != nil {
			return credentials.Signed{}, err
		}
		if secret != "" {
			fields[azurefoundryconfig.ClientSecret] = secret
		} else {
			path, err := ask("Certificate path (PEM or PFX): ", false)
			if err != nil {
				return credentials.Signed{}, err
			}
			if path == "" {
				return credentials.Signed{}, errors.New("A service principal needs its client secret or its certificate.")
			}
			if rest, ok := strings.CutPrefix(path, "~"); ok {
				if home, err := os.UserHomeDir(); err == nil {
					path = filepath.Join(home, rest)
				}
			}
			if absolute, err := filepath.Abs(path); err == nil {
				path = absolute
			}
			fields[azurefoundryconfig.CertificatePath] = path
			password, err := ask("Certificate password (empty for none): ", true)
			if err != nil {
				return credentials.Signed{}, err
			}
			if password != "" {
				fields[azurefoundryconfig.CertificatePassword] = password
			}
		}
	}
	e := environment(func(name string) (string, bool) {
		if field, ok := overlay[name]; ok {
			if value, _ := fields[field].(string); value != "" {
				return value, true
			}
		}
		return os.LookupEnv(name)
	})
	m := &minter{scope: scopeOf(p.Settings[azurefoundryconfig.EntraScope]), own: tenant != ""}
	if m.own {
		m.credential, m.failed = own(e)
	} else {
		var built *chain
		if built, m.failed = defaultChain(e, false, p.Open, func(string) bool { return false }); m.failed == nil {
			m.credential = built
		}
	}
	if m.failed != nil {
		return credentials.Signed{}, m.failed
	}
	ctx := p.Ctx
	if ctx == nil {
		ctx = context.Background()
	}
	p.Say("")
	p.Say("◐ Probing Microsoft Entra ID credential chain (up to %.0fs)...", probeWait.Seconds())
	t, late, err := probe(ctx, m, probeWait)
	if late || err != nil {
		if credentials.Cancelled(err) {
			return credentials.Signed{}, credentials.ErrCancelled
		}
		text, hint := fmt.Sprintf("Token probe timed out after %.0fs", probeWait.Seconds()), probeHint
		if !late {
			text, hint = err.Error(), entraHint
		}
		p.Say("⚠ %s", text)
		p.Say("  Hint: %s", hint)
		answer, err := ask("Save Entra config anyway and validate later? [Y/n]: ", false)
		if err != nil {
			return credentials.Signed{}, err
		}
		switch strings.ToLower(answer) {
		case "", "y", "yes":
			return credentials.Signed{Fields: fields}, nil
		}
		p.Say("Cancelled.")
		return credentials.Signed{}, credentials.ErrCancelled
	}
	via := "default chain"
	if found := sources(e); len(found) > 0 {
		via = strings.Join(found, ", ")
	}
	if m.own {
		via = m.answered()
	}
	p.Say("✓ Entra ID token acquired (%s, scope=%s)", via, m.scope)
	return credentials.Signed{Fields: fields, Identity: identity(t.value)}, nil
}
