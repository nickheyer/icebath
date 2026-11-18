package app

import (
	"time"
)

// Wall clock seconds as a float
func nowSeconds() float64 { return float64(time.Now().UnixNano()) / 1e9 }
