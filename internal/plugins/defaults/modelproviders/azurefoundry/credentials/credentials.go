package credentials

import (
	"cmp"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/nickheyer/icebath/internal/credentials"
	azurefoundryconfig "github.com/nickheyer/icebath/internal/plugins/defaults/modelproviders/azurefoundry/config"
	"github.com/nickheyer/icebath/internal/provider"
	providerconfig "github.com/nickheyer/icebath/internal/provider/config"
	browserutil "github.com/nickheyer/icebath/pkg/browser"
	"github.com/nickheyer/icebath/pkg/strs"
)

const (
	// How long minting a token may take, and with the browser sign-in in the chain, which waits five minutes for the person
	mintWait    = 30 * time.Second
	browserWait = 6 * time.Minute
	// How long a status waits for a token
	probeWait = 10 * time.Second
	// How long a credential that couldn't be built is believed before it is built again
	failedFor = time.Minute
	// Hint of a token the chain can't mint
	entraHint = "Run `az login`, give this virtual machine a managed identity, or set AZURE_TENANT_ID, AZURE_CLIENT_ID and AZURE_CLIENT_SECRET."
)

// Variables the Azure credentials read
var variables = []string{"AZURE_TENANT_ID", "AZURE_CLIENT_ID", "AZURE_CLIENT_SECRET", "AZURE_CLIENT_CERTIFICATE_PATH", "AZURE_CLIENT_CERTIFICATE_PASSWORD",
	"AZURE_CLIENT_SEND_CERTIFICATE_CHAIN", "AZURE_USERNAME", "AZURE_PASSWORD", "AZURE_FEDERATED_TOKEN_FILE", "AZURE_AUTHORITY_HOST", "AZURE_TOKEN_CREDENTIALS",
	"AZURE_POD_IDENTITY_AUTHORITY_HOST", "IDENTITY_ENDPOINT", "IDENTITY_HEADER", "IDENTITY_SERVER_THUMBPRINT", "IMDS_ENDPOINT", "MSI_ENDPOINT", "MSI_SECRET",
	"DEFAULT_IDENTITY_CLIENT_ID"}

// Fields of an account that stand in for variables
var overlay = map[string]string{"AZURE_TENANT_ID": azurefoundryconfig.TenantID, "AZURE_CLIENT_ID": azurefoundryconfig.ClientID,
	"AZURE_CLIENT_SECRET": azurefoundryconfig.ClientSecret, "AZURE_CLIENT_CERTIFICATE_PATH": azurefoundryconfig.CertificatePath,
	"AZURE_CLIENT_CERTIFICATE_PASSWORD": azurefoundryconfig.CertificatePassword}

// Members whose sign-in the profile's variables hold, rather than another program or the machine
var environmental = map[string]bool{"EnvironmentCredential": true, "ClientSecretCredential": true, "CertificateCredential": true,
	"WorkloadIdentityCredential": true, "UsernamePasswordCredential": true}

// Azure Foundry credentials: an API key of the account or of AZURE_FOUNDRY_API_KEY, or Microsoft Entra ID tokens of the account's service
// principal or of the default Azure credential chain
type source struct {
	mu sync.Mutex
	// Entra credential of each account, by its provider and name
	minters map[string]*minter
}

// Entra credential built for one account's variables and scope, and the token it minted last
type minter struct {
	mu sync.Mutex
	// What the credential was built from, which it is built again for once it changes
	key        string
	credential entra
	// Why the credential couldn't be built, and when
	failed error
	at     time.Time
	scope  string
	// Built from the account's own service principal
	own     bool
	current token
	// Why the last token couldn't be minted
	err error
}

func Register(registry *credentials.Registry) {
	registry.Register(&source{minters: map[string]*minter{}})
}

// Way the account signs in: its own, else its provider's, else with an API key
func Mode(account credentials.Account) string {
	for _, mode := range []string{account.Fields.GetString(azurefoundryconfig.AuthMode), account.Provider.Settings.GetString(azurefoundryconfig.AuthMode)} {
		if mode = strings.ToLower(strings.TrimSpace(mode)); mode != "" {
			return mode
		}
	}
	return azurefoundryconfig.APIKey
}

// Scope of Entra tokens the provider's setting names, Azure AI Foundry's when it names none
func scopeOf(setting string) string {
	return cmp.Or(strings.TrimSpace(setting), DefaultScope)
}

// Variables of the account's profile, its own service principal's fields standing in for theirs; a variable the provider ignores reads
// as unset. Whether the account has a service principal of its own
func env(account credentials.Account) (environment, bool) {
	owned := false
	if account.Name != "" {
		for _, field := range overlay {
			owned = owned || strings.TrimSpace(account.Fields.GetString(field)) != ""
		}
	}
	return func(name string) (string, bool) {
		if field, ok := overlay[name]; ok && account.Name != "" {
			if value := strings.TrimSpace(account.Fields.GetString(field)); value != "" {
				return value, true
			}
		}
		if account.Ignores(name) {
			return "", false
		}
		return account.Fields.LookupEnv(name)
	}, owned
}

// Opens the browser sign-in of a chain AZURE_TOKEN_CREDENTIALS puts it in
func open(link string) error {
	return browserutil.Open(context.Background(), link)
}

// Entra credential of the account, built again when what it is built from changes or a minute after it failed to be
func (s *source) minter(account credentials.Account) *minter {
	e, owned := env(account)
	entraScope := scopeOf(account.Provider.Settings.GetString(azurefoundryconfig.EntraScope))
	h := sha256.New()
	fmt.Fprintf(h, "%s\x00%t\x00%s", entraScope, owned, strings.Join(account.Provider.Settings.GetStringSlice(providerconfig.Ignored), "\x00"))
	for _, name := range variables {
		value, ok := e(name)
		fmt.Fprintf(h, "\x00%s=%t:%s", name, ok, value)
	}
	key := hex.EncodeToString(h.Sum(nil))
	id := account.Provider.ID + "\x00" + account.Name
	s.mu.Lock()
	defer s.mu.Unlock()
	if m, ok := s.minters[id]; ok && m.key == key && (m.failed == nil || time.Since(m.at) < failedFor) {
		return m
	}
	m := &minter{key: key, scope: entraScope, own: owned, at: time.Now()}
	if owned {
		m.credential, m.failed = own(e)
	} else {
		var built *chain
		if built, m.failed = defaultChain(e, false, open, account.Ignores); m.failed == nil {
			m.credential = built
		}
	}
	s.minters[id] = m
	return m
}

// Token of the credential, minted again when it is near expiry or past its refresh_on, or when forced after the last was refused
func (m *minter) get(ctx context.Context, force bool) (token, error) {
	if m.failed != nil {
		return token{}, m.failed
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	now := time.Now().Unix()
	if !force && m.current.value != "" && (m.current.refreshOn == 0 || m.current.refreshOn > now) && m.current.expiresOn-now >= refreshOffset {
		return m.current, nil
	}
	if f, ok := m.credential.(forgetter); ok && force {
		f.forget()
	}
	fresh, err := m.credential.token(ctx, []string{m.scope})
	if err != nil {
		m.err = err
		return token{}, err
	}
	m.current, m.err = fresh, nil
	return fresh, nil
}

// How long minting may take
func (m *minter) wait() time.Duration {
	if c, ok := m.credential.(*chain); ok && c.interactive() {
		return browserWait
	}
	return mintWait
}

// Name of the credential that minted, the chain's member that answered
func (m *minter) answered() string {
	if c, ok := m.credential.(*chain); ok {
		return c.answered()
	}
	if m.credential == nil {
		return ""
	}
	return m.credential.name()
}

// Token the minter has within the timeout, timed out when it takes longer
func probe(ctx context.Context, m *minter, timeout time.Duration) (token, bool, error) {
	type result struct {
		t   token
		err error
	}
	done := make(chan result, 1)
	go func() {
		t, err := m.get(ctx, false)
		done <- result{t, err}
	}()
	timer := time.NewTimer(max(timeout, 10*time.Millisecond))
	defer timer.Stop()
	select {
	case r := <-done:
		return r.t, false, r.err
	case <-ctx.Done():
		return token{}, false, ctx.Err()
	case <-timer.C:
		return token{}, true, nil
	}
}

// Who a token signs in as: the user, else the application of a service principal or managed identity
func identity(value string) string {
	claims := credentials.Claims(value)
	return cmp.Or(credentials.Identity(value), credentials.Text(claims["appid"]), credentials.Text(claims["azp"]))
}

func (s *source) Credential(account credentials.Account) (credentials.Credential, bool) {
	if account.Spec() != azurefoundryconfig.Name {
		return credentials.Credential{}, false
	}
	if Mode(account) != azurefoundryconfig.EntraID {
		credential, ok := account.Lookup(providerconfig.APIKeyField, azurefoundryconfig.APIKeyEnv)
		credential.Auth = provider.AuthAPIKey
		return credential, ok
	}
	m := s.minter(account)
	ctx, cancel := context.WithTimeout(context.Background(), m.wait())
	defer cancel()
	t, err := m.get(ctx, false)
	if err != nil {
		slog.Warn("No Microsoft Entra ID token for Azure Foundry", "provider", account.Provider.ID, "account", account.Name, "error", err)
		return credentials.Credential{}, false
	}
	credential := credentials.Credential{APIKey: t.value, Auth: provider.AuthOAuth, Expires: time.Unix(t.expiresOn, 0), Identity: identity(t.value)}
	switch name := m.answered(); {
	case m.own:
		credential.Origin = credentials.Saved
	case environmental[name]:
		credential.Origin, credential.Source = credentials.Environment, name
	default:
		credential.Origin, credential.Source = credentials.External, name
	}
	return credential, true
}

// Whether the Entra token is near expiry or past its refresh_on; never while a token is being minted
func (s *source) Due(account credentials.Account) bool {
	if account.Spec() != azurefoundryconfig.Name || Mode(account) != azurefoundryconfig.EntraID {
		return false
	}
	m := s.minter(account)
	if m.failed != nil || !m.mu.TryLock() {
		return false
	}
	defer m.mu.Unlock()
	now := time.Now().Unix()
	return m.current.value != "" && (m.current.refreshOn != 0 && m.current.refreshOn <= now || m.current.expiresOn-now < refreshOffset)
}

// Mints a new Entra token now, the tokens every credential kept dropped, after Azure refused the last
func (s *source) Renew(ctx context.Context, account credentials.Account) (bool, error) {
	if account.Spec() != azurefoundryconfig.Name || Mode(account) != azurefoundryconfig.EntraID {
		return false, nil
	}
	_, err := s.minter(account).get(ctx, true)
	return true, err
}

// Why the account has no credential: the API key it lacks, or what the Entra token service answered
func (s *source) Why(account credentials.Account) (string, bool) {
	if account.Spec() != azurefoundryconfig.Name {
		return "", false
	}
	if Mode(account) != azurefoundryconfig.EntraID {
		return fmt.Sprintf("Azure Foundry needs an API key. Add one with `icebath auth add %s`, set %s in your keys file, or connect it in "+
			"/providers.\nTo sign in with Microsoft Entra ID instead, pick Microsoft Entra ID when you connect it.", account.Provider.ID,
			azurefoundryconfig.APIKeyEnv), true
	}
	m := s.minter(account)
	if m.failed != nil {
		return m.failed.Error(), true
	}
	if !m.mu.TryLock() {
		return "Microsoft Entra ID is still giving a token. Try again in a moment.", true
	}
	defer m.mu.Unlock()
	if m.err != nil {
		return m.err.Error() + "\n" + entraHint, true
	}
	return "", false
}

// Endpoint, scope and tenant of an Entra sign-in, and whether a token can be minted now, waiting at most ten seconds
func (s *source) Report(ctx context.Context, account credentials.Account) ([]credentials.Detail, bool) {
	if account.Spec() != azurefoundryconfig.Name || Mode(account) != azurefoundryconfig.EntraID {
		return nil, false
	}
	m := s.minter(account)
	e, _ := env(account)
	details := []credentials.Detail{{Name: "Sign-in", Value: "Microsoft Entra ID"},
		{Name: "Address", Value: cmp.Or(strings.TrimSpace(account.Provider.Settings.GetString(providerconfig.BaseURL)), "Not set")},
		{Name: "Scope", Value: m.scope}}
	if tenant := strings.TrimSpace(e.get("AZURE_TENANT_ID")); tenant != "" {
		details = append(details, credentials.Detail{Name: "Tenant", Value: tenant})
	}
	t, late, err := probe(ctx, m, probeWait)
	switch {
	case late:
		return append(details, credentials.Detail{Name: "State", Value: "⚠ Microsoft Entra ID gave no token within " + strs.Span(probeWait) + "."},
			credentials.Detail{Name: "What to do", Value: probeHint}), true
	case err != nil:
		return append(details, credentials.Detail{Name: "State", Value: "⚠ " + err.Error()}), true
	}
	via := "the usual Azure sign-ins"
	if found := sources(e); len(found) > 0 {
		via = strings.Join(found, ", ")
	}
	if m.own {
		via = "the account's service principal"
	}
	details = append(details, credentials.Detail{Name: "State", Value: "✓ Gives a token through " + via},
		credentials.Detail{Name: "Signed in by", Value: SignedInBy(m.answered())},
		credentials.Detail{Name: "Expires", Value: time.Unix(t.expiresOn, 0).Local().Format("2006-01-02 15:04")})
	if who := identity(t.value); who != "" {
		details = append(details, credentials.Detail{Name: "Signed in as", Value: who})
	}
	return details, true
}
