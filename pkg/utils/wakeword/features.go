package wakewordutil

import (
	"encoding/binary"
	"fmt"
	"runtime"
	"slices"
)

// Pipeline geometry: 80 ms chunks of 16 kHz audio, mel frames of 32 bands at 97 a second, embeddings of 96 values from 76 mel frames every 8, ten seconds kept
const (
	sampleRate      = 16000
	maxSeconds      = 10
	autofillSeconds = 8
	samplesPerChunk = 1280
	melSamples      = 1760
	numMels         = 32
	maxMels         = maxSeconds * 97
	embFeatures     = 76
	embStep         = 8
	wwFeatures      = 96
	maxEmb          = maxSeconds * embStep
)

var (
	melShape = []int32{1, melSamples}
	embShape = []int32{1, embFeatures, numMels, 1}
)

// Streaming front end that turns 16 kHz 16-bit mono audio into embedding windows through the mel spectrogram and embedding models; not safe for concurrent use
type Features struct {
	mel, emb     *stage
	samples      []float32
	audio        []float32
	pendingAudio int
	mels         []float32
	pendingMels  int
	cleanup      runtime.Cleanup
}

// Front end over the mel spectrogram and embedding model files, eight seconds of silence counted as pending audio so the first chunks already score
func (rt *Runtime) Features(melPath, embPath string) (*Features, error) {
	mel, err := newStage(rt, melPath, melShape)
	if err != nil {
		return nil, err
	}
	if err := mel.expectInput(melSamples); err != nil {
		mel.release()
		return nil, err
	}
	emb, err := newStage(rt, embPath, embShape)
	if err != nil {
		mel.release()
		return nil, err
	}
	if err := emb.expectInput(embFeatures * numMels); err != nil {
		mel.release()
		emb.release()
		return nil, err
	}
	f := &Features{mel: mel, emb: emb, audio: make([]float32, maxSeconds*sampleRate), pendingAudio: autofillSeconds * sampleRate,
		mels: make([]float32, maxMels*numMels)}
	f.cleanup = runtime.AddCleanup(f, releaseStages, [2]*stage{mel, emb})
	return f, nil
}

func releaseStages(stages [2]*stage) {
	for _, s := range stages {
		s.release()
	}
}

// Feeds 16-bit native endian mono audio and yields each group of embedding windows that becomes ready, stopping when yield returns false; nothing once closed; the yielded slice is only valid until yield returns
func (f *Features) ProcessStreaming(audioChunk []byte, yield func([]float32) bool) error {
	defer runtime.KeepAlive(f)
	if !f.mel.open() || !f.emb.open() {
		return nil
	}
	if len(audioChunk)%2 != 0 {
		return fmt.Errorf("%w: audio chunk of %d bytes is not whole 16-bit samples", ErrPartialFrame, len(audioChunk))
	}
	n := len(audioChunk) / 2
	f.samples = slices.Grow(f.samples[:0], n)[:n]
	for i := range f.samples {
		f.samples[i] = float32(int16(binary.NativeEndian.Uint16(audioChunk[2*i:])))
	}
	shiftAppend(f.audio, f.samples)
	f.pendingAudio = min(len(f.audio), f.pendingAudio+n)
	for f.pendingAudio >= melSamples {
		start := len(f.audio) - f.pendingAudio
		f.pendingAudio = max(0, f.pendingAudio-samplesPerChunk)
		mels, err := f.mel.run(f.audio[start : start+melSamples])
		if err != nil {
			return err
		}
		if len(mels)%numMels != 0 {
			return f.mel.shapeError(fmt.Sprintf("mel spectrogram output of %d values is not whole %d-band frames", len(mels), numMels))
		}
		for i, v := range mels {
			mels[i] = v/10 + 2
		}
		shiftAppend(f.mels, mels)
		f.pendingMels = min(maxMels, f.pendingMels+len(mels)/numMels)
		for f.pendingMels >= embFeatures {
			start := maxMels - f.pendingMels
			f.pendingMels = max(0, f.pendingMels-embStep)
			emb, err := f.emb.run(f.mels[start*numMels : (start+embFeatures)*numMels])
			if err != nil {
				return err
			}
			if len(emb)%wwFeatures != 0 {
				return f.emb.shapeError(fmt.Sprintf("embedding output of %d values is not whole %d-value windows", len(emb), wwFeatures))
			}
			if !yield(emb) {
				return nil
			}
		}
	}
	return nil
}

// Clears the rolling audio and mel buffers back to their freshly loaded state
func (f *Features) Reset() {
	clear(f.audio)
	f.pendingAudio = autofillSeconds * sampleRate
	clear(f.mels)
	f.pendingMels = 0
}

// Releases both interpreters and both models, later calls doing nothing
func (f *Features) Close() {
	f.cleanup.Stop()
	f.mel.release()
	f.emb.release()
}
