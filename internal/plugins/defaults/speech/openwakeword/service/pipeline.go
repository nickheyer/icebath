package service

import (
	"fmt"
	"path/filepath"
)

// Shapes of openWakeWord's pipeline: audio into mel frames, mel frames into embeddings, embeddings into a score
const (
	sampleRate    = 16000
	maxSeconds    = 10
	autofill      = 8
	chunk         = 1280
	melSamples    = 1760
	mels          = 32
	maxMels       = maxSeconds * 97
	embFrames     = 76
	embStep       = 8
	embValues     = 96
	maxEmbeddings = maxSeconds * embStep
)

// Mel spectrogram and embedding models over a rolling buffer of the audio heard
type features struct {
	mel, emb     *stage
	audio        []float32
	pendingAudio int
	frames       []float32
	pendingMels  int
}

func newFeatures(t *tflite, dir string) (*features, error) {
	mel, err := newStage(t, filepath.Join(dir, "models", "melspectrogram.tflite"), []int32{1, melSamples})
	if err != nil {
		return nil, err
	}
	if err := mel.expect(melSamples); err != nil {
		mel.release()
		return nil, err
	}
	emb, err := newStage(t, filepath.Join(dir, "models", "embedding_model.tflite"), []int32{1, embFrames, mels, 1})
	if err != nil {
		mel.release()
		return nil, err
	}
	if err := emb.expect(embFrames * mels); err != nil {
		mel.release()
		emb.release()
		return nil, err
	}
	return &features{mel: mel, emb: emb, audio: make([]float32, maxSeconds*sampleRate), pendingAudio: autofill * sampleRate,
		frames: make([]float32, maxMels*mels)}, nil
}

// Feeds samples, yielding each embedding window they complete
func (f *features) process(samples []int16, yield func([]float32) error) error {
	if !f.mel.open() || !f.emb.open() {
		return nil
	}
	values := make([]float32, len(samples))
	for i, v := range samples {
		values[i] = float32(v)
	}
	shift(f.audio, values)
	f.pendingAudio = min(len(f.audio), f.pendingAudio+len(samples))
	for f.pendingAudio >= melSamples {
		start := len(f.audio) - f.pendingAudio
		f.pendingAudio = max(0, f.pendingAudio-chunk)
		frames, err := f.mel.run(f.audio[start : start+melSamples])
		if err != nil {
			return err
		}
		if len(frames)%mels != 0 {
			return f.mel.shape(fmt.Sprintf("mel spectrogram output of %d values is not whole %d-band frames", len(frames), mels))
		}
		for i, v := range frames {
			frames[i] = v/10 + 2
		}
		shift(f.frames, frames)
		f.pendingMels = min(maxMels, f.pendingMels+len(frames)/mels)
		for f.pendingMels >= embFrames {
			start := maxMels - f.pendingMels
			f.pendingMels = max(0, f.pendingMels-embStep)
			embedding, err := f.emb.run(f.frames[start*mels : (start+embFrames)*mels])
			if err != nil {
				return err
			}
			if len(embedding)%embValues != 0 {
				return f.emb.shape(fmt.Sprintf("embedding output of %d values is not whole %d-value windows", len(embedding), embValues))
			}
			if err := yield(embedding); err != nil {
				return err
			}
		}
	}
	return nil
}

func (f *features) reset() {
	clear(f.audio)
	f.pendingAudio = autofill * sampleRate
	clear(f.frames)
	f.pendingMels = 0
}

func (f *features) close() {
	f.mel.release()
	f.emb.release()
}

// Classifier of one wake phrase over the latest embedding windows
type classifier struct {
	stage      *stage
	windows    int
	embeddings []float32
	pending    int
}

func newClassifier(t *tflite, path string) (*classifier, error) {
	s, err := newStage(t, path, nil)
	if err != nil {
		return nil, err
	}
	dims := s.t.numDims(s.in)
	if dims < 2 {
		s.release()
		return nil, s.shape(fmt.Sprintf("classifier input has %d dimensions, want at least 2", dims))
	}
	windows := int(s.t.dim(s.in, 1))
	if windows < 1 {
		s.release()
		return nil, s.shape(fmt.Sprintf("classifier input takes %d embedding windows", windows))
	}
	if err := s.expect(windows * embValues); err != nil {
		s.release()
		return nil, err
	}
	return &classifier{stage: s, windows: windows, embeddings: make([]float32, max(maxEmbeddings, windows)*embValues)}, nil
}

// Feeds embedding windows, yielding the score of each full window run
func (c *classifier) process(embedding []float32, yield func(float32)) error {
	if !c.stage.open() {
		return nil
	}
	rows := len(c.embeddings) / embValues
	shift(c.embeddings, embedding)
	c.pending = min(rows, c.pending+len(embedding)/embValues)
	for c.pending >= c.windows {
		start := rows - c.pending
		c.pending = max(0, c.pending-1)
		scores, err := c.stage.run(c.embeddings[start*embValues : (start+c.windows)*embValues])
		if err != nil {
			return err
		}
		if len(scores) != 1 {
			return c.stage.shape(fmt.Sprintf("classifier output has %d values, want 1", len(scores)))
		}
		yield(scores[0])
	}
	return nil
}

func (c *classifier) reset() {
	clear(c.embeddings)
	c.pending = 0
}

func (c *classifier) close() { c.stage.release() }
