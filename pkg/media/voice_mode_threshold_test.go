package media

import "testing"

func TestStopComparesPeakAgainstAFractionalThreshold(t *testing.T) {
	withVoiceTempDir(t)
	r := NewAudioRecorder()
	r.SetSilenceThreshold(150.5)
	armRecorder(r, [][]int16{make([]int16, 16000)}, 150)
	if path, _ := r.Stop(); path != "" {
		t.Fatal("peak 150 is below 150.5 and must be discarded")
	}
	armRecorder(r, [][]int16{make([]int16, 16000)}, 151)
	if path, _ := r.Stop(); path == "" {
		t.Fatal("peak 151 is above 150.5 and must be kept")
	}
}

func TestSpeechDetectionComparesRMSAgainstAFractionalThreshold(t *testing.T) {
	r := NewAudioRecorder()
	r.SetSilenceThreshold(150.5)
	r.trackSpeech(150, 1)
	if r.speechStart != 0 {
		t.Fatal("rms 150 is not above 150.5 and starts no speech")
	}
	r.trackSpeech(151, 1)
	if r.speechStart != 1 {
		t.Fatal("rms 151 is above 150.5 and starts speech")
	}
}
