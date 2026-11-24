package portaudioutil

import (
	"fmt"
	"math"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
)

// Way audio flows through a device or a stream
type Direction uint8

const (
	// Audio captured from a device
	Input Direction = iota + 1
	// Audio played to a device
	Output
)

// "input" or "output"
func (d Direction) String() string {
	switch d {
	case Input:
		return "input"
	case Output:
		return "output"
	}
	return "direction " + strconv.Itoa(int(d))
}

func (d Direction) valid() bool { return d == Input || d == Output }

// One audio device as PortAudio reports it, the latencies being the defaults PortAudio suggests for it
type Device struct {
	Name                     string
	Index                    int
	HostAPI                  int
	MaxInputChannels         int
	MaxOutputChannels        int
	DefaultLowInputLatency   time.Duration
	DefaultLowOutputLatency  time.Duration
	DefaultHighInputLatency  time.Duration
	DefaultHighOutputLatency time.Duration
	DefaultSampleRate        float64
}

func (d Device) channels(dir Direction) int {
	if dir == Input {
		return d.MaxInputChannels
	}
	return d.MaxOutputChannels
}

// One host API such as ALSA, Core Audio or WASAPI, with the indexes of its devices and of its default devices, -1 for none
type HostAPI struct {
	Name                string
	Devices             []int
	DefaultInputDevice  int
	DefaultOutputDevice int
}

// PortAudio loaded and initialized: device and host API queries, streams, and one playback of a buffer at a time
type Audio interface {
	Devices() ([]Device, error)
	Device(selector any, dir Direction) (Device, error)
	HostAPI(index int) (HostAPI, error)
	Play(data []int16, sampleRate float64, blockSize int) error
	Stop()
	Playback() (Stream, error)
	OpenInput(p StreamParams, callback InputCallback) (InputStream, error)
	OpenOutput(p StreamParams) (OutputStream, error)
}

type audio struct{}

// Every device PortAudio reports, in index order
func (audio) Devices() ([]Device, error) {
	count := paGetDeviceCount()
	if err := check(count, "count devices"); err != nil {
		return nil, err
	}
	devices := make([]Device, 0, count)
	for i := range int(count) {
		dev, ok := deviceAt(i)
		if !ok {
			return nil, &Error{Op: "query device " + strconv.Itoa(i), Text: "no device information"}
		}
		devices = append(devices, dev)
	}
	return devices, nil
}

// The device a selector names for a direction: nil for the default device, an int index, or a name query whose words must all appear in order in "name, host API"; a DeviceError when it names no device with channels in that direction or several
func (audio) Device(selector any, dir Direction) (Device, error) {
	if !dir.valid() {
		return Device{}, fmt.Errorf("portaudio device: invalid %s", dir)
	}
	index, err := resolveDevice(selector, dir)
	if err != nil {
		return Device{}, err
	}
	dev, ok := deviceAt(index)
	if !ok || dev.channels(dir) < 1 {
		return Device{}, &DeviceError{Selector: selector, Direction: dir}
	}
	return dev, nil
}

// The host API at an index with the indexes of its devices
func (audio) HostAPI(index int) (HostAPI, error) { return hostAPI(index) }

func hostAPI(index int) (HostAPI, error) {
	var info *paHostApiInfo
	if index >= 0 && index <= math.MaxInt32 {
		info = paGetHostApiInfo(int32(index))
	}
	if info == nil {
		return HostAPI{}, &Error{Op: "query host API " + strconv.Itoa(index), Text: "no such host API"}
	}
	api := HostAPI{Name: cstr(info.name), Devices: make([]int, 0, max(info.deviceCount, 0)),
		DefaultInputDevice: int(info.defaultInputDevice), DefaultOutputDevice: int(info.defaultOutputDevice)}
	for i := range info.deviceCount {
		api.Devices = append(api.Devices, int(paHostApiDeviceIndexToDeviceIndex(int32(index), i)))
	}
	return api, nil
}

// Device at an index, false when PortAudio has none there; a name that is not UTF-8 is decoded the way its host API encodes names
func deviceAt(index int) (Device, bool) {
	if index < 0 || index > math.MaxInt32 {
		return Device{}, false
	}
	info := paGetDeviceInfo(int32(index))
	if info == nil {
		return Device{}, false
	}
	name := cstr(info.name)
	if !utf8.ValidString(name) {
		name = deviceName(name, info.hostAPI)
	}
	return Device{Name: name, Index: index, HostAPI: int(info.hostAPI), MaxInputChannels: int(info.maxInputChannels),
		MaxOutputChannels: int(info.maxOutputChannels), DefaultLowInputLatency: seconds(info.defaultLowInputLatency),
		DefaultLowOutputLatency: seconds(info.defaultLowOutputLatency), DefaultHighInputLatency: seconds(info.defaultHighInputLatency),
		DefaultHighOutputLatency: seconds(info.defaultHighOutputLatency), DefaultSampleRate: info.defaultSampleRate}, true
}

func seconds(s float64) time.Duration { return time.Duration(s * float64(time.Second)) }

// Device index a selector stands for in a direction, not yet checked against the devices PortAudio has
func resolveDevice(selector any, dir Direction) (int, error) {
	switch sel := selector.(type) {
	case nil:
		if dir == Input {
			return int(paGetDefaultInputDevice()), nil
		}
		return int(paGetDefaultOutputDevice()), nil
	case int:
		return sel, nil
	case string:
		devices, err := audio{}.Devices()
		if err != nil {
			return 0, err
		}
		return matchDevice(devices, sel, dir, hostAPINames())
	}
	return 0, fmt.Errorf("portaudio device selector of type %T, want nil, an int index or a name", selector)
}

// Host API names by index, each asked of PortAudio at most once through the returned function
func hostAPINames() func(int) (string, error) {
	names := map[int]string{}
	return func(index int) (string, error) {
		if name, ok := names[index]; ok {
			return name, nil
		}
		api, err := hostAPI(index)
		if err != nil {
			return "", err
		}
		names[index] = api.Name
		return api.Name, nil
	}
}

// Index of the one device with channels in the direction whose "name, host API" holds every query word in order, ignoring case; among several, the one named exactly by the query
func matchDevice(devices []Device, query string, dir Direction, hostName func(int) (string, error)) (int, error) {
	words := strings.Fields(strings.ToLower(query))
	var ids, exact []int
	var lines []string
	for _, dev := range devices {
		if dev.channels(dir) <= 0 {
			continue
		}
		api, err := hostName(dev.HostAPI)
		if err != nil {
			return 0, err
		}
		full := dev.Name + ", " + api
		if !containsInOrder(strings.ToLower(full), words) {
			continue
		}
		ids = append(ids, dev.Index)
		lines = append(lines, "["+strconv.Itoa(dev.Index)+"] "+full)
		if strings.EqualFold(dev.Name, query) {
			exact = append(exact, dev.Index)
		}
	}
	switch {
	case len(ids) == 1:
		return ids[0], nil
	case len(exact) == 1:
		return exact[0], nil
	case len(ids) == 0:
		return 0, &DeviceError{Selector: query, Direction: dir}
	}
	return 0, &DeviceError{Selector: query, Direction: dir, Matches: lines}
}

// Whether every word occurs in s, each one after the one before it
func containsInOrder(s string, words []string) bool {
	for _, w := range words {
		at := strings.Index(s, w)
		if at < 0 {
			return false
		}
		s = s[at+len(w):]
	}
	return true
}
