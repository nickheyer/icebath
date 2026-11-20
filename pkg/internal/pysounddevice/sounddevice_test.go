package pysounddevice

import (
	"errors"
	"slices"
	"testing"
)

func TestCallbackFlagsString(t *testing.T) {
	if got := CallbackFlags(0).String(); got != "" {
		t.Fatalf("empty flags %q", got)
	}
	if got := CallbackFlags(0x2 | 0x4 | 0x10).String(); got != "input overflow, output underflow, priming output" {
		t.Fatalf("flags %q", got)
	}
}

func TestPortAudioErrorFormat(t *testing.T) {
	plain := &PortAudioError{Msg: "Error querying device -1"}
	if plain.Error() != "Error querying device -1" {
		t.Fatal(plain.Error())
	}
	coded := &PortAudioError{Msg: "Error opening InputStream: Invalid sample rate", Code: -9997, HasCode: true}
	if coded.Error() != "Error opening InputStream: Invalid sample rate [PaErrorCode -9997]" {
		t.Fatal(coded.Error())
	}
	host := &PortAudioError{Msg: "Error starting stream: Unanticipated host error", Code: -9999, HasCode: true,
		HostAPIName: "ALSA", HostErrorCode: -16, HostErrorText: "Device or resource busy", HasHostError: true}
	if host.Error() != "Error starting stream: Unanticipated host error [PaErrorCode -9999] [ALSA error -16: Device or resource busy]" {
		t.Fatal(host.Error())
	}
}

func TestMatchDevice(t *testing.T) {
	devices := DeviceList{
		{Name: "HDA Intel PCH: ALC892 Analog (hw:0,0)", Index: 0, HostAPI: 0, MaxInputChannels: 2, MaxOutputChannels: 2},
		{Name: "pulse", Index: 1, HostAPI: 0, MaxInputChannels: 32, MaxOutputChannels: 32},
		{Name: "pulse monitor", Index: 2, HostAPI: 0, MaxInputChannels: 2},
		{Name: "HDMI 0", Index: 3, HostAPI: 1, MaxOutputChannels: 8},
	}
	names := func(i int) (string, error) { return []string{"ALSA", "JACK Audio Connection Kit"}[i], nil }
	cases := []struct {
		query, kind string
		want        int
		err         string
	}{
		{"pulse", "input", 1, ""},
		{"alc892 alsa", "input", 0, ""},
		{"hdmi", "output", 3, ""},
		{"hdmi", "input", 0, "No input device matching 'hdmi'"},
		{"jack hdmi", "", 0, "No input/output device matching 'jack hdmi'"},
		{"a", "input", 0, "Multiple input devices found for 'a':\n[0] HDA Intel PCH: ALC892 Analog (hw:0,0), ALSA\n[1] pulse, ALSA\n[2] pulse monitor, ALSA"},
	}
	for _, c := range cases {
		got, err := matchDevice(devices, c.query, c.kind, names)
		if c.err != "" {
			var ve *ValueError
			if !errors.As(err, &ve) || ve.Msg != c.err {
				t.Fatalf("%q %q error %v", c.query, c.kind, err)
			}
			continue
		}
		if err != nil || got != c.want {
			t.Fatalf("%q %q = %d %v", c.query, c.kind, got, err)
		}
	}
}

func TestPlayContextCallback(t *testing.T) {
	ctx := &playContext{data: []int16{1, 2, 3, 4, 5}}
	out := []int16{9, 9, 9}
	if r := ctx.callback(out, 3, 0); r != Continue || !slices.Equal(out, []int16{1, 2, 3}) {
		t.Fatalf("first block %v %v", r, out)
	}
	if r := ctx.callback(out, 3, 0x4); r != Continue || !slices.Equal(out, []int16{4, 5, 0}) || ctx.status != 0x4 {
		t.Fatalf("second block %v %v %v", r, out, ctx.status)
	}
	out = []int16{7, 7, 7}
	if r := ctx.callback(out, 3, 0); r != Abort || !slices.Equal(out, []int16{0, 0, 0}) {
		t.Fatalf("spent buffer %v %v", r, out)
	}
}
