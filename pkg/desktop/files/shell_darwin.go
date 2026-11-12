//go:build darwin

package files

import (
	"errors"
	"sync"
	"unsafe"

	"github.com/ebitengine/purego"
	"github.com/ebitengine/purego/objc"
)

var loadAppKit = sync.OnceValue(func() error {
	for _, framework := range []string{
		"/System/Library/Frameworks/Foundation.framework/Foundation",
		"/System/Library/Frameworks/AppKit.framework/AppKit",
	} {
		if _, err := purego.Dlopen(framework, purego.RTLD_LAZY|purego.RTLD_GLOBAL); err != nil {
			return err
		}
	}
	return nil
})

func sel(name string) objc.SEL { return objc.RegisterName(name) }

func nsString(s string) objc.ID {
	b := append([]byte(s), 0)
	return objc.ID(objc.GetClass("NSString")).Send(sel("stringWithUTF8String:"), unsafe.Pointer(&b[0]))
}

func fileURL(p string) objc.ID {
	return objc.ID(objc.GetClass("NSURL")).Send(sel("fileURLWithPath:"), nsString(p))
}

func goString(ns objc.ID) string {
	if ns == 0 {
		return ""
	}
	ptr := objc.Send[*byte](ns, sel("UTF8String"))
	if ptr == nil {
		return ""
	}
	var out []byte
	for p := unsafe.Pointer(ptr); *(*byte)(p) != 0; p = unsafe.Add(p, 1) {
		out = append(out, *(*byte)(p))
	}
	return string(out)
}

// Selects the item in a Finder window through NSWorkspace
func ShowItemInFolder(fullPath string) {
	if loadAppKit() != nil {
		return
	}
	pool := objc.ID(objc.GetClass("NSAutoreleasePool")).Send(sel("new"))
	defer pool.Send(sel("drain"))
	urls := objc.ID(objc.GetClass("NSArray")).Send(sel("arrayWithObject:"), fileURL(fullPath))
	workspace := objc.ID(objc.GetClass("NSWorkspace")).Send(sel("sharedWorkspace"))
	workspace.Send(sel("activateFileViewerSelectingURLs:"), urls)
}

// Opens the path in its default application through NSWorkspace, the error text on failure and empty on success
func OpenPath(fullPath string) string {
	if err := loadAppKit(); err != nil {
		return err.Error()
	}
	pool := objc.ID(objc.GetClass("NSAutoreleasePool")).Send(sel("new"))
	defer pool.Send(sel("drain"))
	workspace := objc.ID(objc.GetClass("NSWorkspace")).Send(sel("sharedWorkspace"))
	if !objc.Send[bool](workspace, sel("openURL:"), fileURL(fullPath)) {
		return "Failed to open path"
	}
	return ""
}

// Moves the item to the Trash through NSFileManager trashItemAtURL
func TrashItem(fullPath string) error {
	if err := loadAppKit(); err != nil {
		return err
	}
	pool := objc.ID(objc.GetClass("NSAutoreleasePool")).Send(sel("new"))
	defer pool.Send(sel("drain"))
	manager := objc.ID(objc.GetClass("NSFileManager")).Send(sel("defaultManager"))
	var nsErr objc.ID
	ok := objc.Send[bool](manager, sel("trashItemAtURL:resultingItemURL:error:"), fileURL(resolvePath(fullPath)), unsafe.Pointer(nil), unsafe.Pointer(&nsErr))
	if ok {
		return nil
	}
	if nsErr != 0 {
		return errors.New(goString(nsErr.Send(sel("localizedDescription"))))
	}
	return errors.New("Failed to move item to trash")
}
