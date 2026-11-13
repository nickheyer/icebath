package httpclient

import (
	"bufio"
	"bytes"
	"context"
	"crypto/tls"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"strconv"
	"time"

	"golang.org/x/net/proxy"
)

// Stagger between connection attempts to successive resolved addresses
const happyEyeballsDelay = 250 * time.Millisecond

// TCP keepalive so a half open peer errors out instead of hanging a long poll forever
var tcpKeepAlive = net.KeepAliveConfig{Enable: true, Idle: 30 * time.Second, Interval: 10 * time.Second, Count: 3}

// Looks up the addresses a direct connection to a host may dial, satisfied by the standard DNS resolver
type Resolver interface {
	LookupNetIP(ctx context.Context, network, host string) ([]netip.Addr, error)
}

// Opens and secures connections for one transport, direct ones through its resolver and proxy ones through DNS
type dialer struct {
	resolver   Resolver
	connect    time.Duration
	handshake  time.Duration
	write      time.Duration
	preferIPv4 bool
	agent      string
	target     *trust
	proxy      *trust
}

// Context of a dial bounded by the connect deadline
func (d *dialer) bound(ctx context.Context) (context.Context, context.CancelFunc) {
	return within(ctx, d.connect)
}

// Child context that also ends after limit when limit is positive
func within(ctx context.Context, limit time.Duration) (context.Context, context.CancelFunc) {
	if limit > 0 {
		return context.WithTimeout(ctx, limit)
	}
	return context.WithCancel(ctx)
}

// Error of a dial, the connect deadline reported as a timeout when it and not the caller ended the dial
func (d *dialer) failure(parent, bounded context.Context, err error) error {
	if parent.Err() == nil && errors.Is(bounded.Err(), context.DeadlineExceeded) {
		return &deadlineError{after: d.connect}
	}
	return err
}

// Direct connection to a target address through the transport resolver
func (d *dialer) direct(ctx context.Context, _, addr string) (net.Conn, error) {
	markPhase(ctx, PhaseConnect)
	bounded, cancel := d.bound(ctx)
	defer cancel()
	resolver := d.resolver
	if resolver == nil {
		resolver = net.DefaultResolver
	}
	conn, err := d.race(bounded, addr, resolver)
	if err != nil {
		return nil, d.failure(ctx, bounded, err)
	}
	return d.wrap(conn), nil
}

// Connection to a proxy for plain http requests forwarded through it, TLS wrapped for an https proxy
func (d *dialer) forward(ctx context.Context, proxyURL *url.URL) (net.Conn, error) {
	markPhase(ctx, PhaseConnect)
	bounded, cancel := d.bound(ctx)
	defer cancel()
	conn, err := d.proxyConn(bounded, proxyURL)
	if err != nil {
		return nil, &ProxyError{Proxy: proxyLabel(proxyURL), Err: d.failure(ctx, bounded, err)}
	}
	return d.wrap(conn), nil
}

// Connection to a target tunnelled via a proxy of any supported scheme
func (d *dialer) through(ctx context.Context, proxyURL *url.URL, addr string) (net.Conn, error) {
	markPhase(ctx, PhaseConnect)
	bounded, cancel := d.bound(ctx)
	defer cancel()
	var conn net.Conn
	var err error
	switch proxyURL.Scheme {
	case "http", "https":
		conn, err = d.tunnel(bounded, proxyURL, addr)
	case "socks4", "socks4a":
		conn, err = d.socks4(bounded, proxyURL, addr)
	default:
		conn, err = d.socks5(bounded, proxyURL, addr)
	}
	if err != nil {
		var pe *ProxyError
		if errors.As(err, &pe) {
			return nil, err
		}
		return nil, &ProxyError{Proxy: proxyLabel(proxyURL), Err: d.failure(ctx, bounded, err)}
	}
	return d.wrap(conn), nil
}

// TLS session over an open connection verified as the host of addr, the handshake bounded by the TLS deadline
func (d *dialer) secure(ctx context.Context, conn net.Conn, addr string) (net.Conn, error) {
	markPhase(ctx, PhaseTLS)
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		conn.Close()
		return nil, err
	}
	config := d.target.config(host)
	config.NextProtos = []string{"http/1.1"}
	bounded, cancel := within(ctx, d.handshake)
	defer cancel()
	session := tls.Client(conn, config)
	if err := session.HandshakeContext(bounded); err != nil {
		conn.Close()
		if ctx.Err() == nil && errors.Is(bounded.Err(), context.DeadlineExceeded) {
			return nil, &deadlineError{after: d.handshake}
		}
		return nil, d.target.explain(err)
	}
	return session, nil
}

// Connection with every write bounded by the write deadline
func (d *dialer) wrap(conn net.Conn) net.Conn {
	if d.write <= 0 {
		return conn
	}
	return &writeBoundConn{Conn: conn, limit: d.write}
}

// Addresses of a host, IPv4 alone when preferred and the host has any, every family otherwise
func (d *dialer) lookup(ctx context.Context, resolver Resolver, host string) ([]netip.Addr, error) {
	if d.preferIPv4 {
		if addrs, err := resolver.LookupNetIP(ctx, "ip4", host); err == nil && len(addrs) > 0 {
			return addrs, nil
		}
	}
	return resolver.LookupNetIP(ctx, "ip", host)
}

// Staggered connect racing every resolved address across families, the first success winning
func (d *dialer) race(ctx context.Context, addr string, resolver Resolver) (net.Conn, error) {
	host, portText, err := net.SplitHostPort(addr)
	if err != nil {
		return nil, err
	}
	port, err := strconv.ParseUint(portText, 10, 16)
	if err != nil {
		return nil, fmt.Errorf("invalid port %q", portText)
	}
	addrs, err := d.lookup(ctx, resolver, host)
	if err != nil {
		return nil, err
	}
	order := interleave(addrs)
	if len(order) == 0 {
		return nil, &net.DNSError{Err: "no addresses found", Name: host, IsNotFound: true}
	}
	type attempt struct {
		conn net.Conn
		err  error
	}
	attempts, cancel := context.WithCancel(ctx)
	defer cancel()
	results := make(chan attempt, len(order))
	nd := net.Dialer{KeepAliveConfig: tcpKeepAlive}
	next, active := 0, 0
	launch := func() {
		target := netip.AddrPortFrom(order[next], uint16(port)).String()
		next++
		active++
		go func() {
			conn, err := nd.DialContext(attempts, "tcp", target)
			results <- attempt{conn, err}
		}()
	}
	abandon := func() {
		cancel()
		go func(pending int) {
			for ; pending > 0; pending-- {
				if a := <-results; a.conn != nil {
					a.conn.Close()
				}
			}
		}(active)
	}
	launch()
	timer := time.NewTimer(happyEyeballsDelay)
	defer timer.Stop()
	var lastErr error
	for active > 0 {
		select {
		case a := <-results:
			active--
			if a.err == nil {
				abandon()
				return a.conn, nil
			}
			lastErr = a.err
			if active == 0 && next < len(order) {
				launch()
				timer.Reset(happyEyeballsDelay)
			}
		case <-timer.C:
			if next < len(order) {
				launch()
				timer.Reset(happyEyeballsDelay)
			}
		case <-ctx.Done():
			abandon()
			return nil, ctx.Err()
		}
	}
	return nil, lastErr
}

// Addresses deduplicated and alternated between families, starting with the family the resolver put first
func interleave(addrs []netip.Addr) []netip.Addr {
	seen := make(map[netip.Addr]bool, len(addrs))
	var families [2][]netip.Addr
	firstV6 := false
	for _, a := range addrs {
		a = a.Unmap()
		if !a.IsValid() || seen[a] {
			continue
		}
		if len(seen) == 0 {
			firstV6 = a.Is6()
		}
		seen[a] = true
		if a.Is6() == firstV6 {
			families[0] = append(families[0], a)
		} else {
			families[1] = append(families[1], a)
		}
	}
	out := make([]netip.Addr, 0, len(seen))
	for i := 0; len(out) < len(seen); i++ {
		for _, family := range families {
			if i < len(family) {
				out = append(out, family[i])
			}
		}
	}
	return out
}

// Connection to a proxy itself, TLS wrapped for an https proxy and verified against the OS store
func (d *dialer) proxyConn(ctx context.Context, proxyURL *url.URL) (net.Conn, error) {
	conn, err := d.race(ctx, proxyAddr(proxyURL), net.DefaultResolver)
	if err != nil || proxyURL.Scheme != "https" {
		return conn, err
	}
	session := tls.Client(conn, d.proxy.config(proxyURL.Hostname()))
	if err := session.HandshakeContext(ctx); err != nil {
		conn.Close()
		return nil, d.proxy.explain(err)
	}
	return session, nil
}

// Tunnel to a target through an HTTP proxy CONNECT, credentials sent as basic proxy authorization
func (d *dialer) tunnel(ctx context.Context, proxyURL *url.URL, addr string) (net.Conn, error) {
	conn, err := d.proxyConn(ctx, proxyURL)
	if err != nil {
		return nil, err
	}
	req := &http.Request{Method: http.MethodConnect, URL: &url.URL{Opaque: addr}, Host: addr, Header: http.Header{}}
	req.Header.Set("User-Agent", d.agent)
	if user := proxyURL.User; user != nil {
		password, _ := user.Password()
		req.Header.Set("Proxy-Authorization", "Basic "+base64.StdEncoding.EncodeToString([]byte(user.Username()+":"+password)))
	}
	release := guard(ctx, conn)
	reader := bufio.NewReader(&io.LimitedReader{R: conn, N: maxTunnelReply})
	err = req.Write(conn)
	var resp *http.Response
	if err == nil {
		resp, err = http.ReadResponse(reader, req)
	}
	if !release() {
		conn.Close()
		return nil, ctx.Err()
	}
	if err != nil {
		conn.Close()
		return nil, err
	}
	resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		conn.Close()
		return nil, &ProxyError{Proxy: proxyLabel(proxyURL), Status: resp.StatusCode, Err: errors.New(resp.Status)}
	}
	if n := reader.Buffered(); n > 0 {
		early, _ := reader.Peek(n)
		return &bufferedConn{Conn: conn, early: bytes.Clone(early)}, nil
	}
	return conn, nil
}

// Most bytes a proxy may send for its tunnel reply before the reply is abandoned as hostile
const maxTunnelReply = 1 << 20

// Tunnel through a SOCKS5 proxy with the target name resolved on the proxy side
func (d *dialer) socks5(ctx context.Context, proxyURL *url.URL, addr string) (net.Conn, error) {
	var auth *proxy.Auth
	if user := proxyURL.User; user != nil {
		password, _ := user.Password()
		auth = &proxy.Auth{User: user.Username(), Password: password}
	}
	socks, err := proxy.SOCKS5("tcp", proxyAddr(proxyURL), auth, proxyHop{d})
	if err != nil {
		return nil, err
	}
	contextual, ok := socks.(proxy.ContextDialer)
	if !ok {
		return nil, errors.New("SOCKS5 dialer does not accept a context")
	}
	return contextual.DialContext(ctx, "tcp", addr)
}

// SOCKS4 reply codes and their meaning
var socks4Replies = map[byte]string{
	91: "request rejected or failed",
	92: "proxy could not reach the client identd",
	93: "client identd reported a different user",
}

// Tunnel through a SOCKS4 proxy, host names sent in the 4a form so the proxy resolves them
func (d *dialer) socks4(ctx context.Context, proxyURL *url.URL, addr string) (net.Conn, error) {
	host, portText, err := net.SplitHostPort(addr)
	if err != nil {
		return nil, err
	}
	port, err := strconv.ParseUint(portText, 10, 16)
	if err != nil {
		return nil, fmt.Errorf("invalid port %q", portText)
	}
	request := []byte{4, 1, byte(port >> 8), byte(port)}
	ip, ipErr := netip.ParseAddr(host)
	switch {
	case ipErr == nil && ip.Unmap().Is4():
		v4 := ip.Unmap().As4()
		request = append(request, v4[:]...)
	case ipErr == nil:
		return nil, fmt.Errorf("SOCKS4 cannot reach the IPv6 address %s", host)
	default:
		request = append(request, 0, 0, 0, 1)
	}
	if proxyURL.User != nil {
		request = append(request, proxyURL.User.Username()...)
	}
	request = append(request, 0)
	if ipErr != nil {
		request = append(append(request, host...), 0)
	}
	conn, err := d.race(ctx, proxyAddr(proxyURL), net.DefaultResolver)
	if err != nil {
		return nil, err
	}
	release := guard(ctx, conn)
	reply := make([]byte, 8)
	_, err = conn.Write(request)
	if err == nil {
		_, err = io.ReadFull(conn, reply)
	}
	if !release() {
		conn.Close()
		return nil, ctx.Err()
	}
	if err == nil && reply[1] != 90 {
		reason, known := socks4Replies[reply[1]]
		if !known {
			reason = fmt.Sprintf("unknown reply code %d", reply[1])
		}
		err = &ProxyError{Proxy: proxyLabel(proxyURL), Status: int(reply[1]), Err: errors.New(reason)}
	}
	if err != nil {
		conn.Close()
		return nil, err
	}
	return conn, nil
}

// Bounds a proxy handshake by the context, returning a release reporting whether the context stayed live
func guard(ctx context.Context, conn net.Conn) func() bool {
	if deadline, ok := ctx.Deadline(); ok {
		conn.SetDeadline(deadline)
	}
	stop := context.AfterFunc(ctx, func() { conn.SetDeadline(time.Unix(1, 0)) })
	return func() bool {
		live := stop()
		conn.SetDeadline(time.Time{})
		return live && ctx.Err() == nil
	}
}

// Dialer the SOCKS5 client uses to reach the proxy itself
type proxyHop struct{ d *dialer }

func (h proxyHop) Dial(network, addr string) (net.Conn, error) {
	return h.DialContext(context.Background(), network, addr)
}

func (h proxyHop) DialContext(ctx context.Context, _, addr string) (net.Conn, error) {
	return h.d.race(ctx, addr, net.DefaultResolver)
}

// Connection whose first reads drain bytes a proxy sent after its tunnel reply
type bufferedConn struct {
	net.Conn
	early []byte
}

func (c *bufferedConn) Read(p []byte) (int, error) {
	if len(c.early) == 0 {
		return c.Conn.Read(p)
	}
	n := copy(p, c.early)
	c.early = c.early[n:]
	return n, nil
}

// Connection that fails any single write lasting past the limit
type writeBoundConn struct {
	net.Conn
	limit time.Duration
}

func (c *writeBoundConn) Write(p []byte) (int, error) {
	if err := c.Conn.SetWriteDeadline(time.Now().Add(c.limit)); err != nil {
		return 0, err
	}
	n, err := c.Conn.Write(p)
	c.Conn.SetWriteDeadline(time.Time{})
	return n, err
}
