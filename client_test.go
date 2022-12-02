package websocket

import (
	"context"
	"net/http"
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

	go http.ListenAndServe("", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ws, err := Upgrade(w, r)
		if err != nil {
			Info(err)
		}
		Info(ws)
	}))
	time.Sleep(time.Second * 2)
	c := Dialer{
		Extensions: []string{"nacho"},
		Header: map[string][]string{
			"Neko": {"nyan"},
		},
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second*3)
	defer cancel()
	ws, err = c.DialContext(ctx, "")
	if err != nil {
		Info(err)
	}
	Info(ws)
}
