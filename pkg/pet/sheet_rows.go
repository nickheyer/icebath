package pet

import (
	"image"
	"os"

	"github.com/nickheyer/icebath/pkg/internal/pypil"
)

// Decoded RGBA spritesheet of a path
func loadSheet(path string) (*image.NRGBA, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	decoded, _, err := image.Decode(f)
	if err != nil {
		return nil, err
	}
	return pypil.ToNRGBA(decoded), nil
}

// Real frame count of every concrete row of a sheet, the frames before its first blank cell
func RowFrameCounts(path string) (map[string]int, error) {
	sheet, err := loadSheet(path)
	if err != nil {
		return nil, err
	}
	cols, rowCount := max(1, sheet.Rect.Dx()/FrameW), max(1, sheet.Rect.Dy()/FrameH)
	rows := StateRowsForGrid(rowCount)
	out := map[string]int{}
	for rowIdx, name := range rows[:min(len(rows), rowCount)] {
		top := rowIdx * FrameH
		count := cols
		for col := range cols {
			if frameIsBlank(crop(sheet, image.Rect(col*FrameW, top, col*FrameW+FrameW, top+FrameH))) {
				count = col
				break
			}
		}
		out[name] = count
	}
	return out, nil
}

// Row count of a sheet in frame heights, at least one
func SheetRowCount(path string) (int, error) {
	f, err := os.Open(path)
	if err != nil {
		return 0, err
	}
	defer f.Close()
	config, _, err := image.DecodeConfig(f)
	if err != nil {
		return 0, err
	}
	return max(1, config.Height/FrameH), nil
}
