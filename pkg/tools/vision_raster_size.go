package tools

// Width and height of the first frame as Pillow Image.open reports size, an error for bombs and unreadable bytes
func RasterSize(data []byte) (int, int, error) {
	w, h, _, err := rasterConfig(data)
	return w, h, err
}
