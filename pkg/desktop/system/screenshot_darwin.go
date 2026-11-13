//go:build darwin

package system

import (
	"errors"
	"math"
	"sync"

	"github.com/ebitengine/purego"
	"github.com/ebitengine/purego/objc"
)

type cgRect struct{ X, Y, W, H float64 }

type captureFuncs struct {
	preflight        func() bool
	request          func() bool
	listCreateImage  func(bounds cgRect, option, windowID, imageOption uint32) uintptr
	colorSpaceRGB    func() uintptr
	bitmapContext    func(data uintptr, w, h, bits, rowBytes int, space uintptr, info uint32) uintptr
	drawImage        func(ctx uintptr, rect cgRect, image uintptr)
	contextImage     func(ctx uintptr) uintptr
	release          func(ref uintptr)
	hasScreenCapture bool
}

var captureLibs = sync.OnceValues(func() (*captureFuncs, error) {
	f, err := darwinLibs()
	if err != nil {
		return nil, err
	}
	c := &captureFuncs{}
	purego.RegisterLibFunc(&c.preflight, f.cg, "CGPreflightScreenCaptureAccess")
	purego.RegisterLibFunc(&c.request, f.cg, "CGRequestScreenCaptureAccess")
	purego.RegisterLibFunc(&c.colorSpaceRGB, f.cg, "CGColorSpaceCreateDeviceRGB")
	purego.RegisterLibFunc(&c.bitmapContext, f.cg, "CGBitmapContextCreate")
	purego.RegisterLibFunc(&c.drawImage, f.cg, "CGContextDrawImage")
	purego.RegisterLibFunc(&c.contextImage, f.cg, "CGBitmapContextCreateImage")
	purego.RegisterLibFunc(&c.release, f.cf, "CFRelease")
	if sym, err := purego.Dlsym(f.cg, "CGWindowListCreateImage"); err == nil && sym != 0 {
		purego.RegisterFunc(&c.listCreateImage, sym)
	}
	if _, err := purego.Dlopen("/System/Library/Frameworks/ScreenCaptureKit.framework/ScreenCaptureKit", purego.RTLD_LAZY|purego.RTLD_GLOBAL); err == nil {
		c.hasScreenCapture = objc.GetClass("SCScreenshotManager") != 0
	}
	return c, nil
})

// DarwinScreenshots captures windows through ScreenCaptureKit, CGWindowList before macOS 14
type DarwinScreenshots struct{}

// Screen capture natives of macOS
func NewScreenshotNative() *DarwinScreenshots { return &DarwinScreenshots{} }

func (DarwinScreenshots) HasScreenPermission() bool {
	c, err := captureLibs()
	return err == nil && c.preflight()
}

func (DarwinScreenshots) RequestScreenPermission() {
	if c, err := captureLibs(); err == nil {
		c.request()
	}
}

// Opens a URL with the default handler through NSWorkspace
func (DarwinScreenshots) OpenExternal(url string) error {
	if _, err := darwinLibs(); err != nil {
		return err
	}
	u := objc.ID(objc.GetClass("NSURL")).Send(objc.RegisterName("URLWithString:"), objcString(url))
	if u == 0 {
		return errors.New("Invalid URL")
	}
	workspace := objc.ID(objc.GetClass("NSWorkspace")).Send(objc.RegisterName("sharedWorkspace"))
	if !objc.Send[bool](workspace, objc.RegisterName("openURL:"), u) {
		return errors.New("Failed to open: " + url)
	}
	return nil
}

func pngOfCGImage(image uintptr) []byte {
	rep := objc.ID(objc.GetClass("NSBitmapImageRep")).Send(objc.RegisterName("alloc")).Send(objc.RegisterName("initWithCGImage:"), image)
	if rep == 0 {
		return nil
	}
	defer rep.Send(objc.RegisterName("release"))
	props := objc.ID(objc.GetClass("NSDictionary")).Send(objc.RegisterName("dictionary"))
	return nsDataBytes(rep.Send(objc.RegisterName("representationUsingType:properties:"), uint(nsBitmapImageFileTypePNG), props))
}

// PNG of one window at width by height, nil when the window is gone
func (DarwinScreenshots) CaptureWindow(windowID uint32, width, height int) ([]byte, error) {
	c, err := captureLibs()
	if err != nil {
		return nil, err
	}
	pool := objc.ID(objc.GetClass("NSAutoreleasePool")).Send(objc.RegisterName("new"))
	defer pool.Send(objc.RegisterName("drain"))
	if c.hasScreenCapture {
		return captureWithScreenCaptureKit(windowID, width, height)
	}
	if c.listCreateImage == nil {
		return nil, errors.New("window capture is unavailable")
	}
	nullRect := cgRect{X: math.Inf(1), Y: math.Inf(1)}
	image := c.listCreateImage(nullRect, 1<<3, windowID, 1<<0)
	if image == 0 {
		return nil, nil
	}
	defer c.release(image)
	space := c.colorSpaceRGB()
	defer c.release(space)
	ctx := c.bitmapContext(0, width, height, 8, 0, space, 1)
	if ctx == 0 {
		return nil, errors.New("bitmap context unavailable")
	}
	defer c.release(ctx)
	c.drawImage(ctx, cgRect{W: float64(width), H: float64(height)}, image)
	scaled := c.contextImage(ctx)
	if scaled == 0 {
		return nil, nil
	}
	defer c.release(scaled)
	return pngOfCGImage(scaled), nil
}

// Finds the shareable window and takes one ScreenCaptureKit screenshot of it at the requested size
func captureWithScreenCaptureKit(windowID uint32, width, height int) ([]byte, error) {
	type found struct {
		window objc.ID
		err    error
	}
	windows := make(chan found, 1)
	contentBlock := objc.NewBlock(func(_ objc.Block, content objc.ID, failure objc.ID) {
		if failure != 0 || content == 0 {
			windows <- found{err: errors.New("shareable content unavailable")}
			return
		}
		list := content.Send(objc.RegisterName("windows"))
		n := objc.Send[uint](list, objc.RegisterName("count"))
		for i := uint(0); i < n; i++ {
			w := list.Send(objc.RegisterName("objectAtIndex:"), i)
			if objc.Send[uint32](w, objc.RegisterName("windowID")) == windowID {
				windows <- found{window: w.Send(objc.RegisterName("retain"))}
				return
			}
		}
		windows <- found{}
	})
	defer contentBlock.Release()
	objc.ID(objc.GetClass("SCShareableContent")).Send(objc.RegisterName("getShareableContentExcludingDesktopWindows:onScreenWindowsOnly:completionHandler:"), true, false, contentBlock)
	got := <-windows
	if got.err != nil || got.window == 0 {
		return nil, got.err
	}
	defer got.window.Send(objc.RegisterName("release"))
	filter := objc.ID(objc.GetClass("SCContentFilter")).Send(objc.RegisterName("alloc")).Send(objc.RegisterName("initWithDesktopIndependentWindow:"), got.window)
	defer filter.Send(objc.RegisterName("release"))
	config := objc.ID(objc.GetClass("SCStreamConfiguration")).Send(objc.RegisterName("new"))
	defer config.Send(objc.RegisterName("release"))
	config.Send(objc.RegisterName("setWidth:"), uint(width))
	config.Send(objc.RegisterName("setHeight:"), uint(height))
	config.Send(objc.RegisterName("setShowsCursor:"), false)
	type shot struct {
		png []byte
		err error
	}
	shots := make(chan shot, 1)
	imageBlock := objc.NewBlock(func(_ objc.Block, image uintptr, failure objc.ID) {
		if failure != 0 || image == 0 {
			shots <- shot{err: errors.New("window capture failed")}
			return
		}
		shots <- shot{png: pngOfCGImage(image)}
	})
	defer imageBlock.Release()
	objc.ID(objc.GetClass("SCScreenshotManager")).Send(objc.RegisterName("captureImageWithFilter:configuration:completionHandler:"), filter, config, imageBlock)
	result := <-shots
	return result.png, result.err
}
