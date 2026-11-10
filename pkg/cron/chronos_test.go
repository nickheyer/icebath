package cron

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pyjwt"
)

type fakeChronosClient struct {
	mu         sync.Mutex
	provisions []map[string]string
	cancels    []string
	armed      []any
	provision  func(jobID string) error
}

func (f *fakeChronosClient) Provision(jobID, fireAt, callback, dedup string) (any, error) {
	if f.provision != nil {
		if err := f.provision(jobID); err != nil {
			return nil, err
		}
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.provisions = append(f.provisions, map[string]string{"job_id": jobID, "fire_at": fireAt, "agent_callback_url": callback, "dedup_key": dedup})
	return map[string]any{"schedule_id": "sched-" + jobID}, nil
}

func (f *fakeChronosClient) Cancel(jobID string) (any, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.cancels = append(f.cancels, jobID)
	return map[string]any{}, nil
}

func (f *fakeChronosClient) ListArmed() ([]any, error) { return append([]any(nil), f.armed...), nil }

func (f *fakeChronosClient) provisionedIDs() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	var ids []string
	for _, p := range f.provisions {
		ids = append(ids, p["job_id"])
	}
	return ids
}

func patchChronos[T any](t *testing.T, target *T, v T) {
	t.Helper()
	old := *target
	*target = v
	t.Cleanup(func() { *target = old })
}

func chronosFixture(t *testing.T) (*ChronosCronScheduler, *fakeChronosClient, *Store, *bytes.Buffer) {
	t.Helper()
	s, buf := newTestStore(t)
	prov := newChronosProvider(s)
	fake := &fakeChronosClient{}
	prov.client = fake
	patchChronos(t, &chronosCfg, func(_ *Store, keys ...string) any {
		if keys[len(keys)-1] == "callback_url" {
			return "https://agent.example/"
		}
		return "https://portal.test"
	})
	return prov, fake, s, buf
}

func chronosJobs(t *testing.T, jobs ...*Job) {
	t.Helper()
	patchChronos(t, &chronosLoadJobs, func(*Store) ([]*Job, error) { return jobs, nil })
	patchChronos(t, &chronosGetJob, func(_ *Store, id string) (*Job, error) {
		for _, j := range jobs {
			if j.ID == id {
				return j, nil
			}
		}
		return nil, nil
	})
}

func scheduledJob(id, fireAt string) *Job {
	return &Job{ID: id, Enabled: true, NextRunAt: strPtr(fireAt), State: "scheduled"}
}

func TestChronosIsAvailableFalseWithoutConfig(t *testing.T) {
	s, _ := newTestStore(t)
	patchChronos(t, &chronosCfg, func(*Store, ...string) any { return "" })
	if newChronosProvider(s).IsAvailable() {
		t.Fatal("available without config")
	}
	patchChronos(t, &chronosCfg, func(*Store, ...string) any { return "https://x" })
	patchChronos(t, &chronosAuthState, func(*Store) (map[string]any, error) { return map[string]any{"access_token": "tok"}, nil })
	if !newChronosProvider(s).IsAvailable() {
		t.Fatal("configured provider with a token unavailable")
	}
}

func TestChronosArmOneShotSendsProvision(t *testing.T) {
	prov, fake, _, _ := chronosFixture(t)
	if err := prov.armOneShot(&Job{ID: "j1", NextRunAt: strPtr("2026-06-18T12:00:00+00:00")}); err != nil {
		t.Fatal(err)
	}
	want := map[string]string{"job_id": "j1", "fire_at": "2026-06-18T12:00:00+00:00", "dedup_key": "j1:2026-06-18T12:00:00+00:00", "agent_callback_url": "https://agent.example/"}
	if len(fake.provisions) != 1 || len(fake.provisions[0]) != 4 {
		t.Fatalf("provisions %v", fake.provisions)
	}
	for k, v := range want {
		if fake.provisions[0][k] != v {
			t.Fatalf("%s = %q", k, fake.provisions[0][k])
		}
	}
}

func TestChronosRegisterJobArmsOnlyTheCreatedJobAndPropagatesFailure(t *testing.T) {
	prov, fake, _, _ := chronosFixture(t)
	if err := prov.RegisterJob(scheduledJob("created", "2026-06-18T12:00:00+00:00")); err != nil {
		t.Fatal(err)
	}
	if ids := fake.provisionedIDs(); len(ids) != 1 || ids[0] != "created" {
		t.Fatalf("provisioned %v", ids)
	}
	fake.provision = func(string) error { return errors.New("provision rejected") }
	if err := prov.RegisterJob(scheduledJob("created", "2026-06-18T12:00:00+00:00")); err == nil || err.Error() != "provision rejected" {
		t.Fatalf("register error %v", err)
	}
}

func TestChronosIdentityRejectionHandsFiresToTheBuiltinTicker(t *testing.T) {
	prov, fake, s, logs := chronosFixture(t)
	var calls []string
	status := 403
	fake.provision = func(id string) error {
		calls = append(calls, id)
		return &NasCronClientError{Msg: "POST /api/agent-cron/provision returned 403: invalid_client", Status: &status, ErrorCode: "invalid_client"}
	}
	chronosJobs(t, scheduledJob("a", "2026-06-18T12:00:00+00:00"), scheduledJob("b", "2026-06-18T12:05:00+00:00"))
	started := make(chan StartOptions, 1)
	patchChronos(t, &chronosFallbackStart, func(_ context.Context, _ <-chan struct{}, o StartOptions) { started <- o })
	interval := 7.0
	prov.Start(withCronStore(context.Background(), s), make(chan struct{}), StartOptions{Adapters: map[string]int{"x": 1}, Interval: &interval})
	select {
	case o := <-started:
		if o.Interval == nil || *o.Interval != 7 || o.Adapters == nil {
			t.Fatalf("ticker kwargs %+v", o)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("the built-in ticker must take over this process's fires")
	}
	if len(calls) != 1 || calls[0] != "a" {
		t.Fatalf("after the first rejection NAS is left alone: %v", calls)
	}
	if err := prov.RegisterJob(&Job{ID: "c", NextRunAt: strPtr("2026-06-18T12:10:00+00:00")}); err != nil {
		t.Fatal(err)
	}
	_ = prov.OnJobsChanged()
	if len(calls) != 1 {
		t.Fatalf("calls %v", calls)
	}
	if n := strings.Count(logs.String(), "re-login"); n != 1 || !strings.Contains(logs.String(), "built-in cron ticker") {
		t.Errorf("identity warnings %d in %s", n, logs.String())
	}
}

func TestChronosTransientProvisionFailureDoesNotDegrade(t *testing.T) {
	prov, fake, _, _ := chronosFixture(t)
	var attempts []string
	status := 502
	fake.provision = func(id string) error {
		attempts = append(attempts, id)
		return &NasCronClientError{Msg: "POST /api/agent-cron/provision returned 502: upstream", Status: &status}
	}
	chronosJobs(t, scheduledJob("a", "2026-06-18T12:00:00+00:00"))
	_ = prov.Reconcile()
	_ = prov.Reconcile()
	if len(attempts) != 2 || prov.identityRejected {
		t.Fatalf("attempts %v rejected %v", attempts, prov.identityRejected)
	}
}

func TestChronosReconcileArmsAllEnabledAndCancelsOrphans(t *testing.T) {
	prov, fake, _, _ := chronosFixture(t)
	paused := scheduledJob("p", "2026-06-18T12:00:00+00:00")
	paused.State = "paused"
	chronosJobs(t, scheduledJob("a", "2026-06-18T12:00:00+00:00"), scheduledJob("b", "2026-06-18T12:05:00+00:00"), paused)
	if err := prov.Reconcile(); err != nil {
		t.Fatal(err)
	}
	if ids := fake.provisionedIDs(); len(ids) != 2 || ids[0] != "a" || ids[1] != "b" || len(fake.cancels) != 0 {
		t.Fatalf("provisioned %v cancels %v", ids, fake.cancels)
	}
	other, fake2, _, _ := chronosFixture(t)
	fake2.armed = []any{map[string]any{"job_id": "a", "fire_at": "2026-06-18T12:00:00+00:00"}, map[string]any{"job_id": "gone"}, map[string]any{"fire_at": "x"}}
	if err := other.Reconcile(); err != nil {
		t.Fatal(err)
	}
	if ids := fake2.provisionedIDs(); len(ids) != 1 || ids[0] != "b" || len(fake2.cancels) != 1 || fake2.cancels[0] != "gone" {
		t.Fatalf("observed reconcile provisioned %v cancels %v", ids, fake2.cancels)
	}
}

func chronosFireCtx(t *testing.T, s *Store) context.Context {
	t.Helper()
	return withCronStore(context.Background(), s)
}

func TestChronosFireDueRearmsNextOneshot(t *testing.T) {
	prov, fake, s, _ := chronosFixture(t)
	patchChronos(t, &baseClaimFire, func(_ context.Context, _ CronScheduler, id string, _, _ bool) (*Job, error) {
		return &Job{ID: id, ExecutionID: strPtr("exec-1")}, nil
	})
	patchChronos(t, &baseFireClaimed, func(context.Context, CronScheduler, *Job, FireOptions) (bool, error) { return true, nil })
	chronosJobs(t, scheduledJob("j1", "2026-06-18T12:05:00+00:00"))
	ran, err := schedulerFireDue(chronosFireCtx(t, s), prov, "j1", FireOptions{})
	if err != nil || !ran {
		t.Fatalf("fire_due %v %v", ran, err)
	}
	if len(fake.provisions) != 1 || fake.provisions[0]["job_id"] != "j1" || fake.provisions[0]["fire_at"] != "2026-06-18T12:05:00+00:00" {
		t.Fatalf("provisions %v", fake.provisions)
	}
}

func TestChronosFireDueRearmsAfterClaimedJobFailure(t *testing.T) {
	prov, fake, s, _ := chronosFixture(t)
	patchChronos(t, &baseClaimFire, func(_ context.Context, _ CronScheduler, id string, _, _ bool) (*Job, error) { return &Job{ID: id}, nil })
	patchChronos(t, &runOneJobSeam, func(context.Context, *Store, *Job, runOneJobOptions) (bool, error) { return false, nil })
	chronosJobs(t, scheduledJob("j1", "2026-06-18T12:05:00+00:00"))
	if ran, err := schedulerFireDue(chronosFireCtx(t, s), prov, "j1", FireOptions{}); err != nil || !ran {
		t.Fatalf("fire_due %v %v", ran, err)
	}
	if ids := fake.provisionedIDs(); len(ids) != 1 || ids[0] != "j1" {
		t.Fatalf("provisioned %v", ids)
	}
}

func TestChronosFireDueForwardsManualForceToClaim(t *testing.T) {
	prov, _, s, _ := chronosFixture(t)
	var seen []bool
	patchChronos(t, &baseClaimFire, func(_ context.Context, _ CronScheduler, _ string, force, manual bool) (*Job, error) {
		seen = append(seen, force, manual)
		return nil, nil
	})
	if ran, err := schedulerFireDue(chronosFireCtx(t, s), prov, "j1", FireOptions{Force: true}); err != nil || ran {
		t.Fatalf("fire_due %v %v", ran, err)
	}
	if len(seen) != 2 || !seen[0] || seen[1] {
		t.Fatalf("claim keywords %v", seen)
	}
}

func TestChronosNoRearmWhenJobGoneClaimLostOrRunFailed(t *testing.T) {
	prov, fake, s, _ := chronosFixture(t)
	chronosJobs(t)
	patchChronos(t, &baseClaimFire, func(_ context.Context, _ CronScheduler, id string, _, _ bool) (*Job, error) { return &Job{ID: id}, nil })
	patchChronos(t, &baseFireClaimed, func(context.Context, CronScheduler, *Job, FireOptions) (bool, error) { return true, nil })
	if ran, _ := schedulerFireDue(chronosFireCtx(t, s), prov, "j1", FireOptions{}); !ran || len(fake.provisions) != 0 {
		t.Fatalf("job gone: ran %v provisions %v", ran, fake.provisions)
	}
	patchChronos(t, &baseClaimFire, func(context.Context, CronScheduler, string, bool, bool) (*Job, error) { return nil, nil })
	if ran, _ := schedulerFireDue(chronosFireCtx(t, s), prov, "j1", FireOptions{}); ran || len(fake.provisions) != 0 {
		t.Fatalf("claim lost: ran %v provisions %v", ran, fake.provisions)
	}
	chronosJobs(t, scheduledJob("j1", "2026-06-18T12:05:00+00:00"))
	patchChronos(t, &baseFireClaimed, func(context.Context, CronScheduler, *Job, FireOptions) (bool, error) {
		return false, errors.New("run failed")
	})
	if ran, err := prov.FireClaimed(chronosFireCtx(t, s), &Job{ID: "j1"}, FireOptions{}); ran || err == nil || len(fake.provisions) != 0 {
		t.Fatalf("run failed: ran %v err %v provisions %v", ran, err, fake.provisions)
	}
}

func TestChronosIsSplitFireCapable(t *testing.T) {
	prov, _, _, _ := chronosFixture(t)
	if !providerSupportsSplitFire(prov) || !providerSupportsForceFire(prov) {
		t.Fatal("chronos must be split fire and force fire capable")
	}
}

func TestExternalProviderStartRecoversInterruptedRecords(t *testing.T) {
	prov, _, s, _ := chronosFixture(t)
	ctx := context.Background()
	var events []string
	patchChronos(t, &recoverInterruptedExecutions, func(context.Context, *Store) (int, error) {
		events = append(events, "recover")
		return 0, nil
	})
	patchChronos(t, &chronosLoadJobs, func(*Store) ([]*Job, error) {
		events = append(events, "reconcile")
		return nil, nil
	})
	prov.Start(withCronStore(ctx, s), make(chan struct{}), StartOptions{})
	if !reflect.DeepEqual(events, []string{"recover", "reconcile"}) {
		t.Fatalf("recover must run before reconcile: %v", events)
	}
}

func TestNASCronClientRequestsAndErrors(t *testing.T) {
	s, _ := newTestStore(t)
	var seen []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		seen = append(seen, r.Method+" "+r.URL.String()+" "+r.Header.Get("Authorization")+" "+string(body))
		switch r.URL.Path {
		case nasListPath:
			_, _ = w.Write([]byte(`{"armed":[{"job_id":"a","fire_at":"t"}]}`))
		case nasCancelPath:
			w.WriteHeader(403)
			_, _ = w.Write([]byte(`{"error":"invalid_client"}`))
		default:
			_, _ = w.Write(nil)
		}
	}))
	defer srv.Close()
	c := newNASCronClient(s, srv.URL+"/")
	c.token = func() (string, error) { return "tok", nil }
	if out, err := c.Provision("j", "t", "cb", "j:t"); err != nil || out.(*pyjson.Object).Len() != 0 {
		t.Fatalf("provision %v %v", out, err)
	}
	if items, err := c.ListArmed(); err != nil || len(items) != 1 {
		t.Fatalf("list %v %v", items, err)
	}
	_, err := c.Cancel("j")
	var nasErr *NasCronClientError
	if !errors.As(err, &nasErr) || !nasErr.IdentityRejected() || err.Error() != `POST /api/agent-cron/cancel returned 403: {"error":"invalid_client"}` {
		t.Fatalf("cancel error %v", err)
	}
	if !strings.HasPrefix(seen[0], "POST /api/agent-cron/provision Bearer tok {") || seen[1] != "GET /api/agent-cron/list Bearer tok " {
		t.Fatalf("requests %q", seen)
	}
	c.token = func() (string, error) { return "", errors.New("not logged in") }
	if _, err := c.ListArmed(); err == nil || err.Error() != "GET /api/agent-cron/list failed: not logged in" {
		t.Fatalf("token failure %v", err)
	}
}

type chronosRSAKeys struct{ priv, pub string }

var chronosKeys = sync.OnceValue(func() chronosRSAKeys {
	key, _ := rsa.GenerateKey(rand.Reader, 2048)
	der, _ := x509.MarshalPKCS8PrivateKey(key)
	pubDER, _ := x509.MarshalPKIXPublicKey(&key.PublicKey)
	return chronosRSAKeys{string(pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der})), string(pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: pubDER}))}
})

const (
	chronosAud = "agent:inst-123"
	chronosIss = "https://portal.nousresearch.com"
)

func mintFireToken(t *testing.T, priv string, over map[string]any) string {
	t.Helper()
	now := time.Now().Unix()
	claims := pyjson.NewObject()
	base := [][2]any{{"aud", chronosAud}, {"iss", chronosIss}, {"purpose", "cron_fire"}, {"iat", now}, {"nbf", now - 5}, {"exp", now + 300}}
	for _, kv := range base {
		key := kv[0].(string)
		v, overridden := over[key]
		if !overridden {
			claims.Set(key, kv[1])
		} else if v != nil {
			claims.Set(key, v)
		}
	}
	token, err := pyjwt.EncodeRS256(claims, priv)
	if err != nil {
		t.Fatal(err)
	}
	return token
}

func TestVerifyNASFireToken(t *testing.T) {
	keys := chronosKeys()
	now := time.Now().Unix()
	verify := func(token, key, iss string) *pyjson.Object {
		return VerifyNASFireToken(NASFireTokenOptions{Token: token, ExpectedAudience: chronosAud, JWKSOrKey: key, Issuer: iss})
	}
	claims := verify(mintFireToken(t, keys.priv, nil), keys.pub, chronosIss)
	if claims == nil {
		t.Fatal("valid token refused")
	}
	if p, _ := claims.Get("purpose"); p != "cron_fire" {
		t.Fatalf("purpose %v", p)
	}
	if a, _ := claims.Get("aud"); a != chronosAud {
		t.Fatalf("aud %v", a)
	}
	attacker, _ := rsa.GenerateKey(rand.Reader, 2048)
	der, _ := x509.MarshalPKCS8PrivateKey(attacker)
	attackerPriv := string(pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der}))
	for name, token := range map[string]string{
		"wrong audience":     mintFireToken(t, keys.priv, map[string]any{"aud": "agent:someone-else"}),
		"missing purpose":    mintFireToken(t, keys.priv, map[string]any{"purpose": nil}),
		"expired":            mintFireToken(t, keys.priv, map[string]any{"iat": now - 1000, "nbf": now - 1000, "exp": now - 600}),
		"tampered signature": mintFireToken(t, attackerPriv, nil),
	} {
		if verify(token, keys.pub, chronosIss) != nil {
			t.Errorf("%s accepted", name)
		}
	}
	if verify(mintFireToken(t, keys.priv, nil), "", "") != nil {
		t.Error("no key configured accepted")
	}
	if verify("", keys.pub, "") != nil {
		t.Error("empty token accepted")
	}
}

type fakeJWKSource struct{ pub string }

func (f fakeJWKSource) SigningKeyFromJWT(string) (*pyjwt.JWK, error) {
	return &pyjwt.JWK{KeyID: "k", Key: f.pub}, nil
}

func TestVerifyNASFireTokenJWKSURLAndClientHeaders(t *testing.T) {
	keys := chronosKeys()
	const url = "https://portal.nousresearch.com/.well-known/jwks.json"
	var gotURL string
	var gotHeaders map[string]string
	patchChronos(t, &newChronosJWKClient, func(u string, headers map[string]string) chronosSigningKeySource {
		gotURL, gotHeaders = u, headers
		return fakeJWKSource{keys.pub}
	})
	patchChronos(t, &chronosJWKClients, map[string]chronosSigningKeySource{})
	claims := VerifyNASFireToken(NASFireTokenOptions{Token: mintFireToken(t, keys.priv, nil), ExpectedAudience: chronosAud, JWKSOrKey: url, Issuer: chronosIss})
	if claims == nil || gotURL != url {
		t.Fatalf("jwks path claims %v url %q", claims, gotURL)
	}
	if p, _ := claims.Get("purpose"); p != "cron_fire" {
		t.Fatalf("purpose %v", p)
	}
	if gotHeaders["Accept"] == "" || gotHeaders["User-Agent"] == "" {
		t.Fatalf("headers %v", gotHeaders)
	}
	patchChronos(t, &chronosJWKClients, map[string]chronosSigningKeySource{})
	gotURL = ""
	chronosJWKClient(url)
	if gotURL != url {
		t.Fatalf("client url %q", gotURL)
	}
}
