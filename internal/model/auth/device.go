package auth

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/nickheyer/icebath/internal/home"
	"github.com/nickheyer/icebath/internal/httpclient"
)

type Console struct {
	In     io.Reader
	Out    io.Writer
	Secret func(prompt string) (string, error)
}

func (c Console) printf(format string, args ...any) {
	if c.Out != nil {
		fmt.Fprintf(c.Out, format, args...)
	}
}

func (c Console) println(text string) { c.printf("%s\n", text) }

type DeviceReply struct {
	Status int
	Header http.Header
	Body   []byte
}

type DevicePollOptions struct {
	Post           func(ctx context.Context) (DeviceReply, error)
	ExpiresIn      time.Duration
	Interval       time.Duration
	Validate       func(payload json.RawMessage) error
	OnNonJSONError func(r DeviceReply) error
	OnError        func(r DeviceReply, payload json.RawMessage) error
	OnTimeout      func(cause error) error
	Cancelled      func() bool
}

func cancellableSleep(ctx context.Context, d time.Duration, cancelled func() bool) error {
	if d <= 0 {
		if cancelled != nil && cancelled() {
			return ErrDeviceFlowCancelled
		}
		return ctx.Err()
	}
	wake := time.Now().Add(d)
	for {
		if cancelled != nil && cancelled() {
			return ErrDeviceFlowCancelled
		}
		remaining := time.Until(wake)
		if remaining <= 0 {
			return nil
		}
		if cancelled != nil && remaining > time.Second {
			remaining = time.Second
		}
		timer := time.NewTimer(remaining)
		select {
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		case <-timer.C:
		}
	}
}

func PollDeviceToken(ctx context.Context, o DevicePollOptions) (json.RawMessage, error) {
	expires := o.ExpiresIn
	if expires < time.Second {
		expires = time.Second
	}
	deadline := time.Now().Add(expires)
	interval := o.Interval
	var edgeBackoff time.Duration
	unavailable := 0
	for time.Now().Before(deadline) {
		if o.Cancelled != nil && o.Cancelled() {
			return nil, ErrDeviceFlowCancelled
		}
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		r, err := o.Post(ctx)
		if err != nil {
			return nil, err
		}
		if r.Status == http.StatusOK {
			if _, err := parseJSON(r.Body); err != nil {
				return nil, err
			}
			if err := o.Validate(r.Body); err != nil {
				return nil, err
			}
			return json.RawMessage(r.Body), nil
		}
		parsed, perr := parseJSON(r.Body)
		payload, isObject := parsed.(*doc)
		if perr != nil || !isObject {
			if r.Status == 408 || r.Status == 429 || r.Status >= 500 || (r.Status == 403 && r.Header.Get("x-vercel-mitigated") != "") {
				unavailable = r.Status
				if after, ok := httpclient.RetryAfter(r.Header, time.Now()); ok {
					edgeBackoff = min(max(interval, after), 60*time.Second)
				} else {
					edgeBackoff = min(max(edgeBackoff*2, interval*2, 5*time.Second), 60*time.Second)
				}
				if err := cancellableSleep(ctx, max(0, min(edgeBackoff, time.Until(deadline))), o.Cancelled); err != nil {
					return nil, err
				}
				continue
			}
			if r.Status >= 400 {
				return nil, fmt.Errorf("token endpoint answered HTTP %d %s", r.Status, http.StatusText(r.Status))
			}
			return nil, o.OnNonJSONError(r)
		}
		edgeBackoff, unavailable = 0, 0
		switch textOf(payload.value("error")) {
		case "authorization_pending":
			if err := cancellableSleep(ctx, min(interval, max(0, time.Until(deadline))), o.Cancelled); err != nil {
				return nil, err
			}
			continue
		case "slow_down":
			interval = min(interval+time.Second, 30*time.Second)
			if err := cancellableSleep(ctx, interval, o.Cancelled); err != nil {
				return nil, err
			}
			continue
		}
		return nil, o.OnError(r, json.RawMessage(r.Body))
	}
	var cause error
	if unavailable != 0 {
		cause = fmt.Errorf("token endpoint answered HTTP %d", unavailable)
	}
	return nil, o.OnTimeout(cause)
}

func (s *Store) deviceReply(r reply, err error) (DeviceReply, error) {
	return DeviceReply{Status: r.status, Header: r.header, Body: r.body}, err
}

func (s *Store) RequestPortalDeviceCode(ctx context.Context, client *http.Client, portalBaseURL, clientID, scope string, timeout time.Duration) (json.RawMessage, error) {
	form := formOf("client_id", clientID)
	if scope != "" {
		form.set("scope", scope)
	}
	r, err := s.postForm(ctx, client, portalBaseURL+"/api/oauth/device/code", form, nil, timeout)
	if err != nil {
		return nil, err
	}
	if r.status >= 400 {
		return nil, fmt.Errorf("device code endpoint answered HTTP %d %s", r.status, http.StatusText(r.status))
	}
	data, err := r.object()
	if err != nil {
		return nil, err
	}
	missing := []string{}
	for _, f := range []string{"device_code", "user_code", "verification_uri", "verification_uri_complete", "expires_in", "interval"} {
		if !data.has(f) {
			missing = append(missing, f)
		}
	}
	if len(missing) > 0 {
		return nil, fmt.Errorf("Device code response missing fields: %s", strings.Join(missing, ", "))
	}
	return json.RawMessage(r.body), nil
}

func portalDeviceTimeoutMessage(portalBaseURL string) string {
	portal := portalBaseURL
	if portal == "" {
		portal = DefaultPortalURL
	}
	portal = strings.TrimRight(portal, "/")
	return "Timed out waiting for device authorization.\n" +
		"  Portal sign-in is required before the device code can be approved.\n" +
		"  If the browser showed a CAPTCHA / 'You did not pass CAPTCHA' error,\n" +
		"  finish signing in at the Portal in a normal browser tab, then retry:\n" +
		"    icebath portal\n" +
		"  Portal login: " + portal + "/login"
}

func (s *Store) PollPortalToken(ctx context.Context, client *http.Client, portalBaseURL, clientID, deviceCode string, expiresIn, interval, timeout time.Duration, cancelled func() bool) (json.RawMessage, error) {
	return PollDeviceToken(ctx, DevicePollOptions{
		Post: func(ctx context.Context) (DeviceReply, error) {
			return s.deviceReply(s.postForm(ctx, client, portalBaseURL+"/api/oauth/token",
				formOf("grant_type", DeviceCodeGrantType, "client_id", clientID, "device_code", deviceCode), nil, timeout))
		},
		ExpiresIn: expiresIn,
		Interval:  max(time.Second, min(interval, DevicePollIntervalCap)),
		Validate: func(payload json.RawMessage) error {
			d, err := parseObject(payload)
			if err != nil || !d.has("access_token") {
				return errors.New("Token response did not include access_token")
			}
			return nil
		},
		OnError: func(_ DeviceReply, payload json.RawMessage) error {
			d, _ := parseObject(payload)
			description := textOf(d.value("error_description"))
			if description == "" {
				description = "Unknown authentication error"
			}
			return DeviceFlowError(textOf(d.value("error")), description, "")
		},
		OnNonJSONError: func(DeviceReply) error {
			return errors.New("Token endpoint returned a non-JSON error response")
		},
		OnTimeout: func(cause error) error {
			return &DeviceCodeExpiredError{Message: portalDeviceTimeoutMessage(portalBaseURL), Cause: cause}
		},
		Cancelled: cancelled,
	})
}

func (s *Store) PrintDeviceCodeInstructions(ctx context.Context, c Console, verificationURL, userCode string, open bool, failureDash string, swallowOpenErrors bool) error {
	if failureDash == "" {
		failureDash = "--"
	}
	c.println("")
	c.println("To continue:")
	c.printf("  1. Open: %s\n", verificationURL)
	c.printf("  2. If prompted, enter code: %s\n", userCode)
	if !open {
		return nil
	}
	opened, err := s.tryOpenBrowser(ctx, verificationURL)
	if err != nil && !swallowOpenErrors {
		return err
	}
	if opened && err == nil {
		c.println("  (Opened browser for verification)")
	} else {
		c.printf("  Could not open browser automatically %s use the URL above.\n", failureDash)
	}
	return nil
}

func PromptYesNo(c Console, prompt string, defaultYes bool) bool {
	c.printf("%s", prompt)
	answer := ""
	read := false
	if c.In != nil {
		line, err := bufio.NewReader(c.In).ReadString('\n')
		if err == nil || line != "" {
			answer, read = strings.ToLower(strings.TrimSpace(line)), true
		}
	}
	if !read {
		return defaultYes
	}
	if defaultYes {
		return answer == "" || answer == "y" || answer == "yes"
	}
	return answer == "y" || answer == "yes"
}

func (s *Store) PrintLoginSuccess(c Console, provider, configPath string, showAuthState bool) {
	c.println("")
	c.println("Login successful!")
	if showAuthState {
		shown, err := home.Display(s.home)
		if err != nil {
			shown = s.home
		}
		c.printf("  Auth state: %s/auth.json\n", shown)
	}
	c.printf("  Config updated: %s (model.provider=%s)\n", configPath, provider)
}

type ExistingOffer struct {
	Provider       string
	DisplayName    string
	DefaultBaseURL string
	ExpiredNotice  string
	Resolve        func(ctx context.Context) (apiKey Secret, baseURL string, err error)
	IsExpiring     func(token string, skew time.Duration) bool
	UpdateConfig   func(ctx context.Context, provider, baseURL string) (configPath string, err error)
}

func (s *Store) OfferExistingCredentials(ctx context.Context, c Console, o ExistingOffer) (bool, error) {
	if o.Resolve == nil || o.IsExpiring == nil || o.UpdateConfig == nil {
		return false, errors.New("auth: offering stored credentials needs resolve, expiry and config update functions")
	}
	key, baseURL, err := o.Resolve(ctx)
	if err != nil {
		var ae *Error
		if errors.As(err, &ae) {
			return false, nil
		}
		return false, err
	}
	if !key.Empty() && !o.IsExpiring(key.Reveal(), 60*time.Second) {
		c.printf("Existing %s credentials found in icebath auth store.\n", o.DisplayName)
		if PromptYesNo(c, "Use existing credentials? [Y/n]: ", true) {
			if baseURL == "" {
				baseURL = o.DefaultBaseURL
			}
			configPath, err := o.UpdateConfig(ctx, o.Provider, baseURL)
			if err != nil {
				return false, err
			}
			s.PrintLoginSuccess(c, o.Provider, configPath, false)
			return true, nil
		}
	} else if o.ExpiredNotice != "" {
		c.println(o.ExpiredNotice)
	}
	return false, nil
}
