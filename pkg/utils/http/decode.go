package httputil

import (
	"bufio"
	"bytes"
	"compress/flate"
	"compress/gzip"
	"compress/zlib"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/andybalholm/brotli"
	"github.com/klauspost/compress/zstd"
)

// Body with the Content-Encoding codings the header lists undone from the last to the first: gzip, deflate, br and zstd; decoding stops at a coding it does not know, which leaves that layer and every one beneath it as they are
func DecodeContent(h http.Header, body []byte) ([]byte, error) {
	codings := contentCodings(h)
	for i := len(codings) - 1; i >= 0; i-- {
		if !knownCoding(codings[i]) {
			break
		}
		d := &lazyDecoder{src: &sourceReader{r: bytes.NewReader(body)}, coding: codings[i]}
		out, err := io.ReadAll(d)
		d.close()
		if err != nil {
			return nil, err
		}
		body = out
	}
	return body, nil
}

// Content codings the header lists in order, lowercased, identity left out
func contentCodings(h http.Header) []string {
	var codings []string
	for _, v := range h.Values("Content-Encoding") {
		for c := range strings.SplitSeq(v, ",") {
			if c = strings.ToLower(strings.TrimSpace(c)); c != "" && c != "identity" {
				codings = append(codings, c)
			}
		}
	}
	return codings
}

// Whether a content coding has a decoder here
func knownCoding(coding string) bool {
	switch coding {
	case "gzip", "x-gzip", "deflate", "br", "zstd":
		return true
	}
	return false
}

// Failure undoing one content coding
type decodeError struct {
	coding string
	err    error
}

func (e *decodeError) Error() string { return "decoding " + e.coding + " content: " + e.err.Error() }

func (e *decodeError) Unwrap() error { return e.err }

// Reader keeping the last failure of the stream under a decoder so transport errors pass through undisguised
type sourceReader struct {
	r   io.Reader
	err error
}

func (s *sourceReader) Read(p []byte) (int, error) {
	n, err := s.r.Read(p)
	if err != nil && err != io.EOF {
		s.err = err
	}
	return n, err
}

// Decoder of one coding that opens on its first read, deflate taking zlib framing when the stream starts with it and raw data otherwise
type lazyDecoder struct {
	src    *sourceReader
	coding string
	r      io.Reader
	closer func()
	err    error
}

func (d *lazyDecoder) Read(p []byte) (int, error) {
	if d.r == nil && d.err == nil {
		d.open()
	}
	if d.err != nil {
		return 0, d.err
	}
	n, err := d.r.Read(p)
	return n, d.classify(err)
}

// Failure of the stream underneath as read, every other failure a decodeError naming the coding
func (d *lazyDecoder) classify(err error) error {
	if err == nil || err == io.EOF {
		return err
	}
	var de *decodeError
	if d.src.err != nil && errors.Is(err, d.src.err) || errors.As(err, &de) {
		return err
	}
	return &decodeError{coding: d.coding, err: err}
}

func (d *lazyDecoder) open() {
	br := bufio.NewReader(d.src)
	switch d.coding {
	case "gzip", "x-gzip":
		zr, err := gzip.NewReader(br)
		if err != nil {
			d.err = d.classify(err)
			return
		}
		d.r = zr
	case "deflate":
		if hdr, err := br.Peek(2); err == nil && zlibHeader(hdr[0], hdr[1]) {
			zr, err := zlib.NewReader(br)
			if err != nil {
				d.err = d.classify(err)
				return
			}
			d.r, d.closer = zr, func() { _ = zr.Close() }
			return
		}
		fr := flate.NewReader(br)
		d.r, d.closer = fr, func() { _ = fr.Close() }
	case "br":
		d.r = brotli.NewReader(br)
	case "zstd":
		zr, err := zstd.NewReader(br, zstd.WithDecoderConcurrency(1))
		if err != nil {
			d.err = d.classify(err)
			return
		}
		d.r, d.closer = zr, zr.Close
	default:
		d.err = fmt.Errorf("unsupported content coding %q", d.coding)
	}
}

// Releases what the decoder holds
func (d *lazyDecoder) close() {
	if d.closer != nil {
		d.closer()
		d.closer = nil
	}
}

// Whether two bytes open a zlib stream: deflate method, a window of at most 32K and a valid header check
func zlibHeader(cmf, flg byte) bool {
	return cmf&0x0f == 8 && cmf>>4 <= 7 && (uint16(cmf)<<8|uint16(flg))%31 == 0
}

// Response body reading through the decoders and closing them with the raw body
type decodedBody struct {
	r        io.Reader
	decoders []*lazyDecoder
	raw      io.Closer
}

func (b *decodedBody) Read(p []byte) (int, error) { return b.r.Read(p) }

func (b *decodedBody) Close() error {
	for _, d := range b.decoders {
		d.close()
	}
	return b.raw.Close()
}

// Wraps the body so it reads decoded when every coding the response lists is known, the coding headers removed as the body no longer carries them
func decodeResponse(resp *http.Response) {
	if resp.Body == nil || resp.Body == http.NoBody || resp.ContentLength == 0 {
		return
	}
	codings := contentCodings(resp.Header)
	if len(codings) == 0 {
		return
	}
	for _, c := range codings {
		if !knownCoding(c) {
			return
		}
	}
	var r io.Reader = resp.Body
	decoders := make([]*lazyDecoder, 0, len(codings))
	for i := len(codings) - 1; i >= 0; i-- {
		d := &lazyDecoder{src: &sourceReader{r: r}, coding: codings[i]}
		decoders = append(decoders, d)
		r = d
	}
	resp.Body = &decodedBody{r: r, decoders: decoders, raw: resp.Body}
	resp.Header.Del("Content-Encoding")
	resp.Header.Del("Content-Length")
	resp.ContentLength = -1
	resp.Uncompressed = true
}
