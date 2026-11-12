package harnesshost

import (
	"fmt"
	"strconv"

	"github.com/nickheyer/icebath/pkg/internal/pyhost"
)

// A png that has not arrived by then will not, a hidden tab answers jpeg within a few seconds
const (
	pngDeadline   = 10.0
	jpegDeadline  = 60.0
	harnessModule = "browser_harness.helpers"
	loggerName    = "icebath_browser_use_shot"
)

// Which browser the tabs may be activated on, decided by browser_exec from the route it resolved
const (
	ActivateNever   = "never"
	ActivateAlways  = "always"
	ActivateIfCloud = "if-cloud"
)

const captureDoc = "capture_screenshot(path=None, full=False, max_dim=None)\n--\n\n" +
	"Save a PNG of the current viewport. Set max_dim=1800 on a 2x display to keep the file under\n" +
	"the 2000px-per-side limit some image-aware LLMs enforce."

const newTabDoc = "new_tab_in_front(url='about:blank')\n--\n\n" +
	"Open (or reuse a blank) tab, navigate to url, and bring it to the front on a cloud browser."

// Formats a deadline the way Python's g format spec prints it
func formatG(seconds float64) string { return strconv.FormatFloat(seconds, 'g', -1, 64) }

// The harness's per user temp dir shot.png, the system temp dir when the harness has no _ipc._TMP
func defaultShotPath(f *frame) (uintptr, error) {
	path, err := func() (uintptr, error) {
		tmp, err := f.member("browser_harness._ipc", "_TMP")
		if err != nil {
			return 0, err
		}
		joined, err := f.op("truediv", pyhost.Borrowed(tmp), "shot.png")
		if err != nil {
			return 0, err
		}
		str, err := f.member("builtins", "str")
		if err != nil {
			return 0, err
		}
		return f.call(str, []any{pyhost.Borrowed(joined)}, nil)
	}()
	if err == nil {
		return path, nil
	}
	if fallback, err2 := f.raised(err, "ImportError", "AttributeError"); err2 != nil || !fallback {
		if err2 != nil {
			return 0, err2
		}
		return 0, err
	}
	gettempdir, err := f.member("tempfile", "gettempdir")
	if err != nil {
		return 0, err
	}
	dir, err := f.call(gettempdir, nil, nil)
	if err != nil {
		return 0, err
	}
	join, err := f.member("os.path", "join")
	if err != nil {
		return 0, err
	}
	return f.call(join, []any{pyhost.Borrowed(dir), "shot.png"}, nil)
}

// Activates the attached tab when the page reports it hidden, a CDP error or slow round trip capturing anyway
func bringToFrontIfHidden(f *frame, cdp, currentTab uintptr) error {
	err := func() error {
		state, err := f.call(cdp, []any{"Runtime.evaluate"}, map[string]any{"expression": "document.visibilityState", "returnByValue": true})
		if err != nil {
			return err
		}
		result, err := f.method(state, "get", "result")
		if err != nil {
			return err
		}
		if ok, err := f.truthy(result); err != nil {
			return err
		} else if !ok {
			dict, err := f.member("builtins", "dict")
			if err != nil {
				return err
			}
			if result, err = f.call(dict, nil, nil); err != nil {
				return err
			}
		}
		value, err := f.method(result, "get", "value")
		if err != nil {
			return err
		}
		differs, err := f.op("ne", pyhost.Borrowed(value), "hidden")
		if err != nil {
			return err
		}
		if visible, err := f.truthy(differs); err != nil || visible {
			return err
		}
		tab, err := f.call(currentTab, nil, nil)
		if err != nil {
			return err
		}
		target, err := f.method(tab, "__getitem__", "targetId")
		if err != nil {
			return err
		}
		_, err = f.call(cdp, []any{"Target.activateTarget"}, map[string]any{"targetId": pyhost.Borrowed(target)})
		return err
	}()
	if err == nil {
		return nil
	}
	if swallowed, err2 := f.raised(err, "RuntimeError", "OSError", "KeyError"); err2 != nil {
		return err2
	} else if swallowed {
		return nil
	}
	return err
}

// Writes data encoded as format to path, re-encoding a jpeg as png when the path says png
func writeShot(f *frame, data, path uintptr, format string) error {
	if format == "jpeg" {
		lower, err := f.method(path, "lower")
		if err != nil {
			return err
		}
		suffix, err := f.method(lower, "endswith", ".png")
		if err != nil {
			return err
		}
		if png, err := f.truthy(suffix); err != nil {
			return err
		} else if png {
			open, err := f.member("PIL.Image", "open")
			if err != nil {
				return err
			}
			buffer, err := f.member("io", "BytesIO")
			if err != nil {
				return err
			}
			stream, err := f.call(buffer, []any{pyhost.Borrowed(data)}, nil)
			if err != nil {
				return err
			}
			image, err := f.call(open, []any{pyhost.Borrowed(stream)}, nil)
			if err != nil {
				return err
			}
			save, err := f.attr(image, "save")
			if err != nil {
				return err
			}
			_, err = f.call(save, []any{pyhost.Borrowed(path)}, map[string]any{"format": "PNG"})
			return err
		}
	}
	open, err := f.member("builtins", "open")
	if err != nil {
		return err
	}
	file, err := f.call(open, []any{pyhost.Borrowed(path), "wb"}, nil)
	if err != nil {
		return err
	}
	_, werr := f.method(file, "write", pyhost.Borrowed(data))
	_, cerr := f.method(file, "close")
	if werr != nil {
		return werr
	}
	return cerr
}

// Shrinks the saved image to max_dim on its longer side when it is larger
func fitMaxDim(f *frame, path, maxDim uintptr) error {
	open, err := f.member("PIL.Image", "open")
	if err != nil {
		return err
	}
	image, err := f.call(open, []any{pyhost.Borrowed(path)}, nil)
	if err != nil {
		return err
	}
	size, err := f.attr(image, "size")
	if err != nil {
		return err
	}
	largest, err := f.member("builtins", "max")
	if err != nil {
		return err
	}
	longest, err := f.call(largest, []any{pyhost.Borrowed(size)}, nil)
	if err != nil {
		return err
	}
	larger, err := f.op("gt", pyhost.Borrowed(longest), pyhost.Borrowed(maxDim))
	if err != nil {
		return err
	}
	if over, err := f.truthy(larger); err != nil || !over {
		return err
	}
	tuple, err := f.member("builtins", "tuple")
	if err != nil {
		return err
	}
	box, err := f.call(tuple, []any{[]any{pyhost.Borrowed(maxDim), pyhost.Borrowed(maxDim)}}, nil)
	if err != nil {
		return err
	}
	if _, err := f.method(image, "thumbnail", pyhost.Borrowed(box)); err != nil {
		return err
	}
	_, err = f.method(image, "save", pyhost.Borrowed(path))
	return err
}

// One Page.captureScreenshot round trip with its response deadline and format options
func captureOnce(f *frame, cdp uintptr, deadline float64, format string, full any) (uintptr, error) {
	kwargs := map[string]any{"_response_timeout": deadline, "format": format}
	if format == "jpeg" {
		kwargs["quality"] = 90
	}
	kwargs["captureBeyondViewport"] = full
	return f.call(cdp, []any{"Page.captureScreenshot"}, kwargs)
}

// capture_screenshot bound over the harness's cdp and current_tab, a bounded png attempt then a jpeg fallback
func makeCaptureScreenshot(g *pyhost.GIL, cdp, currentTab *pyhost.Ref, activateHidden func(*frame) (bool, error)) (uintptr, error) {
	return g.NewDocFunc("capture_screenshot", captureDoc, func(g *pyhost.GIL, args, kwargs uintptr) (uintptr, error) {
		f := &frame{g: g}
		defer f.release()
		bound, err := f.bind("make_capture_screenshot.<locals>.capture_screenshot", []string{"path", "full", "max_dim"}, []any{nil, false, nil}, args, kwargs)
		if err != nil {
			return 0, err
		}
		given, err := f.object(bound[0])
		if err != nil {
			return 0, err
		}
		chosen := given
		if ok, err := f.truthy(given); err != nil {
			return 0, err
		} else if !ok {
			if chosen, err = defaultShotPath(f); err != nil {
				return 0, err
			}
		}
		fspath, err := f.member("os", "fspath")
		if err != nil {
			return 0, err
		}
		path, err := f.call(fspath, []any{pyhost.Borrowed(chosen)}, nil)
		if err != nil {
			return 0, err
		}
		if hidden, err := activateHidden(f); err != nil {
			return 0, err
		} else if hidden {
			if err := bringToFrontIfHidden(f, cdp.Ptr(), currentTab.Ptr()); err != nil {
				return 0, err
			}
		}
		format := "png"
		reply, err := captureOnce(f, cdp.Ptr(), pngDeadline, "png", bound[1])
		if err != nil {
			if timedOut, err2 := f.raised(err, "TimeoutError"); err2 != nil || !timedOut {
				if err2 != nil {
					return 0, err2
				}
				return 0, err
			}
			format = "jpeg"
			if reply, err = captureOnce(f, cdp.Ptr(), jpegDeadline, "jpeg", bound[1]); err != nil {
				if timedOut, err2 := f.raised(err, "TimeoutError"); err2 != nil || !timedOut {
					if err2 != nil {
						return 0, err2
					}
					return 0, err
				}
				message := fmt.Sprintf("Page.captureScreenshot timed out (png after %ss, jpeg after %ss)", formatG(pngDeadline), formatG(jpegDeadline))
				runtimeError, err2 := f.member("builtins", "RuntimeError")
				if err2 != nil {
					return 0, err2
				}
				exc, err2 := f.call(runtimeError, []any{message}, nil)
				if err2 != nil {
					return 0, err2
				}
				return 0, g.RaiseFrom(exc, err)
			}
		}
		encoded, err := f.method(reply, "__getitem__", "data")
		if err != nil {
			return 0, err
		}
		decode, err := f.member("base64", "b64decode")
		if err != nil {
			return 0, err
		}
		data, err := f.call(decode, []any{pyhost.Borrowed(encoded)}, nil)
		if err != nil {
			return 0, err
		}
		if err := writeShot(f, data, path, format); err != nil {
			return 0, err
		}
		maxDim, err := f.object(bound[2])
		if err != nil {
			return 0, err
		}
		if limit, err := f.truthy(maxDim); err != nil {
			return 0, err
		} else if limit {
			if err := fitMaxDim(f, path, maxDim); err != nil {
				return 0, err
			}
		}
		return g.ToPython(pyhost.Borrowed(path))
	})
}

// new_tab wrapped to bring the opened tab to the front when activate allows, a rejected activation only logged
func makeNewTab(g *pyhost.GIL, newTab, cdp *pyhost.Ref, activate func(*frame) (bool, error)) (uintptr, error) {
	return g.NewDocFunc("new_tab_in_front", newTabDoc, func(g *pyhost.GIL, args, kwargs uintptr) (uintptr, error) {
		f := &frame{g: g}
		defer f.release()
		bound, err := f.bind("make_new_tab.<locals>.new_tab_in_front", []string{"url"}, []any{"about:blank"}, args, kwargs)
		if err != nil {
			return 0, err
		}
		url, err := f.object(bound[0])
		if err != nil {
			return 0, err
		}
		tid, err := f.call(newTab.Ptr(), []any{pyhost.Borrowed(url)}, nil)
		if err != nil {
			return 0, err
		}
		front, err := f.truthy(tid)
		if err != nil {
			return 0, err
		}
		if front {
			if front, err = activate(f); err != nil {
				return 0, err
			}
		}
		if front {
			if _, err := f.call(cdp.Ptr(), []any{"Target.activateTarget"}, map[string]any{"targetId": pyhost.Borrowed(tid)}); err != nil {
				logged, err2 := f.raised(err, "RuntimeError", "OSError")
				if err2 != nil {
					return 0, err2
				}
				if !logged {
					return 0, err
				}
				exc, _ := g.ExceptionObject(err)
				getLogger, err2 := f.member("logging", "getLogger")
				if err2 != nil {
					return 0, err2
				}
				logger, err2 := f.call(getLogger, []any{loggerName}, nil)
				if err2 != nil {
					return 0, err2
				}
				if _, err2 := f.method(logger, "warning", "new_tab: Target.activateTarget failed, tab left in background: %s", pyhost.Borrowed(exc)); err2 != nil {
					return 0, err2
				}
			}
		}
		return g.ToPython(pyhost.Borrowed(tid))
	})
}
