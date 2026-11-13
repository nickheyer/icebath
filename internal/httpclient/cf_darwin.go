package httpclient

import (
	"sync"

	"github.com/nickheyer/icebath/internal/dl"
)

const (
	cfStringEncodingUTF8 = 0x08000100
	cfNumberSInt32Type   = 3
)

// CoreFoundation, CFNetwork and Security entry points, bound once since C callbacks cannot be freed
type cfBinding struct {
	err error

	release              func(ref uintptr)
	stringCreate         func(alloc uintptr, text string, encoding uint32) uintptr
	stringLength         func(ref uintptr) int
	stringMaxSize        func(length int, encoding uint32) int
	stringGet            func(ref uintptr, buf []byte, size int, encoding uint32) bool
	typeOf               func(ref uintptr) uint
	stringType           func() uint
	numberType           func() uint
	numberGet            func(ref uintptr, kind int, out *int32) bool
	dictGet              func(dict, key uintptr) uintptr
	arrayCount           func(ref uintptr) int
	arrayAt              func(ref uintptr, index int) uintptr
	arrayCreate          func(alloc uintptr, capacity int, callbacks uintptr) uintptr
	arrayAppend          func(ref, value uintptr)
	dataCreate           func(alloc uintptr, bytes []byte, length int) uintptr
	urlCreate            func(alloc, text, base uintptr) uintptr
	runLoopCurrent       func() uintptr
	runLoopAdd           func(loop, source, mode uintptr)
	runLoopRemove        func(loop, source, mode uintptr)
	runLoopRun           func(mode uintptr, seconds float64, returnAfterSource bool) int32
	runLoopStop          func(loop uintptr)
	errorCode            func(ref uintptr) int
	errorDescription     func(ref uintptr) uintptr
	typeArrayCallbacks   uintptr
	systemProxySettings  func() uintptr
	proxiesForURL        func(target, settings uintptr) uintptr
	runPACURL            func(pac, target, callback uintptr, context *[5]uintptr) uintptr
	runPACScript         func(script, target, callback uintptr, context *[5]uintptr) uintptr
	pacCallback          uintptr
	dataLength           func(ref uintptr) int
	dataBytes            func(ref uintptr) *byte
	certificateCreate    func(alloc, data uintptr) uintptr
	certificateCopyData  func(cert uintptr) uintptr
	policySSL            func(server bool, hostname uintptr) uintptr
	trustCreate          func(certs, policies uintptr, trust *uintptr) int32
	trustSetOCSP         func(trust, response uintptr) int32
	trustSetSCTs         func(trust, scts uintptr) int32
	trustEvaluateWithErr func(trust uintptr, err *uintptr) bool
	trustCopyChain       func(trust uintptr) uintptr
}

var (
	cfOnce sync.Once
	cf     cfBinding
)

// Result slot of the one PAC evaluation allowed at a time, filled by the shared callback
var pacRun struct {
	mu     sync.Mutex
	list   uintptr
	failed bool
	done   bool
}

// Bound frameworks, loading them on first use
func loadCF() *cfBinding {
	cfOnce.Do(func() { cf.err = cf.bind() })
	return &cf
}

func (b *cfBinding) bind() error {
	foundation, err := dl.Open("/System/Library/Frameworks/CoreFoundation.framework/CoreFoundation", dl.Global)
	if err != nil {
		return err
	}
	network, err := dl.Open("/System/Library/Frameworks/CFNetwork.framework/CFNetwork", dl.Global)
	if err != nil {
		return err
	}
	security, err := dl.Open("/System/Library/Frameworks/Security.framework/Security", dl.Global)
	if err != nil {
		return err
	}
	callbacks, err := foundation.Symbol("kCFTypeArrayCallBacks")
	if err != nil {
		return err
	}
	b.typeArrayCallbacks = callbacks
	for _, set := range []struct {
		lib      *dl.Library
		bindings []dl.Binding
	}{
		{foundation, []dl.Binding{
			{Fn: &b.release, Name: "CFRelease"},
			{Fn: &b.stringCreate, Name: "CFStringCreateWithCString"},
			{Fn: &b.stringLength, Name: "CFStringGetLength"},
			{Fn: &b.stringMaxSize, Name: "CFStringGetMaximumSizeForEncoding"},
			{Fn: &b.stringGet, Name: "CFStringGetCString"},
			{Fn: &b.typeOf, Name: "CFGetTypeID"},
			{Fn: &b.stringType, Name: "CFStringGetTypeID"},
			{Fn: &b.numberType, Name: "CFNumberGetTypeID"},
			{Fn: &b.numberGet, Name: "CFNumberGetValue"},
			{Fn: &b.dictGet, Name: "CFDictionaryGetValue"},
			{Fn: &b.arrayCount, Name: "CFArrayGetCount"},
			{Fn: &b.arrayAt, Name: "CFArrayGetValueAtIndex"},
			{Fn: &b.arrayCreate, Name: "CFArrayCreateMutable"},
			{Fn: &b.arrayAppend, Name: "CFArrayAppendValue"},
			{Fn: &b.dataCreate, Name: "CFDataCreate"},
			{Fn: &b.dataLength, Name: "CFDataGetLength"},
			{Fn: &b.dataBytes, Name: "CFDataGetBytePtr"},
			{Fn: &b.urlCreate, Name: "CFURLCreateWithString"},
			{Fn: &b.runLoopCurrent, Name: "CFRunLoopGetCurrent"},
			{Fn: &b.runLoopAdd, Name: "CFRunLoopAddSource"},
			{Fn: &b.runLoopRemove, Name: "CFRunLoopRemoveSource"},
			{Fn: &b.runLoopRun, Name: "CFRunLoopRunInMode"},
			{Fn: &b.runLoopStop, Name: "CFRunLoopStop"},
			{Fn: &b.errorCode, Name: "CFErrorGetCode"},
			{Fn: &b.errorDescription, Name: "CFErrorCopyDescription"},
		}},
		{network, []dl.Binding{
			{Fn: &b.systemProxySettings, Name: "CFNetworkCopySystemProxySettings"},
			{Fn: &b.proxiesForURL, Name: "CFNetworkCopyProxiesForURL"},
			{Fn: &b.runPACURL, Name: "CFNetworkExecuteProxyAutoConfigurationURL"},
			{Fn: &b.runPACScript, Name: "CFNetworkExecuteProxyAutoConfigurationScript"},
		}},
		{security, []dl.Binding{
			{Fn: &b.certificateCreate, Name: "SecCertificateCreateWithData"},
			{Fn: &b.certificateCopyData, Name: "SecCertificateCopyData"},
			{Fn: &b.policySSL, Name: "SecPolicyCreateSSL"},
			{Fn: &b.trustCreate, Name: "SecTrustCreateWithCertificates"},
			{Fn: &b.trustSetOCSP, Name: "SecTrustSetOCSPResponse"},
			{Fn: &b.trustSetSCTs, Name: "SecTrustSetSignedCertificateTimestamps"},
			{Fn: &b.trustEvaluateWithErr, Name: "SecTrustEvaluateWithError"},
			{Fn: &b.trustCopyChain, Name: "SecTrustCopyCertificateChain"},
		}},
	} {
		if err := set.lib.BindAll(set.bindings); err != nil {
			return err
		}
	}
	pac, err := dl.Callback(func(_, list, failure uintptr) uintptr {
		if failure != 0 || list == 0 {
			pacRun.failed = true
		} else {
			pacRun.list = b.retainArray(list)
		}
		pacRun.done = true
		b.runLoopStop(b.runLoopCurrent())
		return 0
	})
	if err != nil {
		return err
	}
	b.pacCallback = pac
	return nil
}

// Copy of a callback owned array, kept alive past the callback by rebuilding it from its values
func (b *cfBinding) retainArray(list uintptr) uintptr {
	copied := b.arrayCreate(0, 0, b.typeArrayCallbacks)
	for i := 0; i < b.arrayCount(list); i++ {
		b.arrayAppend(copied, b.arrayAt(list, i))
	}
	return copied
}

// CFString of a Go string, owned by the caller
func (b *cfBinding) str(text string) uintptr {
	return b.stringCreate(0, text, cfStringEncodingUTF8)
}

// Go string of a CFString, empty for null or non-string references
func (b *cfBinding) goString(ref uintptr) string {
	if ref == 0 || b.typeOf(ref) != b.stringType() {
		return ""
	}
	size := b.stringMaxSize(b.stringLength(ref), cfStringEncodingUTF8) + 1
	buf := make([]byte, size)
	if !b.stringGet(ref, buf, size, cfStringEncodingUTF8) {
		return ""
	}
	for i, c := range buf {
		if c == 0 {
			return string(buf[:i])
		}
	}
	return string(buf)
}

// Value under a string key of a dictionary
func (b *cfBinding) value(dict uintptr, key string) uintptr {
	name := b.str(key)
	defer b.release(name)
	return b.dictGet(dict, name)
}

// Integer under a string key, false when absent or not a CFNumber
func (b *cfBinding) number(dict uintptr, key string) (int, bool) {
	ref := b.value(dict, key)
	if ref == 0 || b.typeOf(ref) != b.numberType() {
		return 0, false
	}
	var out int32
	if !b.numberGet(ref, cfNumberSInt32Type, &out) {
		return 0, false
	}
	return int(out), true
}

// CFData holding a copy of the bytes, owned by the caller
func (b *cfBinding) data(raw []byte) uintptr {
	return b.dataCreate(0, raw, len(raw))
}
