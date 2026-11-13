//go:build darwin

package system

import (
	"unsafe"

	"github.com/ebitengine/purego/objc"
)

const nsBitmapImageFileTypePNG = 4

func nsDataBytes(data objc.ID) []byte {
	if data == 0 {
		return nil
	}
	n := objc.Send[uint](data, objc.RegisterName("length"))
	if n == 0 {
		return nil
	}
	p := objc.Send[unsafe.Pointer](data, objc.RegisterName("bytes"))
	return append([]byte(nil), unsafe.Slice((*byte)(p), n)...)
}

// PNG of the general pasteboard image, its PNG data or any image it holds encoded as PNG, nil without one
func PlatformClipboardPNG() []byte {
	f, err := darwinLibs()
	if err != nil {
		return nil
	}
	pool := objc.ID(objc.GetClass("NSAutoreleasePool")).Send(objc.RegisterName("new"))
	defer pool.Send(objc.RegisterName("drain"))
	pb := objc.ID(objc.GetClass("NSPasteboard")).Send(objc.RegisterName("generalPasteboard"))
	if png := nsDataBytes(pb.Send(objc.RegisterName("dataForType:"), objc.ID(darwinConstant(f.appKit, "NSPasteboardTypePNG")))); len(png) > 0 {
		return png
	}
	image := objc.ID(objc.GetClass("NSImage")).Send(objc.RegisterName("alloc")).Send(objc.RegisterName("initWithPasteboard:"), pb)
	if image == 0 {
		return nil
	}
	defer image.Send(objc.RegisterName("release"))
	tiff := image.Send(objc.RegisterName("TIFFRepresentation"))
	if tiff == 0 {
		return nil
	}
	rep := objc.ID(objc.GetClass("NSBitmapImageRep")).Send(objc.RegisterName("imageRepWithData:"), tiff)
	if rep == 0 {
		return nil
	}
	props := objc.ID(objc.GetClass("NSDictionary")).Send(objc.RegisterName("dictionary"))
	return nsDataBytes(rep.Send(objc.RegisterName("representationUsingType:properties:"), uint(nsBitmapImageFileTypePNG), props))
}
