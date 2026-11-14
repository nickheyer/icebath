package pypil

import (
	"encoding/binary"
	"errors"
	"fmt"
	"image"
	"io"
	"net"
	"os"
	"strconv"
	"strings"
	"syscall"
)

// libxcb connection error codes surfaced in the OSError text
const (
	xcbConnError         = 1
	xcbConnParseErr      = 5
	xcbConnInvalidScreen = 6
)

// Xauthority address families
const (
	xauFamilyInternet  = 0
	xauFamilyInternet6 = 6
	xauFamilyLocal     = 256
	xauFamilyWild      = 65535
)

// Parsed X display name, the protocol set only when the name carried one
type xDisplay struct {
	host, protocol string
	hasProtocol    bool
	display        int
	screen         int
}

// Leading base ten digits of text as strtoul reads them, ok false when there are none
func strtoul10(text string) (int, string, bool) {
	i := 0
	for i < len(text) && (text[i] == ' ' || (text[i] >= '\t' && text[i] <= '\r')) {
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
		n = 1<<63 - 1
	}
	return int(n), text[i:], true
}

// Display name that is a path to a unix socket, with an optional trailing screen number
func parseDisplaySocketPath(name string) (xDisplay, bool) {
	path := name
	screen := 0
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
		path = path[:dot]
		if _, err := os.Stat(path); err != nil {
			return xDisplay{}, false
		}
		screen = int(n)
	}
	return xDisplay{host: path, protocol: "unix", hasProtocol: true, screen: screen}, true
}

// Display name split into protocol, host, display and screen the way libxcb parses it
func parseXDisplay(name string) (xDisplay, bool) {
	if name == "" {
		name = os.Getenv("DISPLAY")
	}
	if name == "" {
		return xDisplay{}, false
	}
	if d, ok := parseDisplaySocketPath(name); ok {
		return d, true
	}
	var d xDisplay
	if slash := strings.LastIndexByte(name, '/'); slash >= 0 {
		d.protocol, d.hasProtocol = name[:slash], true
		name = name[slash+1:]
	}
	colon := strings.LastIndexByte(name, ':')
	if colon < 0 {
		return xDisplay{}, false
	}
	d.host = name[:colon]
	display, rest, ok := strtoul10(name[colon+1:])
	if !ok {
		return xDisplay{}, false
	}
	d.display = display
	if rest != "" {
		if rest[0] != '.' {
			return xDisplay{}, false
		}
		screen, tail, ok := strtoul10(rest[1:])
		if !ok || tail != "" {
			return xDisplay{}, false
		}
		d.screen = screen
	}
	return d, true
}

// Socket to the X server of a parsed display, unix first for a local name and TCP for a named host
func openXDisplay(d xDisplay) (net.Conn, error) {
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

// One length prefixed field of an Xauthority entry
func readXauField(r io.Reader) ([]byte, error) {
	var n [2]byte
	if _, err := io.ReadFull(r, n[:]); err != nil {
		return nil, err
	}
	out := make([]byte, binary.BigEndian.Uint16(n[:]))
	if _, err := io.ReadFull(r, out); err != nil {
		return nil, err
	}
	return out, nil
}

// MIT magic cookie for the peer of the socket from the Xauthority file, ok false when no entry matches
func xauthCookie(conn net.Conn, display int, xauthFile string) ([]byte, bool) {
	family := xauFamilyLocal
	var address []byte
	switch peer := conn.RemoteAddr().(type) {
	case *net.TCPAddr:
		if ip4 := peer.IP.To4(); ip4 != nil {
			address = ip4
			if !ip4.Equal(net.IPv4(127, 0, 0, 1)) {
				family = xauFamilyInternet
			}
		} else {
			address = peer.IP.To16()
			if !peer.IP.IsLoopback() {
				family = xauFamilyInternet6
			}
		}
	case *net.UnixAddr:
	default:
		return nil, false
	}
	if family == xauFamilyLocal {
		hostname, err := os.Hostname()
		if err != nil {
			return nil, false
		}
		address = []byte(hostname)
	}
	if xauthFile == "" {
		return nil, false
	}
	f, err := os.Open(xauthFile)
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
			if fields[i], err = readXauField(f); err != nil {
				return nil, false
			}
		}
		entryAddress, entryNumber, entryName, entryData := fields[0], fields[1], fields[2], fields[3]
		familyOK := entryFamily == xauFamilyWild || (entryFamily == family && string(entryAddress) == string(address))
		numberOK := len(entryNumber) == 0 || string(entryNumber) == number
		if familyOK && numberOK && string(entryName) == "MIT-MAGIC-COOKIE-1" {
			return entryData, true
		}
	}
}

// Xauthority file libXau reads, the env override first and the home dotfile second
func XauFileName() string {
	if name := os.Getenv("XAUTHORITY"); name != "" {
		return name
	}
	if home := os.Getenv("HOME"); home != "" {
		return home + "/.Xauthority"
	}
	return ""
}

// Bytes padded with zeros to a multiple of four
func xPad4(b []byte) []byte {
	return append(b, make([]byte, (4-len(b)%4)%4)...)
}

// Root window id and pixel size of the numbered screen in a successful setup reply
func xSetupScreen(setup []byte, screen int) (root uint32, width, height int, ok bool) {
	if len(setup) < 40 {
		return 0, 0, 0, false
	}
	le := binary.LittleEndian
	vendorLen := int(le.Uint16(setup[24:]))
	roots := int(setup[28])
	formats := int(setup[29])
	off := 40 + (vendorLen+3)/4*4 + 8*formats
	for i := 0; i < roots; i++ {
		if off+40 > len(setup) {
			return 0, 0, 0, false
		}
		if i == screen {
			return le.Uint32(setup[off:]), int(le.Uint16(setup[off+20:])), int(le.Uint16(setup[off+22:])), true
		}
		depths := int(setup[off+39])
		off += 40
		for j := 0; j < depths; j++ {
			if off+8 > len(setup) {
				return 0, 0, 0, false
			}
			off += 8 + 24*int(le.Uint16(setup[off+2:]))
		}
	}
	return 0, 0, 0, false
}

// Full RGB frame of the root window of an X display, the OSError texts Pillow raises on failure
func GrabX11(xdisplay, xauthFile string) (*image.NRGBA, error) {
	connFailed := func(code int) error { return fmt.Errorf("X connection failed: error %d", code) }
	d, ok := parseXDisplay(xdisplay)
	if !ok {
		return nil, connFailed(xcbConnParseErr)
	}
	conn, err := openXDisplay(d)
	if err != nil {
		return nil, connFailed(xcbConnError)
	}
	defer conn.Close()
	le := binary.LittleEndian
	var authName, authData []byte
	if cookie, found := xauthCookie(conn, d.display, xauthFile); found {
		authName, authData = []byte("MIT-MAGIC-COOKIE-1"), cookie
	}
	req := make([]byte, 12)
	req[0] = 'l'
	le.PutUint16(req[2:], 11)
	le.PutUint16(req[6:], uint16(len(authName)))
	le.PutUint16(req[8:], uint16(len(authData)))
	req = append(req, xPad4(append([]byte{}, authName...))...)
	req = append(req, xPad4(append([]byte{}, authData...))...)
	if _, err := conn.Write(req); err != nil {
		return nil, connFailed(xcbConnError)
	}
	head := make([]byte, 8)
	if _, err := io.ReadFull(conn, head); err != nil {
		return nil, connFailed(xcbConnError)
	}
	body := make([]byte, 4*int(le.Uint16(head[6:])))
	if _, err := io.ReadFull(conn, body); err != nil {
		return nil, connFailed(xcbConnError)
	}
	if head[0] != 1 {
		return nil, connFailed(xcbConnError)
	}
	setup := append(head, body...)
	if len(setup) < 40 || d.screen >= int(setup[28]) {
		return nil, connFailed(xcbConnInvalidScreen)
	}
	root, width, height, ok := xSetupScreen(setup, d.screen)
	if !ok || root == 0 {
		return nil, errors.New("X screen not found")
	}
	get := make([]byte, 20)
	get[0], get[1] = 73, 2
	le.PutUint16(get[2:], 5)
	le.PutUint32(get[4:], root)
	le.PutUint16(get[12:], uint16(width))
	le.PutUint16(get[14:], uint16(height))
	le.PutUint32(get[16:], 0x00ffffff)
	if _, err := conn.Write(get); err != nil {
		return nil, connFailed(xcbConnError)
	}
	reply := make([]byte, 32)
	for {
		if _, err := io.ReadFull(conn, reply); err != nil {
			return nil, connFailed(xcbConnError)
		}
		if reply[0] == 0 {
			return nil, fmt.Errorf("X get_image failed: error %d (%d, %d, %d)", reply[1], reply[10], le.Uint16(reply[8:]), le.Uint32(reply[4:]))
		}
		if reply[0] == 1 {
			break
		}
		if reply[0]&0x7f == 35 {
			if _, err := io.CopyN(io.Discard, conn, 4*int64(le.Uint32(reply[4:]))); err != nil {
				return nil, connFailed(xcbConnError)
			}
		}
	}
	data := make([]byte, 4*int(le.Uint32(reply[4:])))
	if _, err := io.ReadFull(conn, data); err != nil {
		return nil, connFailed(xcbConnError)
	}
	if reply[1] != 24 {
		return nil, fmt.Errorf("unsupported bit depth: %d", reply[1])
	}
	if len(data) < width*4*height {
		return nil, errors.New("not enough image data")
	}
	img := image.NewNRGBA(image.Rect(0, 0, width, height))
	for i := 0; i < width*height; i++ {
		img.Pix[4*i], img.Pix[4*i+1], img.Pix[4*i+2], img.Pix[4*i+3] = data[4*i+2], data[4*i+1], data[4*i], 0xff
	}
	return img, nil
}
