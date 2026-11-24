package ziputil

import (
	"compress/bzip2"
	"errors"
	"io"
	"sync"

	"github.com/klauspost/compress/flate"
	"github.com/klauspost/compress/zstd"
	"github.com/ulikunitz/xz"
)

var (
	inflaters  sync.Pool
	zstdDecode = zstd.ZipDecompressor()
)

// Decoded data of one member: codec failures match ErrCorrupt, and a stream cut short, however the codec words it, ends as a clean end so that the size and checksum checks of archive/zip judge what was decoded
type codecReader struct {
	r    io.Reader
	done func() error
	err  error
}

// Decoded bytes, the first failure or end repeated by every later read
func (c *codecReader) Read(p []byte) (int, error) {
	if c.err != nil {
		return 0, c.err
	}
	n, err := c.r.Read(p)
	if err != nil {
		if errors.Is(err, io.ErrUnexpectedEOF) || errors.Is(err, io.EOF) {
			err = io.EOF
		}
		c.err = corrupt(err)
	}
	return n, c.err
}

// Releases the codec once, later reads failing
func (c *codecReader) Close() error {
	c.err = errMemberClosed
	done := c.done
	c.done = nil
	if done == nil {
		return nil
	}
	return done()
}

// Reader failing with err, for a codec that could not start
func failed(err error) io.ReadCloser { return &codecReader{r: errorReader{err}} }

// Reader whose every read fails with err
type errorReader struct{ err error }

// Nothing read, err returned
func (e errorReader) Read([]byte) (int, error) { return 0, e.err }

// Deflate decoder of a member, taken from a pool and given back on close; the decoder's own close result only repeats what reading reported
func inflate(src io.Reader) io.ReadCloser {
	if fr, ok := inflaters.Get().(io.ReadCloser); ok {
		if rs, ok := fr.(flate.Resetter); ok && rs.Reset(src, nil) == nil {
			return pooledInflater(fr)
		}
	}
	return pooledInflater(flate.NewReader(src))
}

func pooledInflater(fr io.ReadCloser) io.ReadCloser {
	return &codecReader{r: fr, done: func() error {
		inflaters.Put(fr)
		return nil
	}}
}

func unbzip2(src io.Reader) io.ReadCloser { return &codecReader{r: bzip2.NewReader(src)} }

func unzstd(src io.Reader) io.ReadCloser {
	rc := zstdDecode(src)
	return &codecReader{r: rc, done: rc.Close}
}

func unxz(src io.Reader) io.ReadCloser {
	r, err := xz.NewReader(src)
	if err != nil {
		return failed(err)
	}
	return &codecReader{r: r}
}
