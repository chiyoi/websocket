package websocket

import (
	"bufio"
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"net"
	"net/http"
	urlpkg "net/url"
)

// NetDialer can hold net.Dialer or tls.Dialer.
type NetDialer interface {
	Dial(network string, addr string) (net.Conn, error)
	DialContext(ctx context.Context, network string, addr string) (net.Conn, error)
}

// Dialer contains the config to start a websocket connection.
type Dialer struct {
	// Origin should be contained for a web browser.
	Origin string
	// Protocol contains the sub-protocols to use.
	// server may choose one or none from them.
	Protocol []string
	// Extensions are websocket extensions to use.
	// server should support all of them.
	Extensions []string

	// Header contains original headers in the handshake request.
	Header http.Header

	// NetDialer is the net dialer for the connection,
	// defaults to tls.NetDialer for "wss" scheme and net.NetDialer for "ws".
	NetDialer NetDialer
}

// DialContext connects to the url using the provided context.
// The provided context must be non-nil.
func (d *Dialer) DialContext(ctx context.Context, url string) (ws WebSocket, err error) {
	defer func() {
		if err != nil {
			err = fmt.Errorf("websocket: dial: %w", err)
		}
	}()
	if ctx == nil {
		err = errors.New("nil context")
		return
	}

	u, err := urlpkg.Parse(url)
	if err != nil {
		return
	}
	fillHost(u)

	if err = VerifyURI(u.String()); err != nil {
		return
	}

	nd := d.NetDialer
	if nd == nil {
		if u.Scheme == "wss" {
			nd = new(tls.Dialer)
		} else {
			nd = new(net.Dialer)
		}
	}

	conn, err := nd.DialContext(ctx, "tcp", u.Host)
	if err != nil {
		return
	}

	return d.handshake(conn, u)
}

// Dial connects to the url.
func (d *Dialer) Dial(url string) (ws WebSocket, err error) {
	return d.DialContext(context.Background(), url)
}

func fillHost(u *urlpkg.URL) {
	if u.Scheme == "" {
		u.Scheme = "ws"
	}
	if p, ok := PortMap[u.Scheme]; ok {
		hostname, port := u.Hostname(), u.Port()
		if hostname == "" {
			hostname = "localhost"
		}
		if port == "" {
			port = p
		}
		u.Host = hostname + ":" + port
	}
}

func (d *Dialer) handshake(conn net.Conn, u *urlpkg.URL) (ws WebSocket, err error) {
	defer func() {
		if err != nil {
			err = fmt.Errorf("handshake: %w", err)
		}
	}()
	wsKey, accKey := genWsKeyPair()

	h := d.Header.Clone()
	if h == nil {
		h = http.Header{}
	}
	h.Set("Upgrade", "websocket")
	h.Set("Connection", "Upgrade")
	h.Set("Sec-WebSocket-Key", wsKey)
	h.Set("Sec-WebSocket-Version", "13")
	if d.Origin != "" {
		h.Set("Origin", d.Origin)
	}
	h.Del("Sec-WebSocket-Protocol")
	for _, p := range d.Protocol {
		h.Add("Sec-WebSocket-Protocol", p)
	}
	h.Del("Sec-WebSocket-Extensions")
	for _, e := range d.Extensions {
		h.Add("Sec-WebSocket-Extensions", e)
	}

	req := &http.Request{
		Method:     "GET",
		URL:        u,
		Proto:      "HTTP/1.1",
		ProtoMajor: 1,
		ProtoMinor: 1,
		Host:       u.Host,
		Header:     h,
	}
	if err = req.Write(conn); err != nil {
		return
	}

	resp, err := http.ReadResponse(bufio.NewReader(conn), req)
	if err != nil || resp.StatusCode != http.StatusSwitchingProtocols {
		if err == nil {
			err = fmt.Errorf("invalid response status(got `%s`, expect `%03d %s`)", resp.Status, http.StatusSwitchingProtocols, http.StatusText(http.StatusSwitchingProtocols))
		}
		return
	}

	for k, v := range map[string]string{"Upgrade": "websocket", "Connection": "Upgrade", "Sec-WebSocket-Accept": accKey} {
		if got := resp.Header.Get(k); got != v {
			err = fmt.Errorf("invalid response header |%s|(got %s, expect %s)", k, got, v)
			return
		}
	}

	eReq := make(map[string]bool)
	for _, e := range d.Extensions {
		eReq[e] = true
	}
	for _, e := range resp.Header.Values("Sec-WebSocket-Extensions") {
		if !eReq[e] {
			err = fmt.Errorf("response with unsupported extension(%s)", e)
			return
		}
	}

	pResp := resp.Header.Get("Sec-WebSocket-Protocol")
	if !func() bool {
		if pResp == "" {
			return req.Header.Get("Sec-WebSocket-Protocol") == ""
		}
		for _, p := range req.Header.Values("Sec-WebSocket-Protocol") {
			if p == pResp {
				return true
			}
		}
		return false
	}() {
		err = fmt.Errorf("response protocol not supported(%s)", pResp)
		return
	}

	ws = &webSocket{
		conn:  conn,
		state: Open,
		ext:   resp.Header.Values("Sec-WebSocket-Extensions"),
		pro:   resp.Header.Get("Sec-WebSocket-Protocol"),
	}
	return
}

var DefaultDialer = &Dialer{}

// Dial connects to the url using DefaultDialer.
func Dial(url string) (ws WebSocket, err error) {
	return DefaultDialer.Dial(url)
}
