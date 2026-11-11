package media

// PeakRMS is the loudest frame level of the current capture
func (r *AudioRecorder) PeakRMS() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.peakRMS
}
