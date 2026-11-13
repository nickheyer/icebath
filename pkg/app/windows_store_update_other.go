//go:build !windows

package app

import (
	"fmt"
	"math/big"
	"runtime"

	"github.com/nickheyer/icebath/pkg/internal/pyjson"
)

// The Microsoft Store API lives in the Windows Runtime, which only a Windows build reaches
func storeRuntimeUnavailable() error {
	return fmt.Errorf("the Windows Runtime is unavailable on %s", runtime.GOOS)
}

func storeInitApartmentNative() error { return storeRuntimeUnavailable() }

func storeUninitApartmentNative() {}

func storeExecuteNative(string, *big.Int) (*pyjson.Object, error) {
	return nil, storeRuntimeUnavailable()
}
