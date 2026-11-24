package ziputil

import (
	"bufio"
	"encoding/binary"
	"io"
)

// Writes the central directory, the zip64 end records when the member count, directory size or offset need them, and the end record with the comment, then cuts the file where they end; every length was checked when its member began, so nothing here can refuse a member
func (w *Writer) writeDirectory() error {
	dst := io.NewOffsetWriter(w.f, w.off)
	if w.buf == nil {
		w.buf = bufio.NewWriterSize(dst, bufSize)
	} else {
		w.buf.Reset(dst)
	}
	le := binary.LittleEndian
	start, size := w.off, int64(0)
	var rec []byte
	for _, e := range w.members {
		fh := e.fh
		large := fh.CompressedSize64 >= uint32max || fh.UncompressedSize64 >= uint32max
		far := e.offset >= uint32max
		extra, version := fh.Extra, fh.ReaderVersion
		csize, usize, offset := uint32(fh.CompressedSize64), uint32(fh.UncompressedSize64), uint32(e.offset)
		if large || far {
			version = max(version, zip64Version)
			z := make([]byte, 4, zip64ExtraMax+len(fh.Extra))
			le.PutUint16(z, extraZip64)
			if large {
				z = le.AppendUint64(z, fh.UncompressedSize64)
				z = le.AppendUint64(z, fh.CompressedSize64)
				csize, usize = uint32max, uint32max
			}
			if far {
				z = le.AppendUint64(z, uint64(e.offset))
				offset = uint32max
			}
			le.PutUint16(z[2:], uint16(len(z)-4))
			extra = append(z, fh.Extra...)
		}
		rec = rec[:0]
		rec = le.AppendUint32(rec, sigCentral)
		rec = le.AppendUint16(rec, fh.CreatorVersion)
		rec = le.AppendUint16(rec, version)
		rec = le.AppendUint16(rec, fh.Flags)
		rec = le.AppendUint16(rec, fh.Method)
		rec = le.AppendUint16(rec, e.clock)
		rec = le.AppendUint16(rec, e.date)
		rec = le.AppendUint32(rec, fh.CRC32)
		rec = le.AppendUint32(rec, csize)
		rec = le.AppendUint32(rec, usize)
		rec = le.AppendUint16(rec, uint16(len(fh.Name)))
		rec = le.AppendUint16(rec, uint16(len(extra)))
		rec = le.AppendUint16(rec, uint16(len(fh.Comment)))
		rec = append(rec, zeros[:4]...)
		rec = le.AppendUint32(rec, fh.ExternalAttrs)
		rec = le.AppendUint32(rec, offset)
		rec = append(rec, fh.Name...)
		rec = append(rec, extra...)
		rec = append(rec, fh.Comment...)
		w.buf.Write(rec)
		size += int64(len(rec))
	}
	end := start + size
	count := uint64(len(w.members))
	records, size32, offset32 := uint16(count), uint32(size), uint32(start)
	if count >= uint16max || size >= uint32max || start >= uint32max {
		rec = rec[:0]
		rec = le.AppendUint32(rec, sigEnd64)
		rec = le.AppendUint64(rec, end64Len-12)
		rec = le.AppendUint16(rec, zip64Version)
		rec = le.AppendUint16(rec, zip64Version)
		rec = append(rec, zeros[:8]...)
		rec = le.AppendUint64(rec, count)
		rec = le.AppendUint64(rec, count)
		rec = le.AppendUint64(rec, uint64(size))
		rec = le.AppendUint64(rec, uint64(start))
		rec = le.AppendUint32(rec, sigLocator64)
		rec = le.AppendUint32(rec, 0)
		rec = le.AppendUint64(rec, uint64(end))
		rec = le.AppendUint32(rec, 1)
		w.buf.Write(rec)
		end += end64Len + locator64Len
		records, size32, offset32 = uint16max, uint32max, uint32max
	}
	rec = rec[:0]
	rec = le.AppendUint32(rec, sigEnd)
	rec = append(rec, zeros[:4]...)
	rec = le.AppendUint16(rec, records)
	rec = le.AppendUint16(rec, records)
	rec = le.AppendUint32(rec, size32)
	rec = le.AppendUint32(rec, offset32)
	rec = le.AppendUint16(rec, uint16(len(w.comment)))
	rec = append(rec, w.comment...)
	w.buf.Write(rec)
	end += int64(len(rec))
	if err := w.buf.Flush(); err != nil {
		return err
	}
	return w.f.Truncate(end)
}
