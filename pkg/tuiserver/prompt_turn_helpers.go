package tuiserver

import ()

// Runs body and returns what it panicked with, nil when it returned
func catchPanic(body func()) (failure any) {
	defer func() {
		if r := recover(); r != nil {
			failure = r
		}
	}()
	body()
	return nil
}

// Raises a returned error like the Python call that would have raised it
func panicIf(err error) {
	if err != nil {
		panic(err)
	}
}
