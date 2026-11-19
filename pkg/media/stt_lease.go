package media

import (
	"sync"
)

var (
	sttLeaseMu sync.Mutex
	sttLeases  = map[string]bool{}
)
