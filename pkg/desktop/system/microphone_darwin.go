//go:build darwin

package system

import (
	"sync"

	"github.com/ebitengine/purego"
	"github.com/ebitengine/purego/objc"
)

const (
	avAuthorizationNotDetermined = 0
	avAuthorizationRestricted    = 1
	avAuthorizationDenied        = 2
)

var avFoundation = sync.OnceValues(func() (uintptr, error) {
	return purego.Dlopen("/System/Library/Frameworks/AVFoundation.framework/AVFoundation", purego.RTLD_LAZY|purego.RTLD_GLOBAL)
})

// AVCaptureDevice audio authorization, requesting it when undetermined and refusing when denied or restricted
func askForMicrophoneAccess() bool {
	lib, err := avFoundation()
	if err != nil {
		return false
	}
	audio := objc.ID(darwinConstant(lib, "AVMediaTypeAudio"))
	device := objc.ID(objc.GetClass("AVCaptureDevice"))
	switch objc.Send[int](device, objc.RegisterName("authorizationStatusForMediaType:"), audio) {
	case avAuthorizationNotDetermined:
		granted := make(chan bool, 1)
		block := objc.NewBlock(func(_ objc.Block, ok bool) { granted <- ok })
		defer block.Release()
		device.Send(objc.RegisterName("requestAccessForMediaType:completionHandler:"), audio, block)
		return <-granted
	case avAuthorizationRestricted, avAuthorizationDenied:
		return false
	}
	return true
}
