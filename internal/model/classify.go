package model

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/nickheyer/icebath/internal/httpclient"
	"github.com/nickheyer/icebath/internal/model/auth"
	"github.com/nickheyer/icebath/internal/model/portal"
	"github.com/nickheyer/icebath/internal/model/providers"
	"github.com/nickheyer/icebath/internal/model/wire"
)

type ErrorHook interface {
	ClassifyError(ctx context.Context, in HookInput) (HookVerdict, bool, error)
}

type HookInput struct {
	Provider      string
	Model         string
	Status        int
	Code          string
	Message       string
	Body          json.RawMessage
	Err           error
	ApproxTokens  int
	ContextLength int
	Messages      int
}

type HookVerdict struct {
	Reason    string
	Retryable *bool
	Compress  *bool
	Rotate    *bool
	Fallback  *bool
	Message   string
	Detail    *Detail
}

type Hooks struct {
	Errors     []ErrorHook
	Middleware []RequestMiddleware
	Execution  []ExecutionMiddleware
	Observers  []CallObserver
}

type Facts struct {
	Provider      string
	Model         string
	BaseURL       string
	APIKey        auth.Secret
	ApproxTokens  int
	ContextLength int
	Messages      int
}

type ClassifierOptions struct {
	Providers     *providers.Registry
	Hooks         Hooks
	OwnsRoute     func(ctx context.Context, provider, baseURL string) (owns, known bool)
	PrefixedModel func(ctx context.Context, provider, model string) string
	Label         func(ctx context.Context, provider string) string
	Descriptor    func(ctx context.Context, provider string) (providers.Descriptor, bool)
	DiskFull      func(err error) bool
}

type Classifier struct {
	o ClassifierOptions
}

func NewClassifier(o ClassifierOptions) (*Classifier, error) {
	switch {
	case o.Providers == nil:
		return nil, errors.New("model: classifier needs the provider registry")
	case o.OwnsRoute == nil:
		return nil, errors.New("model: classifier needs the route ownership lookup")
	case o.PrefixedModel == nil:
		return nil, errors.New("model: classifier needs the prefixed model id lookup")
	case o.Label == nil:
		return nil, errors.New("model: classifier needs the provider label lookup")
	case o.Descriptor == nil:
		return nil, errors.New("model: classifier needs the provider descriptor lookup")
	case o.DiskFull == nil:
		return nil, errors.New("model: classifier needs the disk full predicate")
	}
	return &Classifier{o: o}, nil
}

type verdict struct {
	class    Class
	hints    Hints
	detail   *Detail
	message  string
	override bool
}

func vd(c Class) verdict { return verdict{class: c, hints: Hints{Retryable: true}} }

func abortFallback(c Class) verdict {
	return verdict{class: c, hints: Hints{Fallback: true}}
}

var (
	vBilling          = verdict{class: ClassBilling, hints: Hints{RotateCredential: true, Fallback: true}}
	vRateLimit        = verdict{class: ClassRateLimit, hints: Hints{Retryable: true, RotateCredential: true, Fallback: true}}
	vAuthRotate       = verdict{class: ClassAuth, hints: Hints{RotateCredential: true, Fallback: true}}
	vAuthFallback     = abortFallback(ClassAuth)
	vModelNotFound    = abortFallback(ClassModelNotFound)
	vUpstreamBlocked  = abortFallback(ClassUpstreamBlocked)
	vContentBlocked   = abortFallback(ClassContentPolicyBlocked)
	vModelEntitlement = verdict{class: ClassModelEntitlement, hints: Hints{RotateCredential: true, Fallback: true}}
	vFormatError      = abortFallback(ClassFormatError)
	vPolicyBlocked    = abortFallback(ClassProviderPolicyBlocked)
	vSSLCert          = abortFallback(ClassSSLCertVerification)
	vContextOverflow  = verdict{class: ClassContextOverflow, hints: Hints{Retryable: true, Compress: true}}
	vPayloadTooLarge  = verdict{class: ClassPayloadTooLarge, hints: Hints{Retryable: true, Compress: true}}
	vOverloaded       = vd(ClassOverloaded)
	vServerError      = vd(ClassServerError)
	vTimeout          = vd(ClassTimeout)
	vUnknown          = vd(ClassUnknown)
	vImageTooLarge    = vd(ClassImageTooLarge)
	vImageCorrupt     = vd(ClassImageCorrupt)
	vMultimodal       = vd(ClassMultimodalToolContent)
	vInvalidEncrypted = vd(ClassInvalidEncryptedContent)
	vReasoningMand    = vd(ClassReasoningMandatory)
	vRoleAlternation  = abortFallback(ClassRoleAlternation)
	vMalformedArgs    = verdict{class: ClassFormatError}
	vCodeRateLimit    = verdict{class: ClassRateLimit, hints: Hints{Retryable: true, RotateCredential: true}}
)

type rule struct {
	patterns []string
	v        verdict
	billing  bool
}

func billingVerdict(msg string) verdict {
	v := vBilling
	if containsAny(msg, unverifiedBillingPatterns) {
		v.detail = &Detail{BillingUnverified: true, PossibleContentFilter: true}
	}
	return v
}

func firstMatch(msg string, rules ...[]rule) (verdict, bool) {
	for _, list := range rules {
		for _, r := range list {
			if !containsAny(msg, r.patterns) {
				continue
			}
			if r.billing {
				return billingVerdict(msg), true
			}
			return r.v, true
		}
	}
	return verdict{}, false
}

var imageToolRules = []rule{
	{patterns: multimodalToolContentPatterns, v: vMultimodal},
	{patterns: imageCorruptPatterns, v: vImageCorrupt},
	{patterns: imageTooLargePatterns, v: vImageTooLarge},
}

var overflowAs5xxRules = []rule{
	{patterns: emptyProviderResponsePatterns, v: vServerError},
	{patterns: memoryCeilingPatterns, v: vOverloaded},
	{patterns: contextOverflowPatterns, v: vContextOverflow},
}

var rules404 = []rule{
	{patterns: billingPatterns, v: vBilling},
	{patterns: providerPolicyBlockedPatterns, v: vPolicyBlocked},
	{patterns: modelNotFoundPatterns, v: vModelNotFound},
}

var tail400Rules = []rule{
	{patterns: providerPolicyBlockedPatterns, v: vPolicyBlocked},
	{patterns: modelNotFoundPatterns, v: vModelNotFound},
	{patterns: rateLimitPatterns, v: vRateLimit},
	{patterns: billingPatterns, billing: true},
}

var messageHeadRules = []rule{
	{patterns: memoryCeilingPatterns, v: vOverloaded},
	{patterns: payloadTooLargePatterns, v: vPayloadTooLarge},
	{patterns: roleAlternationPatterns, v: vRoleAlternation},
	{patterns: streamRenderErrorPatterns, v: vFormatError},
}

var messageTailRules = []rule{
	{patterns: overloadedPatterns, v: vOverloaded},
	{patterns: billingPatterns, billing: true},
	{patterns: rateLimitPatterns, v: vRateLimit},
	{patterns: emptyProviderResponsePatterns, v: vServerError},
	{patterns: contextOverflowPatterns, v: vContextOverflow},
	{patterns: authPatterns, v: vAuthRotate},
	{patterns: providerPolicyBlockedPatterns, v: vPolicyBlocked},
	{patterns: modelNotFoundPatterns, v: vModelNotFound},
	{patterns: timeoutMessagePatterns, v: vTimeout},
	{patterns: connectionMessagePatterns, v: vTimeout},
}

func codeVerdict(code string) (verdict, bool) {
	switch {
	case rateLimitCodes[code]:
		return vCodeRateLimit, true
	case billingErrorCodes[code]:
		return vBilling, true
	case modelNotFoundCodes[code]:
		return vModelNotFound, true
	case contextOverflowCodes[code]:
		return vContextOverflow, true
	case memoryCeilingCodes[code]:
		return vOverloaded, true
	case code == "invalid_encrypted_content":
		return vInvalidEncrypted, true
	}
	return verdict{}, false
}

func familyCodeVerdict(family, code string) (verdict, bool) {
	switch family + "/" + code {
	case "openai/server_error", "gemini/internal", "anthropic/api_error":
		return vServerError, true
	case "gemini/unavailable":
		return vOverloaded, true
	case "gemini/deadline_exceeded":
		return vTimeout, true
	case "anthropic/rate_limit_error":
		return vRateLimit, true
	}
	return verdict{}, false
}

type failure struct {
	err       error
	status    int
	body      wire.Value
	header    http.Header
	text      string
	msg       string
	code      string
	lcode     string
	provider  string
	slug      string
	model     string
	modelSlug string
	baseURL   string
	approx    int
	window    int
	count     int
	anonymous bool
	now       time.Time
}

func newFailure(err error, f Facts, now time.Time) *failure {
	x := &failure{
		err: err, provider: f.Provider, model: f.Model, baseURL: f.BaseURL,
		approx: f.ApproxTokens, window: f.ContextLength, count: f.Messages, now: now,
		slug:      strings.ToLower(strings.TrimSpace(f.Provider)),
		modelSlug: strings.ToLower(strings.TrimSpace(f.Model)),
		anonymous: portal.IsAnonymousRequest(f.Provider, f.APIKey),
	}
	if x.window == 0 {
		x.window = 200000
	}
	x.text = err.Error()
	var ae *wire.APIError
	var se *wire.StreamError
	var status *httpclient.StatusError
	switch {
	case errors.As(err, &ae):
		x.status, x.header = ae.Status, ae.Header
		if v, perr := wire.ParseValue(ae.Body()); perr == nil && (v.IsObject() || v.IsArray()) {
			if v.IsObject() {
				x.body = v
			}
			if !ae.InStream {
				x.text += " " + string(ae.Body())
			}
		}
	case errors.As(err, &se):
		x.body = streamErrorBody(se)
	case errors.As(err, &status):
		x.status = status.Code
	}
	if x.status == 0 {
		x.status = statusFromBody(x.body)
	}
	if x.body.IsObject() {
		x.code = codeFromPayload(x.body, []string{"code", "error_code", "errorCode"}, true)
	}
	x.lcode = strings.ToLower(x.code)
	x.msg = buildErrorText(x.text, x.body)
	return x
}

func streamErrorBody(se *wire.StreamError) wire.Value {
	message := se.Data()
	switch {
	case se.Code == wire.StreamEmptyFrame:
		message = "Provider stream returned an empty SSE data frame (keepalive with no payload)."
	case message == "":
		message = "Provider stream returned non-JSON SSE data."
	}
	inner := (&wire.Dict{}).SetString("code", se.Code).SetString("message", message)
	return (&wire.Dict{}).Set("error", inner.JSON()).Value()
}

func (x *failure) largeSession(frac float64, tokens, messages int) bool {
	return float64(x.approx) > float64(x.window)*frac ||
		(x.window <= 256000 && (x.approx > tokens || x.count > messages))
}

func (x *failure) errObj() wire.Value {
	if obj := x.body.Get("error"); obj.IsObject() {
		return obj
	}
	return wire.Value{}
}

func (x *failure) bodyRaw() json.RawMessage {
	if x.body.IsObject() {
		return x.body.Raw()
	}
	return json.RawMessage("{}")
}

func (c *Classifier) Classify(ctx context.Context, err error, f Facts) *Error {
	x := newFailure(err, f, time.Now())
	stages := []func() (verdict, bool){
		func() (verdict, bool) { return c.pluginVerdict(ctx, x) },
		func() (verdict, bool) { return c.profileVerdict(ctx, x) },
		x.specialCases,
		func() (verdict, bool) { return c.byStatus(ctx, x) },
		x.presetCase,
		x.byCode,
		x.byMessage,
		x.byTransport,
	}
	v := vUnknown
	for _, stage := range stages {
		if got, ok := stage(); ok {
			v = got
			break
		}
	}
	message := extractMessage(x)
	if v.class == ClassAuth || v.class == ClassAuthPermanent {
		if host := c.offRouteHost(ctx, x); host != "" {
			message += " (endpoint: " + host + ")"
		}
	}
	if v.override {
		message = v.message
	}
	e := &Error{Class: v.class, Hints: v.hints, Status: x.status, Provider: f.Provider, Model: f.Model, Message: message, Err: err}
	if v.detail != nil {
		e.Detail = *v.detail
	}
	return e
}

func (c *Classifier) offRouteHost(ctx context.Context, x *failure) string {
	host := providers.URLHostname(x.baseURL)
	if host == "" {
		return ""
	}
	if owns, known := c.o.OwnsRoute(ctx, x.slug, x.baseURL); known && owns {
		return ""
	}
	return host
}

func applyHints(h *Hints, retryable, compress, rotate, fallback *bool) {
	if retryable != nil {
		h.Retryable = *retryable
	}
	if compress != nil {
		h.Compress = *compress
	}
	if rotate != nil {
		h.RotateCredential = *rotate
	}
	if fallback != nil {
		h.Fallback = *fallback
	}
}

func (c *Classifier) pluginVerdict(ctx context.Context, x *failure) (verdict, bool) {
	if len(c.o.Hooks.Errors) == 0 {
		return verdict{}, false
	}
	in := HookInput{
		Provider: x.provider, Model: x.model, Status: x.status, Code: x.code, Message: x.msg,
		Body: x.bodyRaw(), Err: x.err, ApproxTokens: x.approx, ContextLength: x.window, Messages: x.count,
	}
	var winner verdict
	valid := 0
	for _, hook := range c.o.Hooks.Errors {
		hv, ok, err := hook.ClassifyError(ctx, in)
		if err != nil {
			slog.Warn("error classification hook failed", "error", err)
			continue
		}
		if !ok {
			continue
		}
		class, known := ParseClass(hv.Reason)
		if !known {
			continue
		}
		if valid++; valid > 1 {
			continue
		}
		winner = vd(class)
		applyHints(&winner.hints, hv.Retryable, hv.Compress, hv.Rotate, hv.Fallback)
		if text := strings.TrimSpace(hv.Message); text != "" {
			winner.message, winner.override = truncateRunes(text, 500), true
		}
		if hv.Detail != nil {
			d := *hv.Detail
			winner.detail = &d
		}
	}
	if valid == 0 {
		return verdict{}, false
	}
	if valid > 1 {
		slog.Warn("error classification hooks disagreed, the first valid result in registration order won", "skipped", valid-1)
	}
	slog.Info("API error classified by plugin hook", "class", winner.class.String(), "provider", x.provider, "status", x.status)
	return winner, true
}

func (c *Classifier) profileVerdict(ctx context.Context, x *failure) (verdict, bool) {
	if x.slug == "" {
		return verdict{}, false
	}
	p, ok := c.o.Providers.Get(ctx, x.slug)
	if !ok {
		return verdict{}, false
	}
	hook, ok := p.(providers.ErrorClassifier)
	if !ok {
		return verdict{}, false
	}
	pv, ok := hook.ClassifyError(ctx, providers.ErrorInput{
		Err: x.err, Status: x.status, Code: x.code, Message: x.msg, Body: x.bodyRaw(), Model: x.model,
	})
	if !ok {
		return verdict{}, false
	}
	class, known := ParseClass(pv.Reason)
	if !known {
		return verdict{}, false
	}
	v := vd(class)
	applyHints(&v.hints, pv.Retryable, pv.Compress, pv.Rotate, pv.Fallback)
	if pv.Fallback != nil && *pv.Fallback && pv.Retryable == nil && !RetryableClient(class) {
		v.hints.Retryable = false
	}
	if pc := pv.Context; pc != nil {
		v.detail = &Detail{
			ResetAt: pc.ResetAt, BillingUnverified: pc.BillingUnverified, PossibleContentFilter: pc.PossibleContentFilter,
			UpstreamProvider: pc.UpstreamProvider, WelcomeRefusal: pc.WelcomeRefusal, WelcomeRoute: pc.WelcomeRoute,
			UsageWindow: pc.UsageWindow, DisplayMessage: pc.DisplayMessage,
		}
	}
	slog.Info("API error classified by provider profile", "class", class.String(), "provider", x.provider, "status", x.status)
	return v, true
}

func (x *failure) welcomeTier() (verdict, bool) {
	status := x.status
	if !x.anonymous {
		if x.provider == portal.Provider && status == http.StatusBadRequest &&
			portal.WelcomeRouteRefusal(status, x.msg, "") == portal.RouteNamedOnWelcomeHost {
			v := abortFallback(ClassFormatError)
			v.detail = &Detail{WelcomeRoute: portal.RouteNamedOnWelcomeHost}
			return v, true
		}
		return verdict{}, false
	}
	if status == http.StatusPaymentRequired && portal.RouteIsWelcomeHost(x.baseURL) {
		v := abortFallback(ClassAuthPermanent)
		v.detail = &Detail{WelcomeRefusal: &portal.Refusal{Reason: "refused", Alternates: []string{}}}
		return v, true
	}
	if status == http.StatusTooManyRequests {
		refusal, ok := portal.ParseWelcomeRefusal(x.errObj().Raw())
		if !ok {
			refusal, ok = portal.ParseWelcomeRefusal(x.body.Raw())
		}
		if !ok {
			var wait time.Duration
			if seconds, found := x.resetSeconds(); found {
				wait = secondsDuration(seconds)
			}
			refusal = portal.WelcomeRefusalFromHeaders(x.header, wait)
		}
		d := &Detail{WelcomeRefusal: &refusal}
		if portal.WelcomeTierGate(refusal.Reason) {
			v := abortFallback(ClassModelNotFound)
			v.detail = d
			return v, true
		}
		if refusal.RetryAfter > 0 {
			d.ResetAt = x.now.Add(time.Duration(refusal.RetryAfter) * time.Second)
		}
		v := vd(ClassRateLimit)
		v.hints.Fallback = true
		v.detail = d
		return v, true
	}
	base := ""
	if !containsAny(x.msg, welcome403NamedPatterns) {
		base = x.baseURL
	}
	kind := portal.WelcomeRouteRefusal(status, x.msg, base)
	if kind == "" {
		return verdict{}, false
	}
	class := ClassFormatError
	if status == http.StatusForbidden {
		class = ClassAuthPermanent
	}
	v := abortFallback(class)
	v.detail = &Detail{WelcomeRoute: kind}
	return v, true
}

func anthropicThinkingRejection(msg string) bool {
	return strings.Contains(msg, "thinking") &&
		(containsAny(msg, thinkingMutationWords) || (strings.Contains(msg, "redacted_thinking") && strings.Contains(msg, "invalid")))
}

func (x *failure) specialCases() (verdict, bool) {
	msg, status := x.msg, x.status
	if x.lcode == "data_policy_denied" {
		return verdict{class: ClassDataPolicyDenied}, true
	}
	if v, ok := x.welcomeTier(); ok {
		return v, true
	}
	if containsAny(msg, contentPolicyBlockedPatterns) {
		return vContentBlocked, true
	}
	if containsAny(msg, accountPolicyBlockPatterns) {
		return vPolicyBlocked, true
	}
	if x.codexMaskedReplay() {
		return abortFallback(ClassInvalidEncryptedContent), true
	}
	if status == 400 && (x.lcode == "thinking_signature_invalid" || strings.Contains(msg, "thinking_signature_invalid")) {
		return vInvalidEncrypted, true
	}
	if status == 400 && anthropicThinkingRejection(msg) {
		return vd(ClassThinkingSignature), true
	}
	if status == 429 && strings.Contains(msg, "extra usage") && strings.Contains(msg, "long context") {
		return verdict{class: ClassLongContextTier, hints: Hints{Retryable: true, Compress: true}}, true
	}
	if status == 400 && strings.Contains(msg, "long context beta") && strings.Contains(msg, "not yet available") {
		return vd(ClassOAuthLongContextBeta), true
	}
	grammar := strings.Contains(msg, "error parsing grammar") || strings.Contains(msg, "json-schema-to-grammar") ||
		(strings.Contains(msg, "unable to generate parser") && strings.Contains(msg, "template")) ||
		(strings.Contains(msg, "invalid json schema") && strings.Contains(msg, "regex lookaround") && strings.Contains(msg, "not supported"))
	if status == 400 && grammar && !strings.Contains(msg, noUserQuerySignal) {
		return vd(ClassGrammarPattern), true
	}
	if strings.Contains(msg, "do not have an active grok subscription") ||
		(strings.Contains(msg, "out of available resources") && strings.Contains(msg, "grok")) {
		return vAuthFallback, true
	}
	return verdict{}, false
}

func (x *failure) codexMaskedReplay() bool {
	if x.slug != "openai-codex" || (x.status != 0 && x.status != 400) {
		return false
	}
	detail := x.body.Get("detail")
	if detail.Truthy() && strings.ToLower(strings.TrimSpace(detail.Display())) == codexUnsupportedContentDetail {
		return true
	}
	if len(x.body.Fields()) == 0 && strings.Contains(x.msg, codexUnsupportedContentDetail) && strings.Contains(x.msg, "detail") {
		return true
	}
	bodyMsg := ""
	for _, m := range bodyMessageCandidates(x.body) {
		if m.Truthy() {
			bodyMsg = strings.ToLower(strings.TrimSpace(m.Display()))
			break
		}
	}
	return (x.lcode == "invalid_prompt" && bodyMsg == codexMaskedReplayMessage) ||
		strings.TrimSpace(x.msg) == "invalid_prompt: "+codexMaskedReplayMessage
}

func (x *failure) presetCase() (verdict, bool) {
	var pe *PresetNotFoundError
	if errors.As(x.err, &pe) {
		return verdict{class: ClassModelNotFound}, true
	}
	return verdict{}, false
}

func (x *failure) byCode() (verdict, bool) {
	if x.lcode == wire.StreamNonJSON && strings.Contains(x.msg, "request validation failed:") {
		return vFormatError, true
	}
	if v, ok := codeVerdict(x.lcode); ok {
		return v, true
	}
	family := x.slug
	if f, ok := providerCodeFamilies[x.slug]; ok {
		family = f
	}
	return familyCodeVerdict(family, x.lcode)
}

func (x *failure) byMessage() (verdict, bool) {
	if v, ok := firstMatch(x.msg, messageHeadRules, imageToolRules); ok {
		return v, true
	}
	if containsAny(x.msg, usageLimitPatterns) {
		return classify402(x.msg, "", wire.Value{}), true
	}
	return firstMatch(x.msg, messageTailRules)
}

func (x *failure) byTransport() (verdict, bool) {
	msg, err := x.msg, x.err
	if containsAny(msg, sslCertVerifyPatterns) || certVerifyFailure(err) {
		return vSSLCert, true
	}
	if containsAny(msg, sslTransientPatterns) || tlsFailure(err) {
		return vTimeout, true
	}
	if x.status == 0 && (containsAny(msg, serverDisconnectPatterns) || connectionDropped(err)) {
		if _, ok := ReasoningStaleFloor(x.model); ok {
			return vTimeout, true
		}
		if x.largeSession(0.6, 120000, 200) {
			return vContextOverflow, true
		}
		return vTimeout, true
	}
	var stale *StaleCallError
	if errors.As(err, &stale) {
		return abortFallback(ClassTimeout), true
	}
	if transportFailure(err) {
		return vTimeout, true
	}
	return verdict{}, false
}

func (c *Classifier) byStatus(ctx context.Context, x *failure) (verdict, bool) {
	status := x.status
	switch status {
	case 0:
		return verdict{}, false
	case 400:
		return x.classify400(), true
	case 401:
		return vAuthRotate, true
	case 402:
		code := ""
		if x.portalRoute() {
			code = x.lcode
		}
		return classify402(x.msg, code, x.body), true
	case 403:
		return x.status403(), true
	case 404:
		return c.status404(ctx, x), true
	case 408:
		return vTimeout, true
	case 413:
		return vPayloadTooLarge, true
	case 422:
		if oversizedContentRejection(x.body) {
			return vImageTooLarge, true
		}
		if v, ok := firstMatch(x.msg, imageToolRules); ok {
			return v, true
		}
		return vFormatError, true
	case 429:
		return x.status429(), true
	case 500, 502:
		return x.status5xx(), true
	case 503, 529:
		if v, ok := firstMatch(x.msg, overflowAs5xxRules); ok {
			return v, true
		}
		return vOverloaded, true
	}
	switch {
	case status >= 400 && status < 500:
		return vFormatError, true
	case status >= 500 && status < 600:
		return vServerError, true
	}
	return verdict{}, false
}

func (x *failure) status403() verdict {
	if transient403Codes[x.lcode] {
		return vOverloaded
	}
	xaiSpend := x.slug == "xai-oauth" && x.lcode == xaiSpendingLimitCode
	if xaiSpend || containsAny(x.msg, status403BillingPatterns) {
		return vBilling
	}
	if containsAny(x.msg, upstreamBlockedPatterns) {
		return vUpstreamBlocked
	}
	return vAuthFallback
}

func (c *Classifier) status404(ctx context.Context, x *failure) verdict {
	if billingErrorCodes[x.lcode] {
		return vBilling
	}
	if v, ok := firstMatch(x.msg, rules404); ok {
		return v
	}
	name := strings.TrimSpace(x.modelSlug)
	if name != "" && !strings.Contains(name, "/") && c.o.PrefixedModel(ctx, strings.TrimSpace(x.slug), name) != "" {
		return vModelNotFound
	}
	return vUnknown
}

func (x *failure) status429() verdict {
	if billingErrorCodes[x.lcode] {
		return vBilling
	}
	if containsAny(x.msg, overloadedPatterns) {
		return vOverloaded
	}
	if x.openRouterUpstream() {
		v := vd(ClassUpstreamRateLimit)
		v.hints.Fallback = true
		if name := strings.TrimSpace(x.errObj().Path("metadata", "provider_name").Text()); name != "" {
			v.detail = &Detail{UpstreamProvider: name}
		}
		return v
	}
	quotaWall := x.lcode == "usage_limit_reached" || containsAny(x.msg, quotaWallPatterns)
	if quotaWall && !containsAny(x.msg, rateLimitPatterns) && !x.usageLimitTransientSignal() {
		return vBilling
	}
	if seconds, ok := x.resetSeconds(); ok && seconds != 0 {
		v := vRateLimit
		v.detail = &Detail{ResetAt: x.now.Add(secondsDuration(seconds))}
		return v
	}
	return vRateLimit
}

func (x *failure) status5xx() verdict {
	validation := containsAny(x.msg, requestValidationPatterns) || validation5xxCodes[x.lcode]
	if validation && !serverInjectedParamRejection(x.msg, x.slug) {
		return vFormatError
	}
	if v, ok := firstMatch(x.msg, overflowAs5xxRules); ok {
		return v
	}
	return vServerError
}

func (x *failure) portalRoute() bool {
	if strings.ToLower(strings.TrimSpace(x.provider)) == portal.Provider {
		return true
	}
	return x.baseURL != "" && providers.HostMatches(x.baseURL, "icebath.app")
}

func classify402(msg, code string, body wire.Value) verdict {
	if code == portalUsageWindowCode {
		v := vBilling
		v.hints.Fallback = false
		v.detail = &Detail{UsageWindow: true, DisplayMessage: usageWindowDisplayMessage(body)}
		return v
	}
	if containsAny(msg, usageLimitPatterns) && containsAny(msg, usageLimitTransientSignals) {
		return vRateLimit
	}
	return vBilling
}

func usageWindowDisplayMessage(body wire.Value) string {
	if !body.IsObject() {
		return ""
	}
	for _, holder := range []wire.Value{body, body.Get("error")} {
		if !holder.IsObject() {
			continue
		}
		if text, ok := holder.Get("display_message").Str(); ok && strings.TrimSpace(text) != "" {
			return truncateRunes(strings.TrimSpace(text), 500)
		}
	}
	return ""
}

func hasLargeInlineImage(content wire.Value) bool {
	for _, part := range content.Elems() {
		if !part.IsObject() {
			continue
		}
		image := part.Get("image_url")
		url := image
		if image.IsObject() {
			url = image.Get("url")
		}
		if s, ok := url.Str(); ok && strings.HasPrefix(s, "data:image/") && len(s) > imageShrinkTargetBytes {
			return true
		}
	}
	return false
}

func oversizedContentRejection(body wire.Value) bool {
	for _, detail := range body.Get("detail").Elems() {
		if !detail.IsObject() {
			continue
		}
		loc := detail.Get("loc")
		if detail.Get("type").Text() != "string_type" || !loc.IsArray() {
			continue
		}
		items := loc.Elems()
		if len(items) < 2 {
			continue
		}
		parts := make([]string, len(items))
		toolScoped := false
		for i, item := range items {
			parts[i] = strings.ToLower(item.Display())
			toolScoped = toolScoped || strings.HasPrefix(parts[i], "tool")
		}
		n := len(parts)
		if parts[0] == "body" && parts[1] == "messages" && parts[n-2] == "content" && parts[n-1] == "str" &&
			!toolScoped && hasLargeInlineImage(detail.Get("input")) {
			return true
		}
	}
	return false
}

func (x *failure) classify400() verdict {
	msg, code := x.msg, x.lcode
	if oversizedContentRejection(x.body) {
		return vImageTooLarge
	}
	if v, ok := firstMatch(msg, imageToolRules); ok {
		return v
	}
	if strings.Contains(msg, CodexEntitlementMarker) {
		return vModelEntitlement
	}
	if code == "invalid_encrypted_content" || strings.Contains(msg, "invalid_encrypted_content") ||
		(strings.Contains(msg, "encrypted content for item") && strings.Contains(msg, "could not be verified")) ||
		strings.Contains(msg, "could not decrypt the provided encrypted_content") ||
		strings.Contains(msg, "encrypted content could not be decrypted or parsed") ||
		(strings.Contains(msg, "encrypted_content") && strings.Contains(msg, "was not issued to this caller")) ||
		strings.Contains(msg, "conflicting authenticated continuation identities") {
		return vInvalidEncrypted
	}
	if strings.Contains(msg, reasoningMandatoryPattern) || ReasoningFieldRejection(msg) || ReasoningLevelRejection(msg) {
		return vReasoningMand
	}
	if serverInjectedParamRejection(msg, x.slug) {
		return vServerError
	}
	if containsAny(msg, malformedToolArgsPatterns) {
		return vMalformedArgs
	}
	if containsAny(msg, roleAlternationPatterns) {
		return vRoleAlternation
	}
	if containsAny(msg, validation400Patterns) || validation400Codes[code] {
		return vFormatError
	}
	if containsAny(msg, invalidMessageBodyPatterns) || code == "invalid_request_body" {
		slog.Warn("malformed message array 400 classified as format_error, not context overflow, failing fast and falling back "+
			"instead of entering the compression loop, an empty-content assistant stub is usually in the transcript",
			"num_messages", x.count, "approx_tokens", x.approx, "error", truncateRunes(msg, 200))
		return vFormatError
	}
	if memoryCeilingCodes[code] {
		return vOverloaded
	}
	if v, ok := firstMatch(msg, overflowAs5xxRules, tail400Rules); ok {
		return v
	}
	bodyMsg := ""
	for _, m := range bodyMessageCandidates(x.body) {
		text := ""
		if m.Truthy() {
			text = strings.ToLower(strings.TrimSpace(m.Display()))
		}
		if text != "" {
			bodyMsg = text
			break
		}
	}
	generic := len([]rune(bodyMsg)) < 30 || bodyMsg == "error"
	if generic && x.largeSession(0.4, 80000, 80) {
		return vContextOverflow
	}
	return vFormatError
}

func serverInjectedParamRejection(msg, provider string) bool {
	slug := strings.ToLower(strings.TrimSpace(provider))
	for _, entry := range serverInjectedParamSenders {
		if msg != "" && strings.Contains(msg, entry.param) && containsAny(msg, paramRejectionWords) {
			return !containsAny(slug, entry.senders)
		}
	}
	return false
}

func (x *failure) openRouterUpstream() bool {
	obj := x.errObj()
	if strings.ToLower(strings.TrimSpace(obj.Get("message").Display())) != "provider returned error" || !obj.Get("message").Truthy() {
		return false
	}
	if x.slug == "openrouter" {
		return true
	}
	metadata := obj.Get("metadata")
	return metadata.IsObject() && (metadata.Get("raw").Exists() || metadata.Get("provider_name").Exists())
}

func (x *failure) payloads() []wire.Value {
	out := []wire.Value{}
	if x.body.IsObject() {
		out = append(out, x.body)
	}
	if obj := x.errObj(); obj.IsObject() {
		out = append(out, obj)
	}
	return out
}

func presentValue(v wire.Value) bool {
	if !v.Exists() || v.Kind() == wire.KindNull {
		return false
	}
	s, isString := v.Str()
	return !isString || s != ""
}

func (x *failure) usageLimitTransientSignal() bool {
	if containsAny(x.msg, usageLimitTransientSignals) {
		return true
	}
	for _, payload := range x.payloads() {
		for _, name := range resetFields {
			if presentValue(payload.Get(name)) {
				return true
			}
		}
	}
	for _, name := range resetHeaders {
		if x.header.Get(name) != "" {
			return true
		}
	}
	return false
}

func (x *failure) resetSeconds() (float64, bool) {
	for _, payload := range x.payloads() {
		for _, name := range resetFields {
			value := payload.Get(name)
			if !presentValue(value) {
				continue
			}
			if strings.HasSuffix(name, "_at") {
				if f, ok := numberValue(value); ok {
					return max(0, f-epochSeconds(x.now)), true
				}
			}
			if seconds, ok := retryAfterValue(value, x.now); ok {
				return seconds, true
			}
		}
	}
	if d, ok := httpclient.RetryAfter(x.header, x.now); ok {
		return d.Seconds(), true
	}
	if d, ok := ResetDelayFromMessage(x.msg); ok {
		return d.Seconds(), true
	}
	return 0, false
}

func bodyMessageCandidates(body wire.Value) []wire.Value {
	if !body.IsObject() {
		return nil
	}
	obj := body.Get("error")
	if !obj.IsObject() {
		obj = wire.Value{}
	}
	out := []wire.Value{obj.Get("message"), body.Get("message"), body.Get("errorMessage")}
	if args := body.Get("errorArgs"); args.IsObject() {
		out = append(out, args.Get("reason"))
	}
	switch detail := body.Get("detail"); {
	case detail.IsObject():
		out = append(out, detail.Get("message"))
	case detail.IsString():
		out = append(out, detail)
	}
	return out
}

func buildErrorText(text string, body wire.Value) string {
	raw := strings.ToLower(text)
	bodyMsg, metadataMsg := "", ""
	if body.IsObject() {
		obj := body.Get("error")
		if !obj.IsObject() {
			obj = wire.Value{}
		}
		if m := obj.Get("message"); m.Truthy() {
			bodyMsg = strings.ToLower(m.Display())
		}
		if m := body.Get("message"); bodyMsg == "" && m.Truthy() {
			bodyMsg = strings.ToLower(m.Display())
		}
		if obj.IsObject() && len(obj.Fields()) > 0 {
			metadataMsg = openRouterWrappedMessage(obj)
		}
	}
	parts := []string{raw}
	if bodyMsg != "" && !strings.Contains(raw, bodyMsg) {
		parts = append(parts, bodyMsg)
	}
	if metadataMsg != "" && !strings.Contains(raw, metadataMsg) && !strings.Contains(bodyMsg, metadataMsg) {
		parts = append(parts, metadataMsg)
	}
	return strings.Join(parts, " ")
}

func jsonObjectText(text string) (wire.Value, bool) {
	if strings.TrimSpace(text) == "" {
		return wire.Value{}, false
	}
	v, err := wire.ParseValue([]byte(text))
	if err != nil || !v.IsObject() {
		return wire.Value{}, false
	}
	return v, true
}

func openRouterWrappedMessage(obj wire.Value) string {
	metadata := obj.Get("metadata")
	if !metadata.IsObject() {
		return ""
	}
	raw, ok := metadata.Get("raw").Str()
	if !ok {
		return ""
	}
	inner, ok := jsonObjectText(raw)
	if !ok {
		return ""
	}
	if m := inner.Path("error", "message"); m.Truthy() && inner.Get("error").IsObject() {
		return strings.ToLower(m.Display())
	}
	return ""
}

func integerValue(v wire.Value) (int64, bool) {
	if v.Kind() != wire.KindNumber || strings.ContainsAny(string(v.Raw()), ".eE") {
		return 0, false
	}
	return v.Int()
}

func statusFromBody(body wire.Value) int {
	if !body.IsObject() {
		return 0
	}
	obj := body.Get("error")
	var candidates []wire.Value
	if obj.IsObject() {
		candidates = append(candidates, obj.Get("status_code"), obj.Get("status"), obj.Get("http_status"), obj.Get("code"))
	}
	candidates = append(candidates, body.Get("code"))
	for _, c := range candidates {
		if n, ok := integerValue(c); ok && n >= 400 && n < 600 {
			return int(n)
		}
	}
	return 0
}

func codeFromPayload(payload wire.Value, topKeys []string, peek bool) string {
	if !payload.IsObject() {
		return ""
	}
	if obj := payload.Get("error"); obj.IsObject() {
		code := obj.Get("code")
		if !code.Truthy() {
			code = obj.Get("type")
		}
		text, isString := "", !code.Truthy()
		if code.Truthy() && !code.IsString() {
			if status := obj.Get("status"); status.Truthy() {
				code = status
			}
		}
		if s, ok := code.Str(); ok && code.Truthy() {
			text, isString = s, true
		}
		if trimmed := strings.TrimSpace(text); isString && trimmed != "" && trimmed != "400" {
			return trimmed
		}
		if message, ok := obj.Get("message").Str(); peek && ok && strings.HasPrefix(strings.TrimSpace(message), "{") {
			if inner, ok := jsonObjectText(message); ok {
				if nested := codeFromPayload(inner, []string{"code", "error_code"}, false); nested != "" {
					return nested
				}
			}
		}
	}
	for _, key := range topKeys {
		v := payload.Get(key)
		if !v.Truthy() {
			continue
		}
		text := ""
		if s, ok := v.Str(); ok {
			text = strings.TrimSpace(s)
		} else if n, ok := integerValue(v); ok {
			text = fmt.Sprint(n)
		}
		if text != "" && text != "400" {
			return text
		}
		return ""
	}
	return ""
}

func extractMessage(x *failure) string {
	for _, m := range bodyMessageCandidates(x.body) {
		if s, ok := m.Str(); ok && strings.TrimSpace(s) != "" {
			return truncateRunes(strings.TrimSpace(s), 500)
		}
	}
	return truncateRunes(safeText(x.err), 500)
}

func safeText(err error) string {
	return wire.MaskCredentials(httpclient.Message(err))
}

func containsAny(text string, patterns []string) bool {
	for _, p := range patterns {
		if strings.Contains(text, p) {
			return true
		}
	}
	return false
}

func truncateRunes(s string, n int) string {
	count := 0
	for i := range s {
		if count == n {
			return s[:i]
		}
		count++
	}
	return s
}
