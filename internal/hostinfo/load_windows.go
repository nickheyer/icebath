package hostinfo

import (
	"fmt"
	"sync"
	"unsafe"

	"golang.org/x/sys/windows"
)

const (
	pdhFmtDouble          = 0x00000200
	loadSampleSeconds     = 5
	processorQueueCounter = `\System\Processor Queue Length`
)

var (
	pdh                             = windows.NewLazySystemDLL("pdh.dll")
	procPdhOpenQueryW               = pdh.NewProc("PdhOpenQueryW")
	procPdhAddEnglishCounterW       = pdh.NewProc("PdhAddEnglishCounterW")
	procPdhCollectQueryDataEx       = pdh.NewProc("PdhCollectQueryDataEx")
	procPdhGetFormattedCounterValue = pdh.NewProc("PdhGetFormattedCounterValue")
	procPdhCloseQuery               = pdh.NewProc("PdhCloseQuery")
	loadDecay                       = [3]float64{0.9200444146293232478931553241, 0.9834714538216174894737477501, 0.9944598480048967508795473394}
)

// Damped processor queue averages starting at zero that a five second counter sampler feeds once started
type loadState struct {
	mu      sync.Mutex
	started bool
	values  [3]float64
}

// Formatted PDH counter value holding a double
type pdhCounterValue struct {
	_      [2]uint32
	double float64
}

// Averages rounded to two decimals, the sampler started by the first call, zero until samples arrive
func (s *loadState) read() ([3]float64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.started {
		if err := s.start(); err != nil {
			return [3]float64{}, err
		}
		s.started = true
	}
	var averages [3]float64
	for i, v := range s.values {
		averages[i] = roundDecimals(v, 2)
	}
	return averages, nil
}

// Opens the processor queue counter and folds a sample into the averages each time PDH collects one
func (s *loadState) start() error {
	for _, proc := range []*windows.LazyProc{procPdhOpenQueryW, procPdhAddEnglishCounterW, procPdhCollectQueryDataEx, procPdhGetFormattedCounterValue, procPdhCloseQuery} {
		if err := proc.Find(); err != nil {
			return fmt.Errorf("load average: %w", err)
		}
	}
	var query, counter uintptr
	if r, _, _ := procPdhOpenQueryW.Call(0, 0, uintptr(unsafe.Pointer(&query))); r != 0 {
		return fmt.Errorf("load average: PdhOpenQueryW status %#x", r)
	}
	path, err := windows.UTF16PtrFromString(processorQueueCounter)
	if err != nil {
		procPdhCloseQuery.Call(query)
		return fmt.Errorf("load average: %w", err)
	}
	if r, _, _ := procPdhAddEnglishCounterW.Call(query, uintptr(unsafe.Pointer(path)), 0, uintptr(unsafe.Pointer(&counter))); r != 0 {
		procPdhCloseQuery.Call(query)
		return fmt.Errorf("load average: PdhAddEnglishCounterW status %#x", r)
	}
	event, err := windows.CreateEvent(nil, 0, 0, nil)
	if err != nil {
		procPdhCloseQuery.Call(query)
		return fmt.Errorf("load average: %w", err)
	}
	if r, _, _ := procPdhCollectQueryDataEx.Call(query, loadSampleSeconds, uintptr(event)); r != 0 {
		procPdhCloseQuery.Call(query)
		windows.CloseHandle(event)
		return fmt.Errorf("load average: PdhCollectQueryDataEx status %#x", r)
	}
	go func() {
		for {
			if _, err := windows.WaitForSingleObject(event, windows.INFINITE); err != nil {
				return
			}
			s.sample(counter)
		}
	}()
	return nil
}

// Folds the current processor queue length into the three damped averages
func (s *loadState) sample(counter uintptr) {
	var value pdhCounterValue
	if r, _, _ := procPdhGetFormattedCounterValue.Call(counter, pdhFmtDouble, 0, uintptr(unsafe.Pointer(&value))); r != 0 {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for i, decay := range loadDecay {
		s.values[i] = s.values[i]*decay + value.double*(1-decay)
	}
}
