package websocket

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha1"
	"encoding/base64"
	"errors"
	"fmt"
	"math"
	"net"
	"net/url"
	"sync"
	"sync/atomic"
	"time"
	"unicode/utf8"
)

// WebSocket is a websocket connection.
type WebSocket interface {
	// State returns the state of the websocket connection.
	State() ConnectionState
	// Extensions returns extensions used by the websocket connection.
	Extensions() []string
	// Protocol returns the sub-protocol used by the websocket connection.
	Protocol() string

	// RecvCtx waits for a websocket frame, parses it and returns the frame body.
	// Frames with OpPing will be responded with a frame with OpPong.
	// Frames with OpConnectionClose will be responded and packed as ConnectionCloseError.
	RecvCtx(ctx context.Context) (data []byte, meta Metadata, err error)
	// Recv calls WebSocket.RecvCtx with context.Background(), while discards the metadata of received frame.
	Recv() (data []byte, err error)

	// SendCtx sends a frame to WebSocket
	SendCtx(ctx context.Context, op Opcode, data []byte) (err error)
	// Send calls WebSocket.SendCtx to send a frame with OpBinaryFrame with context.Background().
	Send(data []byte) (err error)
	// SendText calls WebSocket.SendCtx to send a frame with OpTextFrame with context.Background().
	SendText(txt string) (err error)
	// Ping sends a frame with OpPing to WebSocket and waits for the next frame, typically a frame with OpPong.
	Ping() (resp []byte, err error)

	// Close sends close frame, waits for the response and closes the underlying
	// net.Conn. It does nothing if WebSocket is already closed or is closing.
	Close() (err error)
}

type Opcode uint8

const (
	OpContinuationFrame Opcode = 0x0
	OpTextFrame         Opcode = 0x1
	OpBinaryFrame       Opcode = 0x2

	OpConnectionClose Opcode = 0x8
	OpPing            Opcode = 0x9
	OpPong            Opcode = 0xa
)

func (op Opcode) String() string {
	switch op {
	case OpContinuationFrame:
		return "continuation frame"
	case OpTextFrame:
		return "text frame"
	case OpBinaryFrame:
		return "binary frame"
	case OpConnectionClose:
		return "connection"
	case OpPing:
		return "ping"
	case OpPong:
		return "pong"
	default:
		return "invalid operation code"
	}
}

type Metadata struct {
	Fin        bool
	Rsv        [3]bool
	Op         Opcode
	Masked     bool
	PayloadLen int64
	Mask       []byte
}

type CloseCode uint16

const (
	NormalClosure           CloseCode = 1000
	GoingAway               CloseCode = 1001
	ProtocolError           CloseCode = 1002
	UnsupportedData         CloseCode = 1003
	NoStatusReceived        CloseCode = 1005
	AbnormalClosure         CloseCode = 1006
	InvalidFramePayloadData CloseCode = 1007
	PolicyViolation         CloseCode = 1008
	MessageTooBig           CloseCode = 1009
	MandatoryExtension      CloseCode = 1010
	InternalServerError     CloseCode = 1011
	TLSHandshake            CloseCode = 1015
)

func (c CloseCode) String() string {
	switch c {
	case NormalClosure:
		return "1000 normal closure"
	case GoingAway:
		return "1001 going away"
	case ProtocolError:
		return "1002 protocol error"
	case UnsupportedData:
		return "1003 unsupported msg"
	case NoStatusReceived:
		return "1005 no status received"
	case AbnormalClosure:
		return "1006 abnormal closure"
	case InvalidFramePayloadData:
		return "1007 invalid frame payload msg"
	case PolicyViolation:
		return "1008 policy violation"
	case MessageTooBig:
		return "1009 message too big"
	case MandatoryExtension:
		return "1010 mandatory extension"
	case InternalServerError:
		return "1011 internal server error"
	case TLSHandshake:
		return "1015 tls handshake"
	default:
		return "invalid close code"
	}
}

var PortMap = map[string]string{
	"ws":  "80",
	"wss": "443",
}

type ConnectionCloseError interface {
	net.Error
	Code() CloseCode
	Msg() any
}

type connectionCloseError struct {
	code CloseCode
	msg  any
}

var _ ConnectionCloseError = (*connectionCloseError)(nil)

func (e connectionCloseError) Error() string {
	return fmt.Sprintf("connection close(%s)", e.code)
}

func (connectionCloseError) Timeout() bool   { return false }
func (connectionCloseError) Temporary() bool { return false }

func (e connectionCloseError) Code() CloseCode { return e.code }
func (e connectionCloseError) Msg() any        { return e.msg }

type timeoutError string

var _ net.Error = (*timeoutError)(nil)

func (e timeoutError) Error() string {
	return fmt.Sprintf("%s timeout", string(e))
}

func (e timeoutError) Timeout() bool   { return true }
func (e timeoutError) Temporary() bool { return true }

const (
	ErrReceiveTimeout timeoutError = "receive"
	ErrSendTimeout    timeoutError = "send"
)

type ConnectionState int

const (
	Connecting ConnectionState = iota
	Open
	Closed
)

func (s ConnectionState) String() string {
	switch s {
	case Connecting:
		return "connecting"
	case Open:
		return "open"
	case Closed:
		return "closed"
	default:
		return "invalid state"
	}
}

type webSocket struct {
	srv   bool
	state ConnectionState
	ext   []string
	pro   string

	conn net.Conn
	rmu  sync.Mutex
	wmu  sync.Mutex

	closing atomic.Bool
}

var _ WebSocket = (*webSocket)(nil)

func (ws *webSocket) State() ConnectionState { return ws.state }
func (ws *webSocket) Extensions() []string   { return ws.ext }
func (ws *webSocket) Protocol() string       { return ws.pro }

func (ws *webSocket) close() (err error) {
	ws.state = Closed
	return ws.conn.Close()
}
func (ws *webSocket) CloseCode(code CloseCode) (err error) {
	if ws.state != Open || !ws.closing.CompareAndSwap(false, true) {
		return
	}
	defer func() {
		if _, ok := err.(ConnectionCloseError); !ok {
			_ = ws.close()
			err = &connectionCloseError{
				code: AbnormalClosure,
				msg:  err,
			}
		}
	}()

	ctx, cancel := context.WithTimeout(context.Background(), time.Second*5)
	defer cancel()

	if err = ws.rmuLockCtx(ctx); err != nil {
		return
	}
	defer ws.rmu.Unlock()

	if err = ws.wmuLockCtx(ctx); err != nil {
		return
	}
	defer ws.wmu.Unlock()

	if err = ws.sendCloseCtxLocked(ctx, code); err != nil {
		return
	}

	_, _, err = ws.recvCtxLocked(ctx)
	return
}
func (ws *webSocket) Close() (err error) {
	return ws.CloseCode(NormalClosure)
}

func (ws *webSocket) read(length int) (data []byte, err error) {
	if length == 0 {
		return
	}
	data = make([]byte, length)

	n, err := ws.conn.Read(data)
	if n != len(data) || err != nil {
		err = fmt.Errorf("io error: %d/%d(%w)", n, len(data), err)
		return
	}
	return
}
func (ws *webSocket) readMetadata() (meta Metadata, err error) {
	metadata, err := ws.read(2)
	if err != nil {
		return
	}

	meta.Fin, meta.Rsv[0], meta.Rsv[1], meta.Rsv[2], meta.Op, meta.Masked, meta.PayloadLen = metadata[0]&0x80 != 0, metadata[0]&0x40 != 0, metadata[0]&0x20 != 0, metadata[0]&0x10 != 0, Opcode(metadata[0]&0x0f), metadata[1]&0x80 != 0, int64(metadata[1]&0x7f)
	if meta.Rsv[0] || meta.Rsv[1] || meta.Rsv[2] {
		err = errors.New("rsv bits are set")
		return
	}
	if ws.srv && !meta.Masked {
		err = errors.New("server received unmasked frame")
		return
	} else if !ws.srv && meta.Masked {
		err = errors.New("client received masked frame")
		return
	}

	switch meta.PayloadLen {
	case 126:
		var ext []byte
		if ext, err = ws.read(2); err != nil {
			return
		}
		meta.PayloadLen = int64(ext[0])<<8 + int64(ext[1])
	case 127:
		var ext []byte
		if ext, err = ws.read(8); err != nil {
			return
		}
		meta.PayloadLen = 0
		for i := 0; i < 8; i++ {
			meta.PayloadLen += int64(int(ext[i]) << 8 * (7 - i))
		}
		if meta.PayloadLen&0x80000000 != 0 {
			err = fmt.Errorf("received 8-byte payload length with the most significant bit set")
			return
		}
	}

	switch meta.Op {
	case OpConnectionClose, OpPing, OpPong:
		if meta.PayloadLen > 125 {
			err = errors.New("payload len too large for control frame")
			return
		}
		if !meta.Fin {
			err = errors.New("control frame is fragmented")
			return
		}
	}

	if meta.Masked {
		if meta.Mask, err = ws.read(4); err != nil {
			return
		}
	}
	return
}
func (ws *webSocket) readPayload(payloadLen int64, mask []byte, verifyUTF8 bool) (data []byte, err error) {
	if data, err = ws.read(int(payloadLen)); err != nil {
		return
	}

	if mask != nil {
		for i := range data {
			data[i] ^= mask[i%4]
		}
	}

	if verifyUTF8 {
		if !utf8.Valid(data) {
			err = errors.New("non-UTF-8 msg within a text message")
			return
		}
	}
	return
}
func (ws *webSocket) receive() (data []byte, meta Metadata, err error) {
	if meta, err = ws.readMetadata(); err != nil {
		return
	}

	switch meta.Op {
	case OpContinuationFrame:
		err = errors.New("received continuation frame as the first frame")
		return
	case OpTextFrame:
	case OpBinaryFrame:
	case OpConnectionClose:
		var p []byte
		if p, err = ws.readPayload(meta.PayloadLen, meta.Mask, false); err != nil {
			return
		}

		if ws.closing.CompareAndSwap(false, true) {
			err = ws.wmuLockCtx(context.Background())
			if err != nil {
				return
			}
			defer ws.wmu.Unlock()
			if err = ws.sendCloseCtxLocked(context.Background(), NormalClosure); err != nil {
				return
			}
		}

		if err := ws.close(); err != nil {
			panic(err)
		}

		if meta.PayloadLen < 2 {
			err = &connectionCloseError{code: NoStatusReceived}
			return
		}
		err = &connectionCloseError{
			CloseCode(uint16(p[0])<<8 + uint16(p[1])),
			p[2:],
		}
		return

	case OpPing:
		if err = ws.pong(); err != nil {
			return
		}
	case OpPong:
	}

	var buf bytes.Buffer
	verifyUTF8 := meta.Op == OpTextFrame

	p, err := ws.readPayload(meta.PayloadLen, meta.Mask, verifyUTF8)
	if err != nil {
		return
	}
	buf.Write(p)

	var curr = meta
	for !curr.Fin {
		if curr, err = ws.readMetadata(); err != nil {
			return
		}
		meta.PayloadLen += curr.PayloadLen

		if curr.Op != OpContinuationFrame {
			err = errors.New("non-continuation frame following first frame")
			return
		}

		if p, err = ws.readPayload(curr.PayloadLen, curr.Mask, verifyUTF8); err != nil {
			return
		}
		buf.Write(p)
	}

	data = buf.Bytes()
	return
}
func (ws *webSocket) rmuLockCtx(ctx context.Context) (err error) {
	done := make(chan struct{})
	go func() {
		defer close(done)
		ws.rmu.Lock()
		select {
		case <-ctx.Done():
			ws.rmu.Unlock()
			return
		default:
		}
	}()
	select {
	case <-ctx.Done():
		return ErrReceiveTimeout
	case <-done:
	}
	return
}
func (ws *webSocket) recvCtxLocked(ctx context.Context) (data []byte, meta Metadata, err error) {
	done := make(chan struct{})
	go func() {
		defer close(done)
		if data, meta, err = ws.receive(); err != nil {
			return
		}
	}()
	select {
	case <-ctx.Done():
		if err = cancelRead(ws.conn); err != nil {
			return
		}
		err = ErrReceiveTimeout
		return
	case <-done:
	}
	return
}
func (ws *webSocket) RecvCtx(ctx context.Context) (data []byte, meta Metadata, err error) {
	defer func() {
		if _, ok := err.(ConnectionCloseError); err != nil && !ok {
			err = fmt.Errorf("websocket: receive error: %w", err)
		}
	}()
	if ws.State() != Open || ws.closing.Load() {
		err = errors.New("ws is not open")
		return
	}
	if ctx == nil {
		err = errors.New("nil context")
		return
	}

	if err = ws.rmuLockCtx(ctx); err != nil {
		return
	}
	defer ws.rmu.Unlock()
	return ws.recvCtxLocked(ctx)
}
func (ws *webSocket) Recv() (data []byte, err error) {
	data, _, err = ws.RecvCtx(context.Background())
	return
}

var MaximumSegmentSize = 1024

func (ws *webSocket) write(b []byte) (err error) {
	if len(b) == 0 {
		return
	}

	n, err := ws.conn.Write(b)
	if n != len(b) || err != nil {
		err = fmt.Errorf("write error: %d/%d(%w)", n, len(b), err)
		return
	}
	return
}
func (ws *webSocket) bufMetadata(buf *bytes.Buffer, fin bool, op Opcode, payload int) (mask []byte) {
	var metadata []byte
	switch {
	case payload <= 125:
		metadata = make([]byte, 2)
		metadata[1] |= byte(payload)
	case payload <= 65535:
		metadata = make([]byte, 4)
		metadata[1] |= 126
		metadata[2], metadata[3] = byte(payload>>8), byte(payload&0xff)
	default:
		metadata = make([]byte, 10)
		metadata[1] |= 127
		for i := 0; i < 8; i++ {
			metadata[2+i] = byte(payload >> 8 * (7 - i) & 0xff)
		}
	}
	if fin {
		metadata[0] |= 0x80
	}
	metadata[0] |= byte(op)
	if !ws.srv {
		metadata[1] |= 0x80
	}
	buf.Write(metadata)

	if !ws.srv {
		mask = make([]byte, 4)
		if _, err := rand.Read(mask); err != nil {
			panic("websocket: random mask failed:" + err.Error())
		}
		buf.Write(mask)
	}
	return
}
func (ws *webSocket) bufPayload(buf *bytes.Buffer, data []byte, mask []byte) {
	data = append(make([]byte, 0, len(data)), data...)
	if mask != nil {
		for i := range data {
			data[i] ^= mask[i%4]
		}
	}
	buf.Write(data)
}

func (ws *webSocket) send(op Opcode, data []byte) (err error) {
	switch op {
	case OpContinuationFrame:
		return errors.New("continuation frame as the first frame")
	case OpTextFrame:
		if !utf8.Valid(data) {
			return errors.New("non-UTF-8 msg within a text message")
		}
		fallthrough
	case OpBinaryFrame:
		var buf bytes.Buffer

		var segment []byte
		split := int(math.Min(float64(len(data)), float64(MaximumSegmentSize)))
		segment, data = data[:split], data[split:]
		mask := ws.bufMetadata(&buf, len(data) == 0, op, len(segment))
		ws.bufPayload(&buf, segment, mask)
		if err = ws.write(buf.Bytes()); err != nil {
			return
		}

		for len(data) != 0 {
			buf.Reset()
			split = int(math.Min(float64(len(data)), float64(MaximumSegmentSize)))
			segment, data = data[:split], data[split:]
			mask = ws.bufMetadata(&buf, len(data) == 0, OpContinuationFrame, len(segment))
			ws.bufPayload(&buf, segment, mask)
			if err = ws.write(buf.Bytes()); err != nil {
				return
			}
		}
		return

	case OpConnectionClose, OpPing, OpPong:
		if len(data) > 125 {
			return errors.New("payload too large for control frame")
		}

		var buf bytes.Buffer
		mask := ws.bufMetadata(&buf, true, op, len(data))
		ws.bufPayload(&buf, data, mask)
		return ws.write(buf.Bytes())

	default:
		return errors.New("invalid opcode")
	}
}

func (ws *webSocket) wmuLockCtx(ctx context.Context) (err error) {
	done := make(chan struct{})
	go func() {
		defer close(done)
		ws.wmu.Lock()
		select {
		case <-ctx.Done():
			ws.wmu.Unlock()
			return
		default:
		}
	}()
	select {
	case <-ctx.Done():
		err = ErrSendTimeout
		return
	case <-done:
	}
	return
}
func (ws *webSocket) sendCtxLocked(ctx context.Context, op Opcode, data []byte) (err error) {
	done := make(chan struct{})
	go func() {
		close(done)
		if err = ws.send(op, data); err != nil {
			return
		}
	}()
	select {
	case <-ctx.Done():
		if err = cancelWrite(ws.conn); err != nil {
			return
		}
		err = ErrSendTimeout
		return
	case <-done:
	}
	return
}
func (ws *webSocket) SendCtx(ctx context.Context, op Opcode, data []byte) (err error) {
	defer func() {
		if _, ok := err.(ConnectionCloseError); err != nil && !ok {
			err = fmt.Errorf("websocket: send error: %w", err)
		}
	}()
	if ws.State() != Open || ws.closing.Load() {
		err = errors.New("ws is not open")
		return
	}
	if ctx == nil {
		return errors.New("nil context")
	}

	if err = ws.wmuLockCtx(ctx); err != nil {
		return
	}
	defer ws.wmu.Unlock()
	return ws.sendCtxLocked(ctx, op, data)
}

func (ws *webSocket) Send(data []byte) (err error) {
	return ws.SendCtx(context.Background(), OpBinaryFrame, data)
}
func (ws *webSocket) SendText(txt string) (err error) {
	return ws.SendCtx(context.Background(), OpTextFrame, []byte(txt))
}

func (ws *webSocket) sendCloseCtxLocked(ctx context.Context, code CloseCode) (err error) {
	var buf bytes.Buffer
	buf.WriteByte(byte(code >> 8 & 0xff))
	buf.WriteByte(byte(code & 0xff))
	return ws.sendCtxLocked(ctx, OpConnectionClose, buf.Bytes())
}

func (ws *webSocket) Ping() (resp []byte, err error) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Second*3)
	defer cancel()

	if err = ws.wmuLockCtx(ctx); err != nil {
		return
	}
	defer ws.wmu.Unlock()
	if err = ws.rmuLockCtx(ctx); err != nil {
		return
	}
	defer ws.rmu.Unlock()
	if err = ws.sendCtxLocked(ctx, OpPing, []byte("ping")); err != nil {
		return
	}
	resp, _, err = ws.recvCtxLocked(ctx)
	return
}
func (ws *webSocket) pong() (err error) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Second*3)
	defer cancel()
	return ws.SendCtx(ctx, OpPong, []byte("pong"))
}

// VerifyURI verifies whether provided uri
// is a valid websocket uri.
//
// rfc 6455: section 3
func VerifyURI(uri string) (err error) {
	u, err := url.Parse(uri)
	if err != nil {
		return
	}
	switch u.Scheme {
	case "ws", "wss":
	default:
		return errors.New("bad scheme")
	}
	if u.Fragment != "" {
		return errors.New("redundant fragment")
	}
	if u.Hostname() == "" {
		return errors.New("missing host")
	}
	return
}

// rfc 6455: section 4.1
func genWsKeyPair() (key, accept string) {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		panic("websocket: random challenge key failed:" + err.Error())
	}
	key = base64.StdEncoding.EncodeToString(b)

	accept = challengeKey(key)
	return
}

// WebSocketGUID is a fixed GUID for websocket server
// to generate |Sec-WebSocket-Accept| header.
//
// rfc 6455: section 4.1
const WebSocketGUID = "258EAFA5-E914-47DA-95CA-C5AB0DC85B11"

func challengeKey(key string) (accept string) {
	sum := sha1.Sum([]byte(key + WebSocketGUID))
	accept = base64.StdEncoding.EncodeToString(sum[:])
	return
}

func cancelRead(conn net.Conn) (err error) {
	if err = conn.SetReadDeadline(time.Now().Add(-1)); err != nil {
		return
	}
	if err = conn.SetReadDeadline(time.Time{}); err != nil {
		return
	}
	return
}
func cancelWrite(conn net.Conn) (err error) {
	if err = conn.SetWriteDeadline(time.Now().Add(-1)); err != nil {
		return
	}
	if err = conn.SetWriteDeadline(time.Time{}); err != nil {
		return
	}
	return
}
