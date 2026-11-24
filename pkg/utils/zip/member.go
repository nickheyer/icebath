package ziputil

import (
	"archive/zip"
	"bufio"
	"encoding/binary"
	"fmt"
	"hash/crc32"
	"io"
	"io/fs"
	"strings"
)

var zeros [16]byte

// Member being written: data goes through the checksum and the compressor into the file after the local header, which Close fills in with the final checksum and sizes
type fileWriter struct {
	w        *Writer
	fh       *zip.FileHeader
	offset   int64
	data     int64
	reserved int
	date     uint16
	clock    uint16
	dir      bool
	comp     io.WriteCloser
	out      counter
	crc      uint32
	size     int64
	err      error
	done     bool
	result   error
}

// Writer counting the bytes it passes on
type counter struct {
	w io.Writer
	n int64
}

// Passes p on, adding what was written to the count
func (c *counter) Write(p []byte) (int, error) {
	n, err := c.w.Write(p)
	c.n += int64(n)
	return n, err
}

// Finishes the open member, writes the local header of a copy of fh at the end of the archive and returns its writer; a directory is recorded at once
func (w *Writer) begin(fh *zip.FileHeader) (*fileWriter, error) {
	if w.closed {
		return nil, errClosed
	}
	if w.open != nil {
		if err := w.open.Close(); err != nil {
			return nil, err
		}
	}
	own := *fh
	fh = &own
	m := &fileWriter{w: w, fh: fh, offset: w.off, dir: strings.HasSuffix(fh.Name, "/")}
	if m.dir {
		fh.Method = zip.Store
		fh.CRC32, fh.CompressedSize64, fh.UncompressedSize64 = 0, 0, 0
	}
	header, err := m.header(!m.dir && fh.UncompressedSize64 >= zip64Reserve)
	if err != nil {
		return nil, err
	}
	m.data = m.offset + int64(len(header))
	if !m.dir {
		dst := io.NewOffsetWriter(w.f, m.data)
		if w.buf == nil {
			w.buf = bufio.NewWriterSize(dst, bufSize)
		} else {
			w.buf.Reset(dst)
		}
		m.out.w = w.buf
		if m.comp, err = w.compressor(fh.Method, &m.out); err != nil {
			return nil, err
		}
	}
	if _, err := w.f.WriteAt(header, m.offset); err != nil {
		return nil, w.cut(m.offset, err)
	}
	if m.dir {
		m.done = true
		w.members = append(w.members, entry{fh, m.offset, m.date, m.clock})
		w.off = m.data
		return m, nil
	}
	w.open = m
	return m, nil
}

// Local header with the checksum and sizes left zero, once the flags, versions, MS-DOS time and extra field of the member are settled; reserve makes room for 64-bit sizes
func (m *fileWriter) header(reserve bool) ([]byte, error) {
	fh := m.fh
	fh.Flags &^= flagDataDescriptor
	nameValid, nameNeeds := detectUTF8(fh.Name)
	commentValid, commentNeeds := detectUTF8(fh.Comment)
	switch {
	case fh.NonUTF8:
		fh.Flags &^= flagUTF8
	case (nameNeeds || commentNeeds) && nameValid && commentValid:
		fh.Flags |= flagUTF8
	}
	m.date, m.clock = dosDateTime(fh.Modified)
	fh.Extra = appendTimestamp(stripExtra(fh.Extra, extraZip64, extraTimestamp), fh.Modified)
	version := methodVersion(fh.Method)
	if reserve {
		version = max(version, zip64Version)
	}
	fh.ReaderVersion = version
	fh.CreatorVersion = fh.CreatorVersion&0xff00 | version
	if len(fh.Name) > uint16max || len(fh.Comment) > uint16max || len(fh.Extra) > uint16max-zip64ExtraMax {
		return nil, fmt.Errorf("zip member %q has a name or comment longer than %d bytes or an extra field longer than %d: %w", fh.Name, uint16max, uint16max-zip64ExtraMax, fs.ErrInvalid)
	}
	extraLen := len(fh.Extra)
	if reserve {
		extraLen += zip64LocalLen
	}
	le := binary.LittleEndian
	b := make([]byte, 0, localLen+len(fh.Name)+extraLen)
	b = le.AppendUint32(b, sigLocal)
	b = le.AppendUint16(b, version)
	b = le.AppendUint16(b, fh.Flags)
	b = le.AppendUint16(b, fh.Method)
	b = le.AppendUint16(b, m.clock)
	b = le.AppendUint16(b, m.date)
	b = append(b, zeros[:12]...)
	b = le.AppendUint16(b, uint16(len(fh.Name)))
	b = le.AppendUint16(b, uint16(extraLen))
	b = append(b, fh.Name...)
	b = append(b, fh.Extra...)
	if reserve {
		b = le.AppendUint16(b, extraZip64)
		b = le.AppendUint16(b, zip64LocalLen-4)
		m.reserved = len(b)
		b = append(b, zeros[:16]...)
	}
	return b, nil
}

// Compresses p into the member, its checksum and size kept; a directory takes only empty writes
func (m *fileWriter) Write(p []byte) (int, error) {
	switch {
	case m.dir:
		if len(p) > 0 {
			return 0, errDirData
		}
		return 0, nil
	case m.done:
		return 0, errMemberClosed
	case m.err != nil:
		return 0, m.err
	}
	n, err := m.comp.Write(p)
	m.crc = crc32.Update(m.crc, crc32.IEEETable, p[:n])
	m.size += int64(n)
	if err != nil {
		m.err = err
	}
	return n, err
}

// Finishes the member into the archive, or drops it and its bytes when anything fails, the result repeated by later calls
func (m *fileWriter) Close() error {
	if m.done {
		return m.result
	}
	m.done = true
	m.w.open = nil
	err := m.err
	if err == nil {
		err = m.comp.Close()
	}
	if err == nil {
		err = m.w.buf.Flush()
	}
	if err == nil {
		err = m.finish()
	}
	if err != nil {
		err = m.w.cut(m.offset, err)
	}
	m.result = err
	return err
}

// Drops the unfinished member and its bytes after cause made it fail, returning cause
func (m *fileWriter) abort(cause error) error {
	if m.done {
		return cause
	}
	m.done, m.result = true, cause
	m.w.open = nil
	return m.w.cut(m.offset, cause)
}

// Puts the checksum and sizes in the local header, or appends a data descriptor and flags it when 64-bit sizes are needed and the header has no room for them, and records the member
func (m *fileWriter) finish() error {
	fh, w, le := m.fh, m.w, binary.LittleEndian
	fh.CRC32 = m.crc
	fh.CompressedSize64, fh.UncompressedSize64 = uint64(m.out.n), uint64(m.size)
	large := fh.CompressedSize64 >= uint32max || fh.UncompressedSize64 >= uint32max
	end := m.data + m.out.n
	if large && m.reserved == 0 {
		fh.Flags |= flagDataDescriptor
		fh.ReaderVersion = max(fh.ReaderVersion, zip64Version)
		var head [4]byte
		le.PutUint16(head[0:], fh.ReaderVersion)
		le.PutUint16(head[2:], fh.Flags)
		if _, err := w.f.WriteAt(head[:], m.offset+4); err != nil {
			return err
		}
		var desc [descriptorLen]byte
		le.PutUint32(desc[0:], sigDescriptor)
		le.PutUint32(desc[4:], m.crc)
		le.PutUint64(desc[8:], fh.CompressedSize64)
		le.PutUint64(desc[16:], fh.UncompressedSize64)
		if _, err := w.f.WriteAt(desc[:], end); err != nil {
			return err
		}
		end += descriptorLen
	} else {
		csize, usize := uint32(fh.CompressedSize64), uint32(fh.UncompressedSize64)
		if m.reserved > 0 {
			csize, usize = uint32max, uint32max
		}
		var fields [12]byte
		le.PutUint32(fields[0:], m.crc)
		le.PutUint32(fields[4:], csize)
		le.PutUint32(fields[8:], usize)
		if _, err := w.f.WriteAt(fields[:], m.offset+14); err != nil {
			return err
		}
		if m.reserved > 0 {
			var sizes [16]byte
			le.PutUint64(sizes[0:], fh.UncompressedSize64)
			le.PutUint64(sizes[8:], fh.CompressedSize64)
			if _, err := w.f.WriteAt(sizes[:], m.offset+int64(m.reserved)); err != nil {
				return err
			}
		}
	}
	w.members = append(w.members, entry{fh, m.offset, m.date, m.clock})
	w.off = end
	return nil
}
