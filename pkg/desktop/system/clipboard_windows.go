//go:build windows

package system

import (
	"bytes"
	"encoding/binary"
	"image/png"
	"time"
	"unsafe"

	"golang.org/x/image/bmp"
	"golang.org/x/sys/windows"
)

const (
	cfDIB        = 8
	biBitfields  = 3
	bitmapHeader = 14
)

var (
	procOpenClipboard           = user32.NewProc("OpenClipboard")
	procCloseClipboard          = user32.NewProc("CloseClipboard")
	procGetClipboardData        = user32.NewProc("GetClipboardData")
	procRegisterClipboardFormat = user32.NewProc("RegisterClipboardFormatW")
	procGlobalLock              = kernel32.NewProc("GlobalLock")
	procGlobalUnlock            = kernel32.NewProc("GlobalUnlock")
	procGlobalSize              = kernel32.NewProc("GlobalSize")
	procRtlMoveMemory           = kernel32.NewProc("RtlMoveMemory")
)

// Opens the clipboard, retrying while another process holds it
func openClipboard() bool {
	for i := 0; i < 5; i++ {
		if r, _, _ := procOpenClipboard.Call(0); r != 0 {
			return true
		}
		time.Sleep(5 * time.Millisecond)
	}
	return false
}

func globalBytes(handle uintptr) []byte {
	if handle == 0 {
		return nil
	}
	p, _, _ := procGlobalLock.Call(handle)
	if p == 0 {
		return nil
	}
	defer procGlobalUnlock.Call(handle)
	n, _, _ := procGlobalSize.Call(handle)
	if n == 0 {
		return nil
	}
	out := make([]byte, n)
	procRtlMoveMemory.Call(uintptr(unsafe.Pointer(&out[0])), p, n)
	return out
}

// PNG of a packed DIB, a BMP file header prepended for the decoder
func dibToPNG(dib []byte) []byte {
	if len(dib) < 40 {
		return nil
	}
	headerSize := binary.LittleEndian.Uint32(dib[0:])
	bitCount := binary.LittleEndian.Uint16(dib[14:])
	compression := binary.LittleEndian.Uint32(dib[16:])
	colors := binary.LittleEndian.Uint32(dib[32:])
	if colors == 0 && bitCount <= 8 {
		colors = 1 << bitCount
	}
	offset := bitmapHeader + headerSize + 4*colors
	if headerSize == 40 && compression == biBitfields {
		offset += 12
	}
	file := make([]byte, bitmapHeader, bitmapHeader+len(dib))
	file[0], file[1] = 'B', 'M'
	binary.LittleEndian.PutUint32(file[2:], uint32(bitmapHeader+len(dib)))
	binary.LittleEndian.PutUint32(file[10:], offset)
	file = append(file, dib...)
	img, err := bmp.Decode(bytes.NewReader(file))
	if err != nil {
		return nil
	}
	var out bytes.Buffer
	if png.Encode(&out, img) != nil {
		return nil
	}
	return out.Bytes()
}

// PNG of the clipboard image, the registered PNG format first and the device independent bitmap encoded second
func PlatformClipboardPNG() []byte {
	if !openClipboard() {
		return nil
	}
	defer procCloseClipboard.Call()
	name, _ := windows.UTF16PtrFromString("PNG")
	if format, _, _ := procRegisterClipboardFormat.Call(uintptr(unsafe.Pointer(name))); format != 0 {
		handle, _, _ := procGetClipboardData.Call(format)
		if data := globalBytes(handle); len(data) > 0 {
			return data
		}
	}
	handle, _, _ := procGetClipboardData.Call(cfDIB)
	return dibToPNG(globalBytes(handle))
}
