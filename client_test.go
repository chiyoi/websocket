package websocket

import (
	"context"
	"io"
	"strings"
	"testing"
	"time"
)

func TestDial(t *testing.T) {
	ws, err := Dial("wss://neko03.moe/")
	if err != nil {
		Info(err)
	}
	Info(ws)
	ws, err = Dial("ws://neko03.moe/")
	if err != nil {
		Info(err)
	}
	Info(ws)

	c := Dialer{
		Body: io.NopCloser(strings.NewReader("neko nyan nyan")),
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second*3)
	defer cancel()
	ws, err = c.DialContext(ctx, "")
	if err != nil {
		Info(err)
	}
	Info(ws)
}
