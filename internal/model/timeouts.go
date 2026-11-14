package model

import (
	"fmt"
	"log/slog"
	"math"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/nickheyer/icebath/internal/model/providers"
	"github.com/nickheyer/icebath/internal/model/wire"
	"github.com/nickheyer/icebath/internal/model/wire/gemini"
)

const (
	highEffortSilenceFloor   = 300.0
	codexFirstProgressBudget = 300.0
)

var (
	bedrockRegionPrefixes = []string{"global.", "us.", "eu.", "apac.", "ap.", "au.", "jp.", "ca.", "sa.", "me.", "af."}
	digitDashDigit        = regexp.MustCompile(`(\d)-(\d)`)
	digitDotDigit         = regexp.MustCompile(`(\d)\.(\d)`)
	gpt55Family           = regexp.MustCompile(`(?:^|[/\-_])gpt-5\.5(?:$|[\-_])`)
)

func inf() float64 { return math.Inf(1) }

func finite(v float64) bool { return !math.IsInf(v, 0) && !math.IsNaN(v) }

func seconds(v float64) time.Duration {
	if !finite(v) || v > float64(math.MaxInt64)/float64(time.Second) {
		return time.Duration(math.MaxInt64)
	}
	return time.Duration(v * float64(time.Second))
}

func thousands(n int) string {
	s := strconv.Itoa(n)
	neg := strings.HasPrefix(s, "-")
	if neg {
		s = s[1:]
	}
	var b strings.Builder
	for i, r := range s {
		if i > 0 && (len(s)-i)%3 == 0 {
			b.WriteByte(',')
		}
		b.WriteRune(r)
	}
	if neg {
		return "-" + b.String()
	}
	return b.String()
}

func openAICodexStaleFloor(est int) float64 {
	switch {
	case est > 100000:
		return 1200
	case est > 50000:
		return 900
	case est > 10000:
		return 600
	}
	return 0
}

func (x *exchange) boundCodexStale(stale float64) float64 {
	if floor := openAICodexStaleFloor(x.est); floor > 0 {
		stale = math.Max(stale, floor)
	}
	hard := x.k.codexHard.or(1500)
	if hard > 0 {
		return math.Min(stale, hard)
	}
	return stale
}

func scaleStaleForContext(base float64, est int) float64 {
	switch {
	case est > 100000:
		return math.Max(base, 300)
	case est > 50000:
		return math.Max(base, 240)
	}
	return base
}

func bedrockReasoningStaleFloor(modelID string) (time.Duration, bool) {
	name := strings.ToLower(strings.TrimSpace(modelID))
	if name == "" {
		return 0, false
	}
	for _, prefix := range bedrockRegionPrefixes {
		if strings.HasPrefix(name, prefix) {
			name = name[len(prefix):]
			break
		}
	}
	bases := []string{name}
	if strings.Contains(name, ".") {
		bases = append(bases, name[strings.LastIndex(name, ".")+1:], strings.Replace(name, ".", "-", 1))
	}
	seen := map[string]bool{}
	for _, cand := range bases {
		for _, form := range []string{cand, swapSeparators(cand, digitDashDigit, "."), swapSeparators(cand, digitDotDigit, "-")} {
			if seen[form] {
				continue
			}
			seen[form] = true
			if floor, ok := ReasoningStaleFloor(form); ok {
				return floor, true
			}
		}
	}
	return 0, false
}

func swapSeparators(s string, re *regexp.Regexp, sep string) string {
	for {
		next := re.ReplaceAllString(s, "${1}"+sep+"${2}")
		if next == s {
			return s
		}
		s = next
	}
}

func (x *exchange) capToRunBudget(timeout float64) float64 {
	if x.o.RunBudget <= 0 || x.o.RunStarted.IsZero() {
		return timeout
	}
	remaining := x.o.RunBudget.Seconds() - time.Since(x.o.RunStarted).Seconds()
	return math.Min(timeout, math.Max(60, remaining*0.5))
}

func (x *exchange) reasoningFloor() (float64, bool) {
	if floor, ok := ReasoningStaleFloor(x.model); ok {
		return floor.Seconds(), true
	}
	if x.mode == wire.ModeBedrock {
		if floor, ok := bedrockReasoningStaleFloor(x.model); ok {
			return floor.Seconds(), true
		}
	}
	return 0, false
}

func (x *exchange) cloudStaleTimeout() float64 {
	if x.k.staleSet {
		return x.k.providerStale
	}
	base, explicitEnv := x.k.streamEnvStaleBase()
	timeout := scaleStaleForContext(base, x.est)
	if floor, ok := x.reasoningFloor(); ok {
		timeout = math.Max(timeout, floor)
	}
	if explicitEnv {
		return timeout
	}
	return x.capToRunBudget(timeout)
}

func (x *exchange) streamStaleTimeout() float64 {
	if x.k.configuredStaleBase() == 180 && x.local {
		timeout := x.k.localStaleDefault()
		slog.Debug(fmt.Sprintf("Local provider detected (%s) — stale stream timeout set to %.0fs", x.rt.BaseURL, timeout))
		return timeout
	}
	return x.cloudStaleTimeout()
}

func (x *exchange) streamTimeouts(stale float64) (write, read, conn float64) {
	if x.k.requestSet {
		base := x.k.providerRequest
		return base, base, math.Min(base, 60)
	}
	base := x.k.apiTimeout.or(1800)
	read = x.k.streamRead.or(120)
	if read == 120 && x.local {
		read = base
		slog.Debug(fmt.Sprintf("Local provider detected (%s) — stream read timeout raised to %.0fs", x.rt.BaseURL, read))
	} else if read == 120 && finite(stale) && stale > read {
		read = stale
		slog.Debug(fmt.Sprintf("Cloud reasoning stream — read timeout raised to %.0fs to match stale-stream detector", read))
	}
	return base, read, 30
}

func (x *exchange) highEffortFloor() float64 {
	r := x.s.o.Reasoning
	if r == nil || !r.Enabled {
		return 0
	}
	effort := strings.ToLower(strings.TrimSpace(r.Effort))
	rank := wire.EffortRank(effort)
	if rank < 0 || rank < wire.EffortRank("high") {
		return 0
	}
	return highEffortSilenceFloor
}

func (x *exchange) longDecodeMultiplier() float64 {
	raw, ok := x.p.Request.ExtraBody["max_output_tokens"]
	if !ok {
		return 1
	}
	v, err := wire.ParseValue(raw)
	if err != nil {
		return 1
	}
	n, isInt := v.Int()
	if !isInt {
		return 1
	}
	return float64(gemini.LongDecodeHops(int(n)))
}

func (x *exchange) nonStreamStaleBase() (float64, bool) {
	if x.k.staleSet {
		return x.k.providerStale, false
	}
	if x.k.callStale.present {
		return x.k.callStale.value, false
	}
	if floor, ok := ReasoningStaleFloor(x.model); ok {
		return floor.Seconds(), false
	}
	return 90, true
}

func (x *exchange) nonStreamStaleTimeout() float64 {
	base, implicit := x.nonStreamStaleBase()
	if implicit && x.local {
		return inf()
	}
	timeout := base
	switch {
	case x.est > 100000:
		timeout = math.Max(base, 240)
	case x.est > 50000:
		timeout = math.Max(base, 150)
	}
	explicit := x.k.staleSet || x.k.callStale.present
	if x.mode == wire.ModeResponses && !explicit {
		timeout = math.Max(timeout, x.highEffortFloor())
	}
	if !explicit {
		timeout *= x.longDecodeMultiplier()
		timeout = x.capToRunBudget(timeout)
	}
	return timeout
}

type watchdogs struct {
	stale            float64
	codex            bool
	ttfbOn           bool
	ttfb             float64
	idleOn           bool
	idle             float64
	idleNeedProgress bool
	progress         float64
}

func (x *exchange) nonStreamWatchdogs() watchdogs {
	wd := watchdogs{stale: x.nonStreamStaleTimeout(), codex: x.mode == wire.ModeResponses}
	codexBackend := x.s.codexBackendOf(x.rt)
	effortFloor := 0.0
	if wd.codex {
		effortFloor = x.highEffortFloor()
	}
	codexFloor := 0.0
	if wd.codex && !x.local {
		codexFloor = openAICodexStaleFloor(x.est)
		wd.stale = x.boundCodexStale(wd.stale)
	}
	idleDefault := 12.0
	switch {
	case x.est > 100000:
		idleDefault = 180
	case x.est > 50000:
		idleDefault = 120
	case x.est > 10000:
		idleDefault = 60
	}
	idleDefault = math.Max(effortFloor, idleDefault)
	wd.ttfbOn = wd.codex
	ttfbExplicit := x.k.codexTTFB.or(-1) != -1
	wd.ttfb = x.k.codexTTFB.or(120)
	switch {
	case wd.ttfb <= 0:
		wd.ttfbOn = false
	case wd.codex && !x.local:
		above := x.k.codexTTFBAbove.or(10000)
		if !x.k.codexTTFBStrict && above > 0 && float64(x.est) >= above && wd.ttfb < idleDefault {
			slog.Info(fmt.Sprintf("Scaling codex-responses no-event TTFB watchdog from %.0fs to %.0fs for large request (context=~%s tokens >= %.0f). Set ICEBATH_CODEX_TTFB_STRICT=1 to keep the smaller cutoff.",
				wd.ttfb, idleDefault, thousands(x.est), above))
			wd.ttfb = idleDefault
		}
		if limit := x.k.codexTTFBMax.or(0); limit > 0 && wd.ttfb > limit {
			slog.Info(fmt.Sprintf("Capping codex-responses no-event TTFB timeout from %.0fs to %.0fs (context=~%s tokens) per ICEBATH_CODEX_TTFB_MAX_SECONDS.",
				wd.ttfb, limit, thousands(x.est)))
			wd.ttfb = limit
		}
	case !ttfbExplicit && x.local:
		if ceiling := x.k.localStaleDefault(); ceiling > wd.ttfb {
			slog.Info(fmt.Sprintf("Local provider detected (%s) — no-event TTFB watchdog raised from %.0fs to %.0fs (agent.local_stream_stale_timeout); set ICEBATH_CODEX_TTFB_TIMEOUT_SECONDS for an explicit cutoff.",
				x.rt.BaseURL, wd.ttfb, ceiling))
			wd.ttfb = ceiling
		}
	}
	if wd.ttfbOn && !ttfbExplicit {
		wd.ttfb = math.Max(wd.ttfb, effortFloor)
	}
	idleExplicit := x.k.codexIdle.or(-1) != -1
	wd.idle = x.k.codexIdle.or(idleDefault)
	wd.idleOn = wd.codex && wd.idle > 0
	wd.idleNeedProgress = wd.codex && codexBackend && codexFloor > 0 && !idleExplicit
	if wd.idleNeedProgress {
		wd.progress = codexFirstProgressBudget
	}
	return wd
}

func (x *exchange) codexSilentHangHint() string {
	if x.mode != wire.ModeResponses || !x.s.codexBackendOf(x.rt) {
		return ""
	}
	model := x.model
	if !gpt55Family.MatchString(strings.ToLower(model)) {
		return ""
	}
	return "Codex backend appears to be silently rejecting " + pyQuote(model) +
		" on chatgpt.com/backend-api/codex (no stream events, no error). " +
		"This is a known backend-side pattern that has affected ChatGPT Plus accounts intermittently. " +
		"Workaround: try `gpt-5.4` on the same OAuth profile, or switch to a different model/provider in your fallback chain. " +
		"Some ChatGPT Codex accounts do not support `gpt-5.4-codex`. See nickheyer/icebath#21444 for symptom history."
}

func pyQuote(s string) string {
	if strings.Contains(s, "'") && !strings.Contains(s, `"`) {
		return `"` + s + `"`
	}
	return "'" + strings.ReplaceAll(s, "'", `\'`) + "'"
}

func (s *Session) codexBackendOf(rt *Route) bool {
	host := strings.ToLower(providers.URLHostname(rt.BaseURL))
	return rt.Provider == "openai-codex" || ((host == "chatgpt.com" || strings.HasSuffix(host, ".chatgpt.com")) &&
		strings.Contains(strings.ToLower(rt.BaseURL), "/backend-api/codex"))
}
