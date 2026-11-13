package connections

import (
	"fmt"
	"math"
	"sync"
	"time"
)

// OauthLoginOutcome is a finished login window for a gateway
type OauthLoginOutcome struct {
	BaseURL string `json:"baseUrl"`
	OK      bool   `json:"ok"`
}

// LoginAttempt is a running login whose window a foreground caller may reveal
type LoginAttempt struct {
	Done   <-chan struct{}
	Result func() (OauthLoginOutcome, error)
	Reveal func()
}

// SilentLoginSuppressedError holds a background login back after recent failures
type SilentLoginSuppressedError struct{ RetryAfter time.Duration }

func (e *SilentLoginSuppressedError) Error() string {
	return fmt.Sprintf("A silent sign-in for this gateway failed recently; the next attempt is held for %ds.", int(math.Ceil(e.RetryAfter.Seconds())))
}

// LoginJoinTimeoutError stops a background caller from waiting on another caller's open window
type LoginJoinTimeoutError struct{ Timeout time.Duration }

func (e *LoginJoinTimeoutError) Error() string {
	return fmt.Sprintf("A sign-in for this gateway is still open; stopped waiting after %ds.", int(math.Ceil(e.Timeout.Seconds())))
}

// LoginFlightKey keys a login flow by cookie jar and gateway origin
func LoginFlightKey(partition, baseURL string) string {
	origin := baseURL
	if parsed, err := parseURL(baseURL); err == nil {
		origin = urlOrigin(parsed.Protocol(), parsed.Host())
	}
	return partition + " " + origin
}

// Origin of a parsed URL the way URL.prototype.origin serializes it
func urlOrigin(protocol, host string) string {
	switch protocol {
	case "http:", "https:", "ws:", "wss:", "ftp:":
		return protocol + "//" + host
	}
	return "null"
}

type loginFlight struct {
	done    chan struct{}
	outcome OauthLoginOutcome
	err     error
	reveal  func()
}

type loginFailure struct {
	consecutive  int
	blockedUntil time.Time
}

// OauthLoginFlights keeps one login flow per jar and origin with backoff for failed background logins
type OauthLoginFlights struct {
	mu       sync.Mutex
	inFlight map[string]*loginFlight
	failures map[string]loginFailure
}

// NewOauthLoginFlights uses the 5 s base, 60 s cap and 15 s join budget
func NewOauthLoginFlights() *OauthLoginFlights {
	return &OauthLoginFlights{inFlight: map[string]*loginFlight{}, failures: map[string]loginFailure{}}
}

const (
	loginBaseDelay   = 5 * time.Second
	loginMaxDelay    = 60 * time.Second
	loginJoinTimeout = 15 * time.Second
)

// IsRunning reports a flow in flight for the key
func (f *OauthLoginFlights) IsRunning(key string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.inFlight[key] != nil
}

func (f *OauthLoginFlights) cooldownLocked(key string) time.Duration {
	failure, ok := f.failures[key]
	if !ok {
		return 0
	}
	return max(0, time.Until(failure.blockedUntil))
}

func (f *OauthLoginFlights) recordFailureLocked(key string) {
	consecutive := f.failures[key].consecutive + 1
	delay := min(time.Duration(float64(loginBaseDelay)*math.Pow(2, float64(consecutive-1))), loginMaxDelay)
	f.failures[key] = loginFailure{consecutive: consecutive, blockedUntil: time.Now().Add(delay)}
}

// Run joins the flow of the key or starts one, a foreground join revealing the window
func (f *OauthLoginFlights) Run(key string, foreground bool, start func() (LoginAttempt, error)) (OauthLoginOutcome, error) {
	f.mu.Lock()
	if running := f.inFlight[key]; running != nil {
		f.mu.Unlock()
		if foreground {
			running.reveal()
			<-running.done
			return running.outcome, running.err
		}
		timer := time.NewTimer(loginJoinTimeout)
		defer timer.Stop()
		select {
		case <-running.done:
			return running.outcome, running.err
		case <-timer.C:
			return OauthLoginOutcome{}, &LoginJoinTimeoutError{Timeout: loginJoinTimeout}
		}
	}
	if !foreground {
		if remaining := f.cooldownLocked(key); remaining > 0 {
			f.mu.Unlock()
			return OauthLoginOutcome{}, &SilentLoginSuppressedError{RetryAfter: remaining}
		}
	}
	attempt, err := start()
	if err != nil {
		f.recordFailureLocked(key)
		f.mu.Unlock()
		return OauthLoginOutcome{}, err
	}
	flight := &loginFlight{done: make(chan struct{}), reveal: attempt.Reveal}
	f.inFlight[key] = flight
	f.mu.Unlock()
	<-attempt.Done
	outcome, err := attempt.Result()
	f.mu.Lock()
	if err != nil {
		f.recordFailureLocked(key)
	} else {
		delete(f.failures, key)
	}
	flight.outcome, flight.err = outcome, err
	if f.inFlight[key] == flight {
		delete(f.inFlight, key)
	}
	f.mu.Unlock()
	close(flight.done)
	return outcome, err
}
