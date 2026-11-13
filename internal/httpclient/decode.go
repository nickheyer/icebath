package httpclient

import (
	"bufio"
	"compress/flate"
	"compress/gzip"
	"compress/zlib"
	"errors"
	"io"
	"mime"
	"net/http"
	"strings"

	"github.com/andybalholm/brotli"
	"github.com/klauspost/compress/zstd"
	"golang.org/x/text/encoding"
	"golang.org/x/text/encoding/htmlindex"
	"golang.org/x/text/encoding/ianaindex"
	"golang.org/x/text/transform"
)

// Content codings advertised on requests that name none
const acceptEncoding = "gzip, deflate, br, zstd"

// Replaces an encoded body with a streaming decoder for every coding it understands, outermost first
func decodeResponse(resp *http.Response, x *exchange) {
	if resp.Request != nil && resp.Request.Method == http.MethodHead {
		return
	}
	if resp.StatusCode == http.StatusNoContent || resp.StatusCode == http.StatusNotModified {
		return
	}
	var codings []string
	for _, value := range resp.Header.Values("Content-Encoding") {
		for _, c := range strings.Split(value, ",") {
			if c = strings.ToLower(strings.TrimSpace(c)); c != "" && c != "identity" {
				codings = append(codings, c)
			}
		}
	}
	known := 0
	for i := len(codings) - 1; i >= 0 && supportedCoding(codings[i]); i-- {
		known++
	}
	if known == 0 {
		return
	}
	layers := codings[len(codings)-known:]
	remaining := codings[:len(codings)-known]
	resp.Body = &decodedBody{raw: resp.Body, layers: layers, x: x}
	resp.Header.Del("Content-Encoding")
	if len(remaining) > 0 {
		resp.Header.Set("Content-Encoding", strings.Join(remaining, ", "))
	}
	resp.Header.Del("Content-Length")
	resp.ContentLength = -1
	resp.Uncompressed = true
}

// Reports whether a content coding has a decoder
func supportedCoding(c string) bool {
	switch c {
	case "gzip", "x-gzip", "deflate", "br", "zstd":
		return true
	}
	return false
}

// Body decoded through its codings, the decoders built on first read so an empty body decodes to nothing
type decodedBody struct {
	raw     io.ReadCloser
	layers  []string
	x       *exchange
	reader  io.Reader
	closers []func()
	err     error
}

func (b *decodedBody) Read(p []byte) (int, error) {
	if b.err != nil {
		return 0, b.err
	}
	if b.reader == nil {
		if err := b.open(); err != nil {
			b.err = err
			return 0, err
		}
	}
	n, err := b.reader.Read(p)
	if err != nil && err != io.EOF {
		var known *Error
		if !errors.As(err, &known) {
			err = &Error{Phase: PhaseDecode, Host: b.x.host, Err: err}
		}
		b.err = err
	}
	return n, err
}

// Builds the decoder chain, innermost coding last, after confirming the body carries any bytes
func (b *decodedBody) open() error {
	base := bufio.NewReader(b.raw)
	if _, err := base.Peek(1); err != nil {
		if err == io.EOF {
			b.reader = base
			return nil
		}
		return err
	}
	var r io.Reader = base
	for i := len(b.layers) - 1; i >= 0; i-- {
		next, closer, err := decoder(b.layers[i], r)
		if err != nil {
			var known *Error
			if errors.As(err, &known) {
				return err
			}
			return &Error{Phase: PhaseDecode, Host: b.x.host, Err: err}
		}
		if closer != nil {
			b.closers = append(b.closers, closer)
		}
		r = next
	}
	b.reader = r
	return nil
}

func (b *decodedBody) Close() error {
	for _, c := range b.closers {
		c()
	}
	return b.raw.Close()
}

// Streaming reader undoing one coding, deflate accepted both zlib wrapped and raw
func decoder(coding string, r io.Reader) (io.Reader, func(), error) {
	switch coding {
	case "gzip", "x-gzip":
		zr, err := gzip.NewReader(r)
		if err != nil {
			return nil, nil, err
		}
		return zr, func() { zr.Close() }, nil
	case "deflate":
		br := bufio.NewReader(r)
		head, err := br.Peek(2)
		if err != nil && err != io.EOF {
			return nil, nil, err
		}
		if len(head) == 2 && head[0]&0x0f == 8 && (uint16(head[0])<<8|uint16(head[1]))%31 == 0 {
			zr, err := zlib.NewReader(br)
			if err != nil {
				return nil, nil, err
			}
			return zr, func() { zr.Close() }, nil
		}
		fr := flate.NewReader(br)
		return fr, func() { fr.Close() }, nil
	case "br":
		return brotli.NewReader(r), nil, nil
	case "zstd":
		zr, err := zstd.NewReader(r, zstd.WithDecoderConcurrency(1))
		if err != nil {
			return nil, nil, err
		}
		return zr, zr.Close, nil
	}
	return nil, nil, errors.New("unsupported content coding " + coding)
}

// Body text decoded with the Content-Type charset, UTF-8 with invalid bytes replaced when none is usable
func Text(h http.Header, body []byte) string {
	enc := declaredCharset(h.Get("Content-Type"))
	if enc == nil {
		return strings.ToValidUTF8(string(body), "\uFFFD")
	}
	out, _, err := transform.Bytes(enc.NewDecoder(), body)
	if err != nil {
		return string(out) + "\uFFFD"
	}
	return string(out)
}

// Encoding a media type names in its charset parameter, nil for UTF-8 and for unknown names
func declaredCharset(contentType string) encoding.Encoding {
	if contentType == "" {
		return nil
	}
	_, params, err := mime.ParseMediaType(contentType)
	if err != nil {
		return nil
	}
	name := strings.ToLower(strings.Trim(params["charset"], `"' `))
	switch name {
	case "", "utf-8", "utf8":
		return nil
	}
	if enc, err := ianaindex.IANA.Encoding(name); err == nil && enc != nil {
		return enc
	}
	if enc, err := htmlindex.Get(name); err == nil {
		return enc
	}
	return nil
}
