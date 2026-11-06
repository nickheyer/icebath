package provider

import (
	"context"
	"fmt"
	"log/slog"
	"math"
	"net/http"
	"net/url"
	"strings"
	"time"
)

type staleStreakKey struct{}

// Context whose stale kills feed the retry session stale breaker
func withStaleStreak(ctx context.Context, s *RetrySession) context.Context {
	return context.WithValue(ctx, staleStreakKey{}, s)
}

// Counts one stale kill against the breaker of the session the context carries
func bumpStaleStreak(ctx context.Context) {
	if s, _ := ctx.Value(staleStreakKey{}).(*RetrySession); s != nil {
		s.staleStreak.Add(1)
	}
}

// Consecutive stale attempts the session has seen since the provider last answered
func (s *RetrySession) StaleStreak() int { return int(s.staleStreak.Load()) }

// Clears the stale breaker after the provider proved responsive or the route changed
func (s *RetrySession) ResetStaleStreak() { s.staleStreak.Store(0) }

// Consecutive stale attempts after which calls fail at once, zero turns the breaker off
func staleGiveup() int { return envInt("ICEBATH_STREAM_STALE_GIVEUP", 5) }

// Fails the call before any network attempt once the stale streak reaches the give up threshold
func checkStaleGiveup(s *RetrySession) error {
	giveup, streak := staleGiveup(), s.StaleStreak()
	if giveup > 0 && streak >= giveup {
		return fmt.Errorf("Provider has been unresponsive (no response received) for %d consecutive stale attempts — aborting this call to avoid an indefinite stall. Switch models or start a new session, then retry.", streak)
	}
	return nil
}

var prefillProgress = GetPrefillProgress

var localHealthClient = &http.Client{Transport: &http.Transport{Proxy: nil}, Timeout: 3 * time.Second}

// True when the llama-server at baseURL answers its health check with 200 within a few seconds
func localServerAnswers(baseURL string) bool {
	parts, err := url.Parse(baseURL)
	if err != nil || parts.Scheme == "" || parts.Hostname() == "" {
		return false
	}
	netloc := parts.Host
	if parts.User != nil {
		netloc = parts.User.String() + "@" + netloc
	}
	if strings.ToLower(parts.Hostname()) == "localhost" {
		netloc = strings.Replace(netloc, "localhost", "127.0.0.1", 1)
	}
	resp, err := localHealthClient.Get(parts.Scheme + "://" + netloc + "/health")
	if err != nil {
		return false
	}
	defer resp.Body.Close()
	return resp.StatusCode == http.StatusOK
}

// Turn start lets one attempt past an open stale breaker when the local llama-server answers its health check again
func (s *RetrySession) RetryRecoveredLocalServer(apiMode APIMode, provider, baseURL string) bool {
	giveup, streak := staleGiveup(), s.StaleStreak()
	if giveup <= 0 || streak < giveup || (apiMode != "" && apiMode != ModeChatCompletions) || !IsManagedProvider(provider, baseURL) || !localServerAnswers(baseURL) {
		return false
	}
	slog.Info(fmt.Sprintf("Local model server answers its health check after %d stale attempts; trying once more.", streak))
	s.staleStreak.Store(int64(giveup - 1))
	return true
}

// Live weight load or prompt reading notice while the managed local server works before the first token
func managedLocalLoadNotice(baseURL, model string, estTokens int) (string, bool) {
	if baseURL == "" {
		return "", false
	}
	managed := auxReadManagedLocalNetloc()
	parts, err := pyURLParse(baseURL, "")
	if managed == "" || err != nil || strings.ToLower(parts.netloc) != managed {
		return "", false
	}
	if progress, ok := GetLoadingProgress()[model]; ok {
		return fmt.Sprintf("⏳ loading %s into memory — %d%% (responses start once the model is loaded)", model, progress.Percent), true
	}
	processed, ok, err := prefillProgress(model)
	if err != nil {
		slog.Debug("local load notice unavailable", "err", err)
		return "", false
	}
	if !ok {
		return "", false
	}
	if estTokens > 0 && int64(estTokens) >= processed {
		pct := int(math.RoundToEven(float64(processed) / float64(estTokens) * 100))
		return fmt.Sprintf("⚙ processing prompt — %d%%", max(0, min(100, pct))), true
	}
	return "⚙ processing prompt", true
}
