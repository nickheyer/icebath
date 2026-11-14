package gateway

import (
	"context"
	"fmt"
	"os"
	"reflect"
	"strings"
	"testing"

	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/provider"
	"github.com/nickheyer/icebath/pkg/state"
)

const aliasRetryConfig = "platform_toolsets: {cli: []}\n" +
	"model: {provider: custom, default: m, base_url: http://default.invalid/v1}\n" +
	"model_aliases:\n" +
	"  alias-key: {model: model-alias, provider: custom, base_url: \"${P5_ENDPOINT}\", api_key: \"${P5_ALIAS_KEY}\"}\n" +
	"  twin-key: {model: model-alias, provider: custom, base_url: \"${P5_ENDPOINT}\", api_key: \"${P5_ALIAS_KEY}\"}\n" +
	"  same-name: {model: same-name, provider: custom, base_url: \"${P5_ENDPOINT}\"}\n"

const aliasRetryPrompt = "ICEBATH_EPHEMERAL_SYSTEM_PROMPT"

type aliasRetryOwner struct {
	ctx  context.Context
	a    *SessionAuthority
	home string
}

// Profile env file whose alias endpoint and key are generation n
func aliasRetryEnv(t *testing.T, home string, n int) {
	t.Helper()
	envRefsWrite(t, home, ".env", fmt.Sprintf("P5_ENDPOINT=http://endpoint-%d.invalid/v1\nP5_ALIAS_KEY=alias-key-%d\n", n, n))
}

// Owner over a profile with the alias config, its env at generation one and the alias cache empty
func newAliasRetryOwner(t *testing.T) aliasRetryOwner {
	t.Helper()
	provider.DirectAliases.Clear()
	t.Cleanup(provider.DirectAliases.Clear)
	for _, name := range []string{"P5_ENDPOINT", "P5_ALIAS_KEY"} {
		t.Setenv(name, "")
		os.Unsetenv(name)
	}
	ctx, a, home := envRefsOwner(t, aliasRetryConfig, "")
	aliasRetryEnv(t, home, 1)
	return aliasRetryOwner{ctx, a, home}
}

func (o aliasRetryOwner) params(fields map[string]any) map[string]any {
	out := map[string]any{"request_id": "retry", "source": "cli", "cwd": o.home, "toolsets": []any{}}
	for k, v := range fields {
		out[k] = v
	}
	return out
}

func (o aliasRetryOwner) create(t *testing.T, params map[string]any) SessionRef {
	t.Helper()
	ref, err := createLocalSession(o.ctx, o.a, envRefsActor(o.a), params, localCreateOptions{})
	if err != nil {
		t.Fatal(err)
	}
	return ref
}

func (o aliasRetryOwner) stage(t *testing.T, params map[string]any) *state.LocalCreateReceipt {
	t.Helper()
	ref, staged, err := stageLocalSession(o.ctx, o.a, envRefsActor(o.a), params, localCreateOptions{})
	if err != nil || staged == nil {
		t.Fatalf("staging answered %v %v %v", ref, staged, err)
	}
	return staged
}

// Off loop commit and publication every async create runs after staging
func (o aliasRetryOwner) commit(t *testing.T, staged *state.LocalCreateReceipt) SessionRef {
	t.Helper()
	if _, err := o.a.DB.CommitLocalSession(o.ctx, o.a.Epoch, *staged); err != nil {
		t.Fatal(err)
	}
	sid, err := receiptField(staged.Receipt, "session_id")
	if err != nil {
		t.Fatal(err)
	}
	ref, err := restoreLocalSession(o.ctx, o.a, pyStrOf(sid))
	if err != nil {
		t.Fatal(err)
	}
	return ref
}

func (o aliasRetryOwner) storedPolicy(t *testing.T, ref SessionRef) any {
	t.Helper()
	raw, _ := recoverReceipt(t, o.ctx, o.a.DB, ref.SessionID).Get("policy")
	return recoverPlain(t, raw)
}

func (o aliasRetryOwner) launchKey(t *testing.T, saved any) string {
	t.Helper()
	policy, err := restorePolicy(saved)
	if err != nil {
		t.Fatal(err)
	}
	key, err := launchKey(o.a, policy)
	if err != nil || key == nil {
		t.Fatalf("launch key %v %v", key, err)
	}
	return *key
}

// Base url of the model block a stored policy runs with
func restorePolicyURL(t *testing.T, ctx context.Context, saved any) any {
	t.Helper()
	policy, err := restorePolicy(saved)
	if err != nil {
		t.Fatal(err)
	}
	return envRefsModel(t, ctx, policy)["base_url"]
}

func TestExactAliasRetryAfterAnOwnerKeyRotationReturnsTheOriginal(t *testing.T) {
	o := newAliasRetryOwner(t)
	ref := o.create(t, o.params(map[string]any{"model": "alias-key"}))
	saved := o.storedPolicy(t, ref)
	if key := o.launchKey(t, saved); key != "alias-key-1" {
		t.Fatalf("launch key %q", key)
	}
	aliasRetryEnv(t, o.home, 2)
	if again := o.create(t, o.params(map[string]any{"model": "alias-key"})); again != ref {
		t.Fatalf("retry answered %v, want %v", again, ref)
	}
	if after := o.storedPolicy(t, ref); !reflect.DeepEqual(after, saved) {
		t.Fatalf("stored policy changed to %v", after)
	}
	if key := o.launchKey(t, saved); key != "alias-key-1" {
		t.Fatalf("launch key after retry %q", key)
	}
}

func TestExactAliasRetryAfterAnEndpointRotationReturnsTheOriginal(t *testing.T) {
	for _, alias := range []string{"same-name", "alias-key"} {
		for _, order := range []string{"sequential", "overlap-first-wins", "overlap-second-wins"} {
			t.Run(alias+"/"+order, func(t *testing.T) {
				o := newAliasRetryOwner(t)
				params := func() map[string]any { return o.params(map[string]any{"model": alias}) }
				var ref SessionRef
				var saved any
				if order == "sequential" {
					ref = o.create(t, params())
					saved = o.storedPolicy(t, ref)
					aliasRetryEnv(t, o.home, 2)
					if again := o.create(t, params()); again != ref {
						t.Fatalf("retry answered %v, want %v", again, ref)
					}
				} else {
					staged := []*state.LocalCreateReceipt{o.stage(t, params())}
					aliasRetryEnv(t, o.home, 2)
					staged = append(staged, o.stage(t, params()))
					configs := make([]any, 2)
					for i, s := range staged {
						text, err := pyItem(s.Receipt, "policy", "config_json")
						if err != nil {
							t.Fatal(err)
						}
						decoded, err := pyjson.Loads(text.(string))
						if err != nil {
							t.Fatal(err)
						}
						configs[i] = pyjson.Plain(decoded)
					}
					if reflect.DeepEqual(configs[0], configs[1]) {
						t.Fatalf("both attempts froze %v", configs[0])
					}
					if order == "overlap-second-wins" {
						staged[0], staged[1] = staged[1], staged[0]
					}
					ref = o.commit(t, staged[0])
					saved = o.storedPolicy(t, ref)
					if again := o.commit(t, staged[1]); again != ref {
						t.Fatalf("losing commit answered %v, want %v", again, ref)
					}
				}
				if after := o.storedPolicy(t, ref); !reflect.DeepEqual(after, saved) {
					t.Fatalf("stored policy changed to %v", after)
				}
				generation := 1
				if order == "overlap-second-wins" {
					generation = 2
				}
				if url := restorePolicyURL(t, o.ctx, saved); url != fmt.Sprintf("http://endpoint-%d.invalid/v1", generation) {
					t.Fatalf("base url %v", url)
				}
			})
		}
	}
}

// Env file rotating only the alias key
func aliasRetryRotateKey(t *testing.T, home string) {
	t.Helper()
	envRefsWrite(t, home, ".env", "P5_ENDPOINT=http://endpoint-1.invalid/v1\nP5_ALIAS_KEY=alias-key-2\n")
}

func TestAnOverlappingExactAliasRetryAfterAKeyRotationReusesTheHeldKey(t *testing.T) {
	for _, order := range []string{"first-wins", "second-wins"} {
		t.Run(order, func(t *testing.T) {
			o := newAliasRetryOwner(t)
			params := func() map[string]any { return o.params(map[string]any{"model": "alias-key"}) }
			staged := []*state.LocalCreateReceipt{o.stage(t, params())}
			aliasRetryRotateKey(t, o.home)
			staged = append(staged, o.stage(t, params()))
			if order == "second-wins" {
				staged[0], staged[1] = staged[1], staged[0]
			}
			ref := o.commit(t, staged[0])
			if again := o.commit(t, staged[1]); again != ref {
				t.Fatalf("losing commit answered %v, want %v", again, ref)
			}
			var count int
			if err := o.a.DB.ReadCtx(o.ctx, func(conn state.SQLiteConn) error {
				return conn.QueryRowContext(o.ctx, "SELECT COUNT(*) FROM sessions").Scan(&count)
			}); err != nil || count != 1 {
				t.Fatalf("sessions %d %v", count, err)
			}
			saved := o.storedPolicy(t, ref)
			if key := o.launchKey(t, saved); key != "alias-key-1" {
				t.Fatalf("launch key %q", key)
			}
			policy, err := restorePolicy(saved)
			if err != nil || policy.CredentialRef == nil {
				t.Fatalf("policy %v %v", policy, err)
			}
			_, fingerprint, _ := strings.Cut(*policy.CredentialRef, "#")
			if fingerprint != provider.FingerprintSecretValue("alias-key-1") {
				t.Fatalf("credential ref %q", *policy.CredentialRef)
			}
		})
	}
}

func TestAnOverlappingAliasRetryDifferingOnlyInADroppedNameReusesTheHeldKey(t *testing.T) {
	installClientEnvSecretPolicy(t)
	o := newAliasRetryOwner(t)
	staged := []*state.LocalCreateReceipt{o.stage(t, o.params(map[string]any{"model": "alias-key",
		"client_env": map[string]any{aliasRetryPrompt: "P"}}))}
	aliasRetryRotateKey(t, o.home)
	staged = append(staged, o.stage(t, o.params(map[string]any{"model": "alias-key",
		"client_env": map[string]any{aliasRetryPrompt: "P", "PATH": "/client/bin"}})))
	ref := o.commit(t, staged[1])
	if again := o.commit(t, staged[0]); again != ref {
		t.Fatalf("losing commit answered %v, want %v", again, ref)
	}
	if key := o.launchKey(t, o.storedPolicy(t, ref)); key != "alias-key-1" {
		t.Fatalf("launch key %q", key)
	}
}
