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

	// RecvCtx waits for a websocket message, and parses it into Message.
	// Frames with OpPing will be responded with a frame with OpPong.
	// Frames with OpConnectionClose will be responded and packed as ConnectionCloseError.
	RecvCtx(ctx context.Context) (msg Message, err error)
	// Recv works like WebSocket.RecvCtx but discards the frame metadata.
	Recv() (data []byte, err error)

	// SendCtx sends a message to the connection.
	SendCtx(ctx context.Context, msg Message) (err error)
	// Send sends a message with OpBinaryFrame to the connection.
	Send(data []byte) (err error)
	// SendText sends a message with OpTextFrame to the connection.
	SendText(txt string) (err error)
	// Ping sends a frame with OpPing to the connection and waits for the next frame,
	// typically a frame with OpPong, parses it and returns its body.
	Ping() (resp []byte, err error)

	// Close sends a frame with OpConnectionClose, waits for the response and closes the underlying
	// net.Conn. It does nothing if WebSocket is already closed or is closing.
	Close() (err error)
}

// Opcode indicates the operation of a websocket frame.
//
// rfc 6455: section 5.2
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
		return "Opcode(continuation frame)"
	case OpTextFrame:
		return "Opcode(text frame)"
	case OpBinaryFrame:
		return "Opcode(binary frame)"
	case OpConnectionClose:
		return "Opcode(connection)"
	case OpPing:
		return "Opcode(ping)"
	case OpPong:
		return "Opcode(pong)"
	default:
		return "Opcode(invalid operation)"
	}
}

// Message is a websocket message.
// To send a message, the Message.Len will be calculated from Message.Data and
// the original value will be discarded.
//
// rfc 6455: section 1.2
type Message struct {
	// Op is the Opcode of the message(the Opcode of the first frame).
	//
	// rfc 6455: section 5.2
	Op Opcode
	// Len indicates the content-length of the message(the total payload length of all frames).
	Len int64

	// Data is the message content(concatenate the payload of all frames).
	Data []byte
}

// CloseCode indicates the close status of a closed websocket connection,
// call CloseCode.String() to get the status text.
//
// rfc 6455: section 7.4
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

// ConnectionCloseError contains all about an error caused by connection close.
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

// ConnectionState indicates the connection state of a websocket connection.
//
// rfc 6455
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

	_, err = ws.recvCtxLocked(ctx)
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
func (ws *webSocket) readMetadata() (fin bool, op Opcode, payloadLen int64, mask []byte, err error) {
	metadata, err := ws.read(2)
	if err != nil {
		return
	}

	var rsv [3]bool
	var masked bool
	fin, rsv[0], rsv[1], rsv[2], op, masked, payloadLen = metadata[0]&0x80 != 0, metadata[0]&0x40 != 0, metadata[0]&0x20 != 0, metadata[0]&0x10 != 0, Opcode(metadata[0]&0x0f), metadata[1]&0x80 != 0, int64(metadata[1]&0x7f)
	if rsv[0] || rsv[1] || rsv[2] {
		err = errors.New("some of rsv bits are set")
		return
	}
	if ws.srv && !masked {
		err = errors.New("server received unmasked frame")
		return
	} else if !ws.srv && masked {
		err = errors.New("client received masked frame")
		return
	}

	switch payloadLen {
	case 126:
		var ext []byte
		if ext, err = ws.read(2); err != nil {
			return
		}
		payloadLen = int64(ext[0])<<8 + int64(ext[1])
	case 127:
		var ext []byte
		if ext, err = ws.read(8); err != nil {
			return
		}
		payloadLen = 0
		for i := 0; i < 8; i++ {
			payloadLen += int64(int(ext[i]) << 8 * (7 - i))
		}
		if payloadLen&0x80000000 != 0 {
			err = fmt.Errorf("received 8-byte payload length with the most significant bit set")
			return
		}
	}

	switch op {
	case OpConnectionClose, OpPing, OpPong:
		if payloadLen > 125 {
			err = errors.New("payload len too large for control frame")
			return
		}
		if !fin {
			err = errors.New("control frame is fragmented")
			return
		}
	}

	if masked {
		if mask, err = ws.read(4); err != nil {
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
func (ws *webSocket) receive() (msg Message, err error) {
	fin, op, payloadLen, mask, err := ws.readMetadata()
	if err != nil {
		return
	}
	msg.Op, msg.Len = op, payloadLen

	switch op {
	case OpContinuationFrame:
		err = errors.New("received continuation frame as the first frame")
		return
	case OpTextFrame:
	case OpBinaryFrame:
	case OpConnectionClose:
		var p []byte
		if p, err = ws.readPayload(payloadLen, mask, false); err != nil {
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

		if payloadLen < 2 {
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
	verifyUTF8 := op == OpTextFrame

	p, err := ws.readPayload(payloadLen, mask, verifyUTF8)
	if err != nil {
		return
	}
	buf.Write(p)

	for !fin {
		if fin, op, payloadLen, mask, err = ws.readMetadata(); err != nil {
			return
		}
		msg.Len += payloadLen

		if op != OpContinuationFrame {
			err = errors.New("non-continuation frame following first frame")
			return
		}

		if p, err = ws.readPayload(payloadLen, mask, verifyUTF8); err != nil {
			return
		}
		buf.Write(p)
	}

	msg.Data = buf.Bytes()
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
		return timeoutError("receive")
	case <-done:
	}
	return
}
func (ws *webSocket) recvCtxLocked(ctx context.Context) (msg Message, err error) {
	done := make(chan struct{})
	go func() {
		defer close(done)
		if msg, err = ws.receive(); err != nil {
			return
		}
	}()
	select {
	case <-ctx.Done():
		if err = cancelRead(ws.conn); err != nil {
			return
		}
		err = timeoutError("receive")
		return
	case <-done:
	}
	return
}
func (ws *webSocket) RecvCtx(ctx context.Context) (msg Message, err error) {
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
	msg, err := ws.RecvCtx(context.Background())
	data = msg.Data
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
		err = timeoutError("send")
		return
	case <-done:
	}
	return
}
func (ws *webSocket) sendCtxLocked(ctx context.Context, msg Message) (err error) {
	done := make(chan struct{})
	go func() {
		close(done)
		if err = ws.send(msg.Op, msg.Data); err != nil {
			return
		}
	}()
	select {
	case <-ctx.Done():
		if err = cancelWrite(ws.conn); err != nil {
			return
		}
		err = timeoutError("send")
		return
	case <-done:
	}
	return
}
func (ws *webSocket) SendCtx(ctx context.Context, msg Message) (err error) {
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
	return ws.sendCtxLocked(ctx, msg)
}

func (ws *webSocket) Send(data []byte) (err error) {
	return ws.SendCtx(context.Background(), Message{
		Op:   OpBinaryFrame,
		Data: data,
	})
}
func (ws *webSocket) SendText(txt string) (err error) {
	return ws.SendCtx(context.Background(), Message{
		Op:   OpTextFrame,
		Data: []byte(txt),
	})
}

func (ws *webSocket) sendCloseCtxLocked(ctx context.Context, code CloseCode) (err error) {
	var buf bytes.Buffer
	buf.WriteByte(byte(code >> 8 & 0xff))
	buf.WriteByte(byte(code & 0xff))
	return ws.sendCtxLocked(ctx, Message{
		Op:   OpConnectionClose,
		Data: buf.Bytes(),
	})
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
	if err = ws.sendCtxLocked(ctx, Message{
		Op:   OpPing,
		Data: []byte("ping"),
	}); err != nil {
		return
	}
	msg, err := ws.recvCtxLocked(ctx)
	resp = msg.Data
	return
}
func (ws *webSocket) pong() (err error) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Second*3)
	defer cancel()
	return ws.SendCtx(ctx, Message{
		Op:   OpPong,
		Data: []byte("pong"),
	})
}

// VerifyURI verifies whether the provided uri is a valid websocket uri,
// returns nil if it's valid.
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
