package botdesktop

import (
	"bytes"
	"cmp"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"image"
	"image/jpeg"
	"io"
	"math"
	"net"
	"os"
	"strconv"
	"strings"
	"syscall"

	"golang.org/x/image/draw"

	"github.com/nickheyer/icebath/internal/config"
	"github.com/nickheyer/icebath/internal/terminal"
)

// Box a picture of the screen fits in, and its JPEG quality
const (
	ThumbWidth   = 960
	ThumbHeight  = 600
	ThumbQuality = 72
)

// JPEG of the running screen shrunk into the box, nil when no screen is up; it only reads, so neither the lease nor a person in control is
// disturbed
func (d *Desktop) Thumbnail(ctx context.Context, cfg *config.Config, width, height, quality int) ([]byte, error) {
	published, err := d.Published(ctx, cfg)
	if err != nil {
		return nil, err
	}
	display := published["DISPLAY"]
	if display == "" {
		return nil, nil
	}
	sandboxed, err := d.sandboxRunning(ctx, cfg)
	if err != nil {
		return nil, err
	}
	if sandboxed {
		var shot []byte
		err := d.owned(ctx, cfg, func(env terminal.Environment) error {
			if env != nil {
				shot = d.sandboxGrab(env, published, width, height, quality)
			}
			return nil
		})
		return shot, err
	}
	if _, ok := live(cfg); !ok {
		return nil, nil
	}
	frame, err := grabX11(display, cmp.Or(published["XAUTHORITY"], xauthority()))
	if err != nil {
		return nil, err
	}
	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, shrink(frame, width, height), &jpeg.Options{Quality: quality}); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// Frame scaled down to fit the box with its aspect kept, the frame itself when it fits already
func shrink(frame *image.NRGBA, width, height int) image.Image {
	w, h := frame.Rect.Dx(), frame.Rect.Dy()
	if w <= width && h <= height {
		return frame
	}
	f := min(float64(width)/float64(w), float64(height)/float64(h))
	out := image.NewRGBA(image.Rect(0, 0, max(int(math.Round(float64(w)*f)), 1), max(int(math.Round(float64(h)*f)), 1)))
	draw.CatmullRom.Scale(out, out.Rect, frame, frame.Rect, draw.Src, nil)
	return out
}

// Connection failures of the X client library, carried in the error text
const (
	xConnError         = 1
	xConnParse         = 5
	xConnInvalidScreen = 6
)

// Xauthority address families
const (
	familyInternet  = 0
	familyInternet6 = 6
	familyLocal     = 256
	familyWild      = 65535
)

// An X display name read apart, the protocol set only when the name carried one
type xDisplay struct {
	host, protocol string
	hasProtocol    bool
	display        int
	screen         int
}

// Leading decimal digits of text as strtoul reads them, false when there are none
func leadingNumber(text string) (int, string, bool) {
	i := 0
	for i < len(text) && (text[i] == ' ' || text[i] >= '\t' && text[i] <= '\r') {
		i++
	}
	if i < len(text) && (text[i] == '+' || text[i] == '-') {
		i++
	}
	start := i
	for i < len(text) && text[i] >= '0' && text[i] <= '9' {
		i++
	}
	if i == start {
		return 0, text, false
	}
	n, err := strconv.ParseUint(text[start:i], 10, 63)
	if err != nil {
		n = math.MaxInt64
	}
	return int(n), text[i:], true
}

// Display name that is a path to a unix socket, a screen number after it optional
func socketDisplay(name string) (xDisplay, bool) {
	path, screen := name, 0
	if _, err := os.Stat(path); err != nil {
		if !errors.Is(err, syscall.ENOENT) && !errors.Is(err, syscall.ENOTDIR) {
			return xDisplay{}, false
		}
		dot := strings.LastIndexByte(path, '.')
		if dot < 0 || dot+1 >= len(path) || path[dot+1] < '1' || path[dot+1] > '9' {
			return xDisplay{}, false
		}
		n, err := strconv.ParseUint(path[dot+1:], 10, 31)
		if err != nil {
			return xDisplay{}, false
		}
		if path = path[:dot]; func() bool { _, err := os.Stat(path); return err != nil }() {
			return xDisplay{}, false
		}
		screen = int(n)
	}
	return xDisplay{host: path, protocol: "unix", hasProtocol: true, screen: screen}, true
}

// Display name read into protocol, host, display and screen as the X client library reads it
func parseDisplay(name string) (xDisplay, bool) {
	if name == "" {
		return xDisplay{}, false
	}
	if d, ok := socketDisplay(name); ok {
		return d, true
	}
	var d xDisplay
	if slash := strings.LastIndexByte(name, '/'); slash >= 0 {
		d.protocol, d.hasProtocol, name = name[:slash], true, name[slash+1:]
	}
	colon := strings.LastIndexByte(name, ':')
	if colon < 0 {
		return xDisplay{}, false
	}
	d.host = name[:colon]
	display, rest, ok := leadingNumber(name[colon+1:])
	if !ok {
		return xDisplay{}, false
	}
	d.display = display
	if rest != "" {
		if rest[0] != '.' {
			return xDisplay{}, false
		}
		screen, tail, ok := leadingNumber(rest[1:])
		if !ok || tail != "" {
			return xDisplay{}, false
		}
		d.screen = screen
	}
	return d, true
}

// Socket to the X server of the display, unix first for a local name and TCP for a named host
func dialDisplay(d xDisplay) (net.Conn, error) {
	if d.hasProtocol && d.protocol == "unix" && strings.HasPrefix(d.host, "/") {
		return net.Dial("unix", d.host)
	}
	tcp := func() (net.Conn, error) {
		return net.Dial("tcp", net.JoinHostPort(d.host, strconv.Itoa(6000+d.display)))
	}
	if (!d.hasProtocol || d.protocol != "unix") && d.host != "" && d.host != "unix" {
		return tcp()
	}
	file := "/tmp/.X11-unix/X" + strconv.Itoa(d.display)
	conn, err := net.Dial("unix", "@"+file)
	if err == nil {
		return conn, nil
	}
	if !errors.Is(err, syscall.ENOENT) && !errors.Is(err, syscall.ECONNREFUSED) {
		return nil, err
	}
	conn, err = net.Dial("unix", file)
	if err != nil && !d.hasProtocol && d.host == "" {
		return tcp()
	}
	return conn, err
}

func xauthField(r io.Reader) ([]byte, error) {
	var n [2]byte
	if _, err := io.ReadFull(r, n[:]); err != nil {
		return nil, err
	}
	out := make([]byte, binary.BigEndian.Uint16(n[:]))
	_, err := io.ReadFull(r, out)
	return out, err
}

// MIT magic cookie the Xauthority file holds for the peer of the socket, false when no entry matches
func xauthCookie(conn net.Conn, display int, file string) ([]byte, bool) {
	family := familyLocal
	var address []byte
	switch peer := conn.RemoteAddr().(type) {
	case *net.TCPAddr:
		if ip4 := peer.IP.To4(); ip4 != nil {
			address = ip4
			if !ip4.Equal(net.IPv4(127, 0, 0, 1)) {
				family = familyInternet
			}
		} else {
			address = peer.IP.To16()
			if !peer.IP.IsLoopback() {
				family = familyInternet6
			}
		}
	case *net.UnixAddr:
	default:
		return nil, false
	}
	if family == familyLocal {
		hostname, err := os.Hostname()
		if err != nil {
			return nil, false
		}
		address = []byte(hostname)
	}
	if file == "" {
		return nil, false
	}
	f, err := os.Open(file)
	if err != nil {
		return nil, false
	}
	defer f.Close()
	number := strconv.Itoa(display)
	for {
		var fam [2]byte
		if _, err := io.ReadFull(f, fam[:]); err != nil {
			return nil, false
		}
		entryFamily := int(binary.BigEndian.Uint16(fam[:]))
		var fields [4][]byte
		for i := range fields {
			if fields[i], err = xauthField(f); err != nil {
				return nil, false
			}
		}
		familyOK := entryFamily == familyWild || entryFamily == family && string(fields[0]) == string(address)
		numberOK := len(fields[1]) == 0 || string(fields[1]) == number
		if familyOK && numberOK && string(fields[2]) == "MIT-MAGIC-COOKIE-1" {
			return fields[3], true
		}
	}
}

// Xauthority file of this process: XAUTHORITY, else the one in the home folder
func xauthority() string {
	if name := os.Getenv("XAUTHORITY"); name != "" {
		return name
	}
	if home := os.Getenv("HOME"); home != "" {
		return home + "/.Xauthority"
	}
	return ""
}

func pad4(b []byte) []byte { return append(b, make([]byte, (4-len(b)%4)%4)...) }

// Root window and size in pixels of the screen of a setup reply
func setupScreen(setup []byte, screen int) (root uint32, width, height int, ok bool) {
	if len(setup) < 40 {
		return 0, 0, 0, false
	}
	le := binary.LittleEndian
	vendor, roots, formats := int(le.Uint16(setup[24:])), int(setup[28]), int(setup[29])
	off := 40 + (vendor+3)/4*4 + 8*formats
	for i := range roots {
		if off+40 > len(setup) {
			return 0, 0, 0, false
		}
		if i == screen {
			return le.Uint32(setup[off:]), int(le.Uint16(setup[off+20:])), int(le.Uint16(setup[off+22:])), true
		}
		depths := int(setup[off+39])
		off += 40
		for range depths {
			if off+8 > len(setup) {
				return 0, 0, 0, false
			}
			off += 8 + 24*int(le.Uint16(setup[off+2:]))
		}
	}
	return 0, 0, 0, false
}

// Whole root window of an X display, authorized by the cookie its Xauthority file holds
func grabX11(display, xauth string) (*image.NRGBA, error) {
	failed := func(code int) error { return fmt.Errorf("X connection failed: error %d", code) }
	d, ok := parseDisplay(display)
	if !ok {
		return nil, failed(xConnParse)
	}
	conn, err := dialDisplay(d)
	if err != nil {
		return nil, failed(xConnError)
	}
	defer conn.Close()
	le := binary.LittleEndian
	var authName, authData []byte
	if cookie, found := xauthCookie(conn, d.display, xauth); found {
		authName, authData = []byte("MIT-MAGIC-COOKIE-1"), cookie
	}
	req := make([]byte, 12)
	req[0] = 'l'
	le.PutUint16(req[2:], 11)
	le.PutUint16(req[6:], uint16(len(authName)))
	le.PutUint16(req[8:], uint16(len(authData)))
	req = append(req, pad4(authName)...)
	req = append(req, pad4(authData)...)
	if _, err := conn.Write(req); err != nil {
		return nil, failed(xConnError)
	}
	head := make([]byte, 8)
	if _, err := io.ReadFull(conn, head); err != nil {
		return nil, failed(xConnError)
	}
	body := make([]byte, 4*int(le.Uint16(head[6:])))
	if _, err := io.ReadFull(conn, body); err != nil || head[0] != 1 {
		return nil, failed(xConnError)
	}
	setup := append(head, body...)
	if len(setup) < 40 || d.screen >= int(setup[28]) {
		return nil, failed(xConnInvalidScreen)
	}
	rootWindow, width, height, ok := setupScreen(setup, d.screen)
	if !ok || rootWindow == 0 {
		return nil, errors.New("X screen not found")
	}
	get := make([]byte, 20)
	get[0], get[1] = 73, 2
	le.PutUint16(get[2:], 5)
	le.PutUint32(get[4:], rootWindow)
	le.PutUint16(get[12:], uint16(width))
	le.PutUint16(get[14:], uint16(height))
	le.PutUint32(get[16:], 0x00ffffff)
	if _, err := conn.Write(get); err != nil {
		return nil, failed(xConnError)
	}
	reply := make([]byte, 32)
	for {
		if _, err := io.ReadFull(conn, reply); err != nil {
			return nil, failed(xConnError)
		}
		if reply[0] == 0 {
			return nil, fmt.Errorf("X get_image failed: error %d (%d, %d, %d)", reply[1], reply[10], le.Uint16(reply[8:]), le.Uint32(reply[4:]))
		}
		if reply[0] == 1 {
			break
		}
		if reply[0]&0x7f == 35 {
			if _, err := io.CopyN(io.Discard, conn, 4*int64(le.Uint32(reply[4:]))); err != nil {
				return nil, failed(xConnError)
			}
		}
	}
	data := make([]byte, 4*int(le.Uint32(reply[4:])))
	if _, err := io.ReadFull(conn, data); err != nil {
		return nil, failed(xConnError)
	}
	if reply[1] != 24 {
		return nil, fmt.Errorf("unsupported bit depth: %d", reply[1])
	}
	if len(data) < width*4*height {
		return nil, errors.New("not enough image data")
	}
	img := image.NewNRGBA(image.Rect(0, 0, width, height))
	for i := range width * height {
		img.Pix[4*i], img.Pix[4*i+1], img.Pix[4*i+2], img.Pix[4*i+3] = data[4*i+2], data[4*i+1], data[4*i], 0xff
	}
	return img, nil
}
