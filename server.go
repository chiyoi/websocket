package websocket

import (
	"errors"
	"fmt"
	"net"
	"net/http"
)

type ServerConfig struct {
	// Protocol contains protocols the server support.
	// May choose one of them for each connect request.
	Protocol []string
	// Extensions contains extensions the server support.
	// for each connection request, server should support all the client extensions.
	Extensions []string
}

// Upgrade takes over an existing http connection and returns a websocket connection
func (srv *ServerConfig) Upgrade(w http.ResponseWriter, r *http.Request) (ws WebSocket, err error) {
	defer func() {
		if err != nil {
			err = fmt.Errorf("websocket: upgrade: %w", err)
		}
	}()
	hi, ok := w.(http.Hijacker)
	if !ok {
		err = errors.New("cannot take over connection")
		return
	}
	conn, _, err := hi.Hijack()
	if err != nil {
		return
	}
	return srv.handshake(conn, r)
}

func (srv *ServerConfig) handshake(conn net.Conn, r *http.Request) (ws WebSocket, err error) {
	defer func() {
		if err != nil {
			err = fmt.Errorf("handshake: %w", err)
		}
	}()
	for k, v := range map[string]string{"Upgrade": "websocket", "Connection": "Upgrade", "Sec-WebSocket-Version": "13"} {
		if got := r.Header.Get(k); got != v {
			err = fmt.Errorf("invalid request header |%s|(got %s, expect %s)", k, got, v)
			return
		}
	}
	for _, k := range []string{"Sec-WebSocket-Key"} {
		if r.Header.Get(k) == "" {
			err = fmt.Errorf("missing request header |%s|", k)
			return
		}
	}

	h := http.Header{}
	h.Set("Upgrade", "websocket")
	h.Set("Connection", "Upgrade")
	h.Set("Sec-WebSocket-Accept", challengeKey(r.Header.Get("Sec-WebSocket-Key")))

	eReq := map[string]bool{}
	for _, e := range r.Header.Values("Sec-WebSocket-Extensions") {
		eReq[e] = true
	}
	for _, e := range srv.Extensions {
		if eReq[e] {
			h.Add("Sec-WebSocket-Extensions", e)
		}
	}

	pResp := map[string]bool{}
	for _, p := range srv.Protocol {
		pResp[p] = true
	}
	for _, p := range r.Header.Values("Sec-WebSocket-Protocol") {
		if pResp[p] {
			h.Set("Sec-WebSocket-Protocol", p)
			break
		}
	}

	resp := &http.Response{
		Status:     fmt.Sprintf("%03d %s", http.StatusSwitchingProtocols, http.StatusText(http.StatusSwitchingProtocols)),
		StatusCode: http.StatusSwitchingProtocols,
		Proto:      "HTTP/1.1",
		ProtoMajor: 1,
		ProtoMinor: 1,
		Header:     h,
		Request:    r,
		TLS:        r.TLS,
	}
	if err = resp.Write(conn); err != nil {
		return
	}

	ws = &webSocket{
		conn:  conn,
		srv:   true,
		state: Open,
		ext:   h.Values("Sec-WebSocket-Extensions"),
		pro:   h.Get("Sec-WebSocket-Protocol"),
	}
	return
}

var defaultServerConfig ServerConfig
var DefaultServerConfig = &defaultServerConfig

func Upgrade(w http.ResponseWriter, r *http.Request) (WebSocket, error) {
	return DefaultServerConfig.Upgrade(w, r)
}
