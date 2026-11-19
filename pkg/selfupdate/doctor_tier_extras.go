//go:build !icebath_core && !icebath_noextras

package selfupdate

import (
	"context"
	"time"

	"github.com/nickheyer/icebath/pkg/browser"
	"github.com/nickheyer/icebath/pkg/codeexec"
	"github.com/nickheyer/icebath/pkg/cron"
	"github.com/nickheyer/icebath/pkg/internal/pysounddevice"
	"github.com/nickheyer/icebath/pkg/media"
	"github.com/nickheyer/icebath/pkg/tools"
)

// Binds the doctor rows of voice, wake word, execute_code, the browser tools and the cron store, and the live browser probe
func init() {
	setDoctorPackageProbe("PortAudio (voice mode audio)", func(context.Context) bool { return pysounddevice.Load() == nil })
	setDoctorPackageProbe("Porcupine (wake word)", func(ctx context.Context) bool { return media.LoadPorcupineLibrary(ctx) == nil })
	setDoctorPackageProbe("openWakeWord (wake word)", func(ctx context.Context) bool { return media.LoadOpenWakeWordLibrary(ctx) == nil })
	setDoctorPackageProbe("sherpa-onnx (wake word)", func(context.Context) bool { return media.SherpaOnnxAvailable() })
	setDoctorPackageProbe("Python on PATH (execute_code)", func(ctx context.Context) bool {
		_, err := codeexec.RuntimePython(ctx)
		return err == nil
	})
	doctorUsingLightpandaEngine = browser.UsingLightpandaEngine
	doctorLightpandaStatus = browser.LightpandaEngineStatus
	doctorFindLightpandaBinary = browser.FindLightpandaBinary
	doctorChromiumExecutable = func(env *tools.Env) string { return browser.ChromiumExecutable(env, true) }
	doctorChromiumMissingHint = browser.ChromiumMissingHint
	doctorLightpandaInstallHint = browser.LightpandaInstallHint
	doctorCronProbeReport = cron.ProbeReport
	doctorCronFreeBytes = cron.FreeBytes
	doctorCronLowFreeBytes = cron.LowFreeBytes
	doctorLiveBrowser = func(e *doctorLiveEnv) { bindDoctorChromiumProbe(e) }
}

// Chromium launcher of the live browser probe, its seams replaced by tests
type doctorChromiumProbe struct {
	env            *doctorLiveEnv
	launchChromium func(key string, opts browser.LaunchOptions) (*browser.LaunchedBrowser, string)
	stopBrowser    func(key string)
}

// Probe bound to the real Chromium launcher and set as the browser seams of the env
func bindDoctorChromiumProbe(e *doctorLiveEnv) *doctorChromiumProbe {
	p := &doctorChromiumProbe{env: e}
	p.launchChromium = func(key string, opts browser.LaunchOptions) (*browser.LaunchedBrowser, string) {
		return browser.LaunchChromium(e.ctx, e.toolsEnv(), key, opts)
	}
	p.stopBrowser = func(key string) { browser.StopBrowser(e.ctx, key) }
	e.browserAvailable = p.available
	e.launchBrowserProbe = p.launch
	return p
}

// Reports whether a Chromium exists for the local browser launcher
func (p *doctorChromiumProbe) available() bool {
	return browser.ChromiumExecutable(p.env.toolsEnv(), true) != ""
}

// Launches Chromium through the browser launcher, waits for its CDP endpoint and always stops it
func (p *doctorChromiumProbe) launch(timeout float64) (bool, string) {
	key := "doctor-live-probe"
	limit := time.Duration(timeout * float64(time.Second))
	launched, launchErr := func() (*browser.LaunchedBrowser, string) {
		defer p.stopBrowser(key)
		return p.launchChromium(key, browser.LaunchOptions{Ephemeral: true, Timeout: &limit})
	}()
	if launched == nil {
		if launchErr == "" {
			launchErr = "launch failed"
		}
		return false, launchErr
	}
	return true, "launched + CDP ready + stopped"
}
