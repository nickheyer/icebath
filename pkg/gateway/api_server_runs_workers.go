package gateway

import "sync"

// Executor threads still inside an API server agent turn, taken before submit and released by the worker
var apiWorker struct {
	sync.Mutex
	live int
}

// Executor threads still inside an API server agent turn
func APIWorkerLiveCount() int {
	apiWorker.Lock()
	defer apiWorker.Unlock()
	return apiWorker.live
}

// Submits fn through submit with the worker lifetime count held until the worker itself returns
func SubmitAPIWorker(submit func(counted func() (any, error)) error, fn func() (any, error)) error {
	apiWorker.Lock()
	apiWorker.live++
	apiWorker.Unlock()
	counted := func() (any, error) {
		defer func() {
			apiWorker.Lock()
			apiWorker.live--
			apiWorker.Unlock()
		}()
		return fn()
	}
	if err := submit(counted); err != nil {
		apiWorker.Lock()
		apiWorker.live--
		apiWorker.Unlock()
		return err
	}
	return nil
}
