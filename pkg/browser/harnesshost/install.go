package harnesshost

import "github.com/nickheyer/icebath/pkg/internal/pyhost"

// True for the harness's own helper, a same named function from the agent's agent_helpers.py is the agent's choice
func isHarnessHelper(f *frame, fn uintptr) (bool, error) {
	if !f.g.Callable(fn) {
		return false, nil
	}
	module, err := f.attr(fn, "__module__")
	if err != nil {
		if missing, err2 := f.raised(err, "AttributeError"); err2 != nil || !missing {
			if err2 != nil {
				return false, err2
			}
			return false, err
		}
		return false, nil
	}
	same, err := f.op("eq", pyhost.Borrowed(module), harnessModule)
	if err != nil {
		return false, err
	}
	return f.truthy(same)
}

// True only when the live harness daemon says it drives a Browser Use cloud browser, asked per call
func harnessDaemonIsCloud(f *frame) (bool, error) {
	kind, err := f.member("browser_harness.admin", "daemon_browser_kind")
	if err != nil {
		return false, err
	}
	answer, err := f.call(kind, nil, nil)
	if err != nil {
		return false, err
	}
	same, err := f.op("eq", pyhost.Borrowed(answer), "cloud")
	if err != nil {
		return false, err
	}
	return f.truthy(same)
}

// Activation decision for a mode, the daemon asked under if-cloud and fixed answers otherwise
func activationCheck(mode string) func(*frame) (bool, error) {
	switch mode {
	case ActivateIfCloud:
		return harnessDaemonIsCloud
	case ActivateAlways:
		return func(*frame) (bool, error) { return true, nil }
	}
	return func(*frame) (bool, error) { return false, nil }
}

// Rebinds capture_screenshot and new_tab on the helpers module before browser_harness.run star imports them
func install(f *frame, helpers uintptr, mode string) error {
	check := activationCheck(mode)
	capture, err := f.attr(helpers, "capture_screenshot")
	if err != nil {
		return err
	}
	if own, err := isHarnessHelper(f, capture); err != nil {
		return err
	} else if own {
		cdp, err := f.attr(helpers, "cdp")
		if err != nil {
			return err
		}
		currentTab, err := f.attr(helpers, "current_tab")
		if err != nil {
			return err
		}
		replacement, err := f.keep(makeCaptureScreenshot(f.g, f.g.NewRef(cdp), f.g.NewRef(currentTab), check))
		if err != nil {
			return err
		}
		if err := f.g.SetAttr(helpers, "capture_screenshot", pyhost.Borrowed(replacement)); err != nil {
			return err
		}
	}
	if mode == ActivateNever {
		return nil
	}
	newTab, err := f.attr(helpers, "new_tab")
	if err != nil {
		return err
	}
	if own, err := isHarnessHelper(f, newTab); err != nil || !own {
		return err
	}
	cdp, err := f.attr(helpers, "cdp")
	if err != nil {
		return err
	}
	replacement, err := f.keep(makeNewTab(f.g, f.g.NewRef(newTab), f.g.NewRef(cdp), check))
	if err != nil {
		return err
	}
	return f.g.SetAttr(helpers, "new_tab", pyhost.Borrowed(replacement))
}
