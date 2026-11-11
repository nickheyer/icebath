package pyzip

import (
	"bytes"
	"encoding/binary"
)

// zipfile.ZipFile.comment, the comment bytes the end of central directory record carries
func (r *ZipReader) Comment() ([]byte, error) {
	info, err := r.fp.Stat()
	if err != nil {
		return nil, err
	}
	filesize := info.Size()
	if filesize < sizeEndCentDir {
		return nil, nil
	}
	data := make([]byte, sizeEndCentDir)
	if n := readAt(r.fp, data, filesize-sizeEndCentDir); n == sizeEndCentDir && bytes.Equal(data[:4], stringEndArchive) && data[20] == 0 && data[21] == 0 {
		return []byte{}, nil
	}
	maxCommentStart := max(filesize-maxComment-sizeEndCentDir, 0)
	tail := make([]byte, min(filesize-maxCommentStart, maxComment+sizeEndCentDir))
	tail = tail[:readAt(r.fp, tail, maxCommentStart)]
	start := bytes.LastIndex(tail, stringEndArchive)
	if start < 0 || len(tail[start:]) < sizeEndCentDir {
		return nil, nil
	}
	size := int(binary.LittleEndian.Uint16(tail[start+20 : start+22]))
	comment := tail[start+sizeEndCentDir:]
	return append([]byte{}, comment[:min(size, len(comment))]...), nil
}
