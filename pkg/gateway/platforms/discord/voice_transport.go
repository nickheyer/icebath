package discord

import (
	"crypto/aes"
	"crypto/cipher"
	"encoding/binary"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"sync"
	"time"

	"golang.org/x/crypto/chacha20poly1305"
)

// Transport encryption modes Discord offers, in the order this client prefers them
const (
	modeAES256GCMRTPSize         = "aead_aes256_gcm_rtpsize"
	modeXChaCha20Poly1305RTPSize = "aead_xchacha20_poly1305_rtpsize"
)

var voiceSupportedModes = []string{modeAES256GCMRTPSize, modeXChaCha20Poly1305RTPSize}

// First supported mode the server offers, by this client's preference
func selectVoiceMode(offered []string) (string, error) {
	for _, want := range voiceSupportedModes {
		for _, m := range offered {
			if m == want {
				return m, nil
			}
		}
	}
	return "", fmt.Errorf("IndexError: list index out of range")
}

// AEAD of a transport mode keyed with the session secret, replaced by tests that fake nacl.secret.Aead
var transportAEAD = func(mode string, key []byte) (cipher.AEAD, error) {
	switch mode {
	case modeAES256GCMRTPSize:
		block, err := aes.NewCipher(key)
		if err != nil {
			return nil, err
		}
		return cipher.NewGCM(block)
	case modeXChaCha20Poly1305RTPSize:
		return chacha20poly1305.NewX(key)
	}
	return nil, fmt.Errorf("unsupported voice encryption mode %q", mode)
}

// Nonce of an rtpsize mode, the four byte counter followed by zeros up to the AEAD nonce size
func rtpsizeNonce(aead cipher.AEAD, counter []byte) []byte {
	nonce := make([]byte, aead.NonceSize())
	copy(nonce, counter)
	return nonce
}

// Encrypts one RTP payload with the header as associated data and appends the nonce counter
func encryptRTPSize(aead cipher.AEAD, header, payload []byte, counter uint32) []byte {
	var c [4]byte
	binary.BigEndian.PutUint32(c[:], counter)
	out := append([]byte{}, header...)
	out = aead.Seal(out, rtpsizeNonce(aead, c[:]), payload, header)
	return append(out, c[:]...)
}

// RTP header of one outgoing Opus frame
func rtpHeader(sequence uint16, timestamp, ssrc uint32) []byte {
	h := make([]byte, 12)
	h[0], h[1] = 0x80, 0x78
	binary.BigEndian.PutUint16(h[2:], sequence)
	binary.BigEndian.PutUint32(h[4:], timestamp)
	binary.BigEndian.PutUint32(h[8:], ssrc)
	return h
}

// IP discovery request of an SSRC, type 1 with length 70
func ipDiscoveryRequest(ssrc uint32) []byte {
	p := make([]byte, 74)
	binary.BigEndian.PutUint16(p[0:], 1)
	binary.BigEndian.PutUint16(p[2:], 70)
	binary.BigEndian.PutUint32(p[4:], ssrc)
	return p
}

// External address and port of an IP discovery response
func parseIPDiscovery(recv []byte) (string, int, error) {
	end := -1
	for i := 8; i < len(recv); i++ {
		if recv[i] == 0 {
			end = i
			break
		}
	}
	if end < 0 {
		return "", 0, errors.New("ValueError: subsection not found")
	}
	return string(recv[8:end]), int(binary.BigEndian.Uint16(recv[len(recv)-2:])), nil
}

// UDP reader thread of one voice connection, paused while no listener is registered
type socketReader struct {
	mu        sync.Mutex
	cond      *sync.Cond
	sock      *net.UDPConn
	callbacks []socketListener
	nextID    int
	running   bool
	ended     bool
	idle      bool
}

// Registered packet callback and the handle that removes it
type socketListener struct {
	id int
	fn func([]byte)
}

func newSocketReader() *socketReader {
	r := &socketReader{idle: true}
	r.cond = sync.NewCond(&r.mu)
	go r.run()
	return r
}

// Adds a packet callback, waking a reader that idled for lack of listeners
func (r *socketReader) register(fn func([]byte)) int {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.nextID++
	r.callbacks = append(r.callbacks, socketListener{id: r.nextID, fn: fn})
	if r.idle {
		r.idle = false
		r.running = true
		r.cond.Broadcast()
	}
	return r.nextID
}

// Removes a packet callback, idling the reader when it was the last one
func (r *socketReader) unregister(id int) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for i, l := range r.callbacks {
		if l.id == id {
			r.callbacks = append(r.callbacks[:i:i], r.callbacks[i+1:]...)
			if len(r.callbacks) == 0 && r.running {
				r.idle = true
				r.running = false
			}
			return
		}
	}
}

// Stops reading until resumed
func (r *socketReader) pause() {
	r.mu.Lock()
	r.idle = false
	r.running = false
	r.mu.Unlock()
}

// Resumes reading when listeners exist or when forced
func (r *socketReader) resume(force bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.running {
		return
	}
	if !force && len(r.callbacks) == 0 {
		r.idle = true
		return
	}
	r.idle = false
	r.running = true
	r.cond.Broadcast()
}

// Ends the reader for good
func (r *socketReader) stop() {
	r.mu.Lock()
	r.ended = true
	r.running = true
	r.cond.Broadcast()
	r.mu.Unlock()
}

// Socket the reader polls, swapped on every voice server update
func (r *socketReader) setSocket(s *net.UDPConn) {
	r.mu.Lock()
	r.sock = s
	r.cond.Broadcast()
	r.mu.Unlock()
}

// Reads packets while running and hands each to every callback
func (r *socketReader) run() {
	buf := make([]byte, 2048)
	for {
		r.mu.Lock()
		for !r.ended && (!r.running || r.sock == nil) {
			r.cond.Wait()
		}
		if r.ended {
			r.callbacks = nil
			r.running = false
			r.mu.Unlock()
			return
		}
		sock := r.sock
		r.mu.Unlock()
		sock.SetReadDeadline(time.Now().Add(30 * time.Second))
		n, err := sock.Read(buf)
		if err != nil {
			var ne net.Error
			if errors.As(err, &ne) && ne.Timeout() {
				continue
			}
			slog.Debug(fmt.Sprintf("Error reading from socket in socket reader, this should be safe to ignore: %s", err))
			r.mu.Lock()
			if r.sock == sock {
				r.sock = nil
			}
			r.mu.Unlock()
			continue
		}
		data := append([]byte(nil), buf[:n]...)
		r.mu.Lock()
		cbs := append([]socketListener(nil), r.callbacks...)
		r.mu.Unlock()
		for _, cb := range cbs {
			func() {
				defer func() {
					if p := recover(); p != nil {
						slog.Error(fmt.Sprintf("Error calling socket listener in socket reader: %v", p))
					}
				}()
				cb.fn(data)
			}()
		}
	}
}
