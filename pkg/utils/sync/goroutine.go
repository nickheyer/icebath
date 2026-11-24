package syncutil

import (
	"bytes"
	"runtime"
)

// Prefix of the header line the runtime writes above every goroutine's stack
var goroutineHeader = []byte("goroutine ")

// Number the runtime gave the calling goroutine, never 0 and never reused while the process lives
func GoroutineID() uint64 {
	var buf [40]byte
	id, _ := headerID(buf[:runtime.Stack(buf[:], false)])
	return id
}

// Goroutine number a stack header line starts with, false for any other line
func headerID(line []byte) (uint64, bool) {
	digits, ok := bytes.CutPrefix(line, goroutineHeader)
	if !ok {
		return 0, false
	}
	var id uint64
	n := 0
	for ; n < len(digits) && digits[n] >= '0' && digits[n] <= '9'; n++ {
		id = id*10 + uint64(digits[n]-'0')
	}
	if n == 0 || n == len(digits) || digits[n] != ' ' {
		return 0, false
	}
	return id, true
}

// Calls keep with the number of every goroutine alive now, stopping the world to list them; the buffer starts at about 1 KiB per goroutine so a busy process is not stopped once per doubling
func eachLiveGoroutine(keep func(id uint64)) {
	buf := make([]byte, max(64<<10, runtime.NumGoroutine()<<10))
	for {
		n := runtime.Stack(buf, true)
		if n < len(buf) {
			buf = buf[:n]
			break
		}
		buf = make([]byte, 2*len(buf))
	}
	for line := range bytes.Lines(buf) {
		if id, ok := headerID(line); ok {
			keep(id)
		}
	}
}
