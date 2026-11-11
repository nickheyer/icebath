package gateway

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/internal/pyint"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
)

const (
	ScaleToZeroEnv                  = "ICEBATH_SCALE_TO_ZERO"
	FlyAppNameEnv                   = "FLY_APP_NAME"
	FlyMachineIDEnv                 = "FLY_MACHINE_ID"
	FlyAPISocket                    = "/.fly/api"
	SleepURLEnv                     = "GATEWAY_RELAY_SLEEP_URL"
	DefaultIdleTimeoutMinutes       = 2
	BrokeredSuspendTimeoutS         = 40.0
	FlyFreezeGraceS                 = 15.0
	FlyFreezeGraceTickS             = 0.25
	scaleToZeroSuspendSelfTimeoutS  = 10.0
	scaleToZeroResponseReadLimit    = 65536
	scaleToZeroRejectedBodyMaxBytes = 500
)

// Dashboard client liveness marker relative to the process home
var DashboardClientHeartbeatRel = filepath.Join("state", "dashboard_clients.heartbeat")

var (
	scaleToZeroTruthy         = map[string]bool{"1": true, "true": true, "yes": true, "on": true}
	malformedSleepURLLogged   bool
	malformedSleepURLLoggedMu sync.Mutex
)

func scaleToZeroLog() *slog.Logger { return slog.Default().With("logger", "gateway.scale_to_zero") }

// Stripped value of key from env, the process environment when env is nil
func scaleToZeroEnvStr(env map[string]string, key string) string {
	if env == nil {
		return pystr.Strip(os.Getenv(key))
	}
	return pystr.Strip(env[key])
}

// Whether the Labs toggle stamp is set to a truthy value
func ScaleToZeroEnabled(env map[string]string) bool {
	return scaleToZeroTruthy[strings.ToLower(scaleToZeroEnvStr(env, ScaleToZeroEnv))]
}

// Idle timeout minutes coerced to seconds, a non numeric or non positive value giving the default
func ParseIdleTimeoutSeconds(cfgValue any, defaultMinutes int) float64 {
	minutes, err := pyint.Float(cfgValue)
	if err != nil {
		minutes = 0.0
	}
	if minutes <= 0 {
		return float64(defaultMinutes) * 60.0
	}
	return minutes * 60.0
}

// True when the only platform named is relay or none is named at all
func MessagingIsRelayOnlyOrAbsent(platforms []string) bool {
	for _, p := range platforms {
		if name := strings.ToLower(pystr.Strip(p)); name != "relay" {
			return false
		}
	}
	return true
}

// Arms the idle watcher only with the flag on, relay only messaging and a registered wake URL
func ShouldArm(enabled, relayOnlyOrAbsent bool, wakeURL *string) bool {
	return enabled && relayOnlyOrAbsent && wakeURL != nil && *wakeURL != ""
}

// No active work, no live background work and no inbound within the idle window
func IsIdle(activeWorkCount int, secondsSinceLastInbound, idleTimeoutSeconds float64, hasLiveBackgroundWork bool) bool {
	return activeWorkCount <= 0 && !hasLiveBackgroundWork && secondsSinceLastInbound >= idleTimeoutSeconds
}

// Dashboard client liveness marker under the given home, the process home when empty
func DashboardClientHeartbeatPath(hermesHome string) string {
	if hermesHome == "" {
		hermesHome = home.Process()
	}
	return filepath.Join(hermesHome, DashboardClientHeartbeatRel)
}

// Stamps the dashboard client marker now, false when the touch fails
func TouchDashboardClientHeartbeat(path string) bool {
	if path == "" {
		path = DashboardClientHeartbeatPath("")
	}
	err := func() error {
		if err := os.MkdirAll(filepath.Dir(path), 0o777); err != nil {
			return err
		}
		f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o666)
		if err != nil {
			return err
		}
		if err := f.Close(); err != nil {
			return err
		}
		now := time.Now()
		return os.Chtimes(path, now, now)
	}()
	if err != nil {
		scaleToZeroLog().Debug("scale-to-zero: dashboard heartbeat touch failed", "error", err)
		return false
	}
	return true
}

// Epoch seconds a dashboard client was last seen clamped to now, nil when never, now when unreadable
func DashboardClientLastSeen(path string, now *float64) *float64 {
	current := float64(time.Now().UnixNano()) / 1e9
	if now != nil {
		current = *now
	}
	if path == "" {
		path = DashboardClientHeartbeatPath("")
	}
	info, err := os.Stat(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return &current
	}
	mtime := math.Min(float64(info.ModTime().UnixNano())/1e9, current)
	return &mtime
}

// True when Fly machine identity is present and the local Machines API socket exists
func SelfSuspendAvailable(env map[string]string) bool {
	if scaleToZeroEnvStr(env, FlyAppNameEnv) == "" || scaleToZeroEnvStr(env, FlyMachineIDEnv) == "" {
		return false
	}
	_, err := os.Stat(FlyAPISocket)
	return err == nil
}

// Absolute https NAS sleep endpoint, nil when unset or malformed with one warning per process
func BrokeredSleepURL(env map[string]string) *string {
	raw := os.Getenv(SleepURLEnv)
	if env != nil {
		raw = env[SleepURLEnv]
	}
	value := pystr.Strip(raw)
	if value == "" {
		return nil
	}
	parsed, err := url.Parse(value)
	if err != nil || parsed.Scheme != "https" || parsed.Host == "" {
		malformedSleepURLLoggedMu.Lock()
		first := !malformedSleepURLLogged
		malformedSleepURLLogged = true
		malformedSleepURLLoggedMu.Unlock()
		if first {
			scaleToZeroLog().Warn(fmt.Sprintf("scale-to-zero: ignoring malformed %s (want an absolute https URL)", SleepURLEnv))
		}
		return nil
	}
	return &value
}

// Whether an in guest or brokered suspend lever exists
func SuspendAvailable(env map[string]string) bool {
	return SelfSuspendAvailable(env) || BrokeredSleepURL(env) != nil
}

// POSTs the NAS sleep URL, true only on a 2xx and never raising
func RequestBrokeredSuspend(ctx context.Context, rawURL string, timeout float64, client *http.Client) bool {
	if client == nil {
		client = &http.Client{}
	}
	reqCtx, cancel := context.WithTimeout(ctx, time.Duration(timeout*float64(time.Second)))
	defer cancel()
	req, err := http.NewRequestWithContext(reqCtx, http.MethodPost, rawURL, strings.NewReader(""))
	if err != nil {
		scaleToZeroLog().Warn(fmt.Sprintf("scale-to-zero: brokered suspend request failed: %v", err))
		return false
	}
	resp, err := client.Do(req)
	if err != nil {
		scaleToZeroLog().Warn(fmt.Sprintf("scale-to-zero: brokered suspend request failed: %v", err))
		return false
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	_ = resp.Body.Close()
	status := resp.StatusCode
	if status >= 400 {
		reason := strings.TrimPrefix(resp.Status, strconv.Itoa(status)+" ")
		scaleToZeroLog().Warn(fmt.Sprintf("scale-to-zero: brokered suspend rejected: %d %s", status, reason))
		return false
	}
	ok := status >= 200 && status < 300
	if ok {
		scaleToZeroLog().Info(fmt.Sprintf("scale-to-zero: machine suspend accepted by NAS (%d)", status))
	} else {
		scaleToZeroLog().Warn(fmt.Sprintf("scale-to-zero: brokered suspend returned %d", status))
	}
	return ok
}

// POSTs the local flaps suspend for this machine over its unix socket, true when flaps answered 2xx
func SuspendSelf(env map[string]string, socketPath string, timeout float64) bool {
	app, machineID := scaleToZeroEnvStr(env, FlyAppNameEnv), scaleToZeroEnvStr(env, FlyMachineIDEnv)
	if app == "" || machineID == "" {
		scaleToZeroLog().Warn("scale-to-zero: suspend_self called without Fly machine identity")
		return false
	}
	if socketPath == "" {
		socketPath = FlyAPISocket
	}
	request := "POST /v1/apps/" + app + "/machines/" + machineID + "/suspend HTTP/1.1\r\n" +
		"Host: flaps\r\nContent-Length: 0\r\nConnection: close\r\n\r\n"
	response, err := func() ([]byte, error) {
		deadline := time.Duration(timeout * float64(time.Second))
		conn, err := net.DialTimeout("unix", socketPath, deadline)
		if err != nil {
			return nil, err
		}
		defer conn.Close()
		if err := conn.SetDeadline(time.Now().Add(deadline)); err != nil {
			return nil, err
		}
		if _, err := conn.Write([]byte(request)); err != nil {
			return nil, err
		}
		var out []byte
		chunk := make([]byte, 4096)
		for len(out) < scaleToZeroResponseReadLimit {
			_ = conn.SetReadDeadline(time.Now().Add(deadline))
			n, err := conn.Read(chunk)
			out = append(out, chunk[:n]...)
			if err == io.EOF || (err == nil && n == 0) {
				break
			}
			if err != nil {
				return nil, err
			}
		}
		return out, nil
	}()
	if err != nil {
		scaleToZeroLog().Warn(fmt.Sprintf("scale-to-zero: flaps suspend request failed: %v", err))
		return false
	}
	statusLine, _, _ := strings.Cut(string(response), "\r\n")
	statusLine = pystr.DecodeASCIIReplace([]byte(statusLine))
	parts := pystr.SplitWhitespace(statusLine, -1)
	ok := false
	if len(parts) >= 2 && pystr.StrIsDigit(parts[1]) {
		if code, err := strconv.Atoi(parts[1]); err == nil {
			ok = code >= 200 && code < 300
		}
	}
	if ok {
		scaleToZeroLog().Info(fmt.Sprintf("scale-to-zero: machine suspend accepted by flaps (%s)", statusLine))
		return true
	}
	body := response
	if _, after, found := strings.Cut(string(response), "\r\n\r\n"); found {
		body = []byte(after)
	}
	if len(body) > scaleToZeroRejectedBodyMaxBytes {
		body = body[:scaleToZeroRejectedBodyMaxBytes]
	}
	dumped, _ := pyjson.Dumps(pystr.DecodeUTF8Replace(body))
	scaleToZeroLog().Warn(fmt.Sprintf("scale-to-zero: flaps suspend rejected: %s %s", statusLine, pystr.Slice(dumped, 0, scaleToZeroRejectedBodyMaxBytes)))
	return false
}
