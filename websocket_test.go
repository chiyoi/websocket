package websocket

import (
	"context"
	"net/http"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/chiyoi/websocket/internal/logs"
)

func TestPing(t *testing.T) {
	http.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		ws, err := Upgrade(w, r)
		if err != nil {
			logs.Error(err)
			return
		}
		defer func() {
			logs.Info("server close:", ws.CloseMsg([]byte("nyan"), AbnormalClosure))
		}()
		logs.Info("server:", ws.State())

		for i := 2; i > 0; i-- {
			time.Sleep(time.Second)
			var resp []byte
			resp, err = ws.Ping()
			if err != nil {
				logs.Error("server error:", err)
				return
			}
			logs.Info("server received:", string(resp))
		}
	})
	go http.ListenAndServe("", nil)
	time.Sleep(time.Second * 2)

	ws, err := Dial("")
	if err != nil {
		logs.Error(err)
	}
	defer func() {
		if err = ws.Close(); err != nil {
			if _, ok := err.(ConnectionCloseError); ok {
				logs.Info("client close:", err)
				runtime.Gosched()
				return
			}
			logs.Error("client error:", err)
		}
	}()
	logs.Info("client:", ws.State())

	for {
		var rev []byte
		rev, err = ws.Recv()
		if err != nil {
			if cce, ok := err.(ConnectionCloseError); ok {
				logs.Info("client close:", string(cce.Msg()))
				return
			}
			logs.Error("client error:", err)
			break
		}
		logs.Info("client received:", string(rev))
	}

	time.Sleep(time.Second)
}

func TestMessage(t *testing.T) {
	var ch = make(chan int, 1)
	http.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		ws, err := Upgrade(w, r)
		if err != nil {
			logs.Error(err)
		}
		logs.Info("server:", ws.State())

		if err = ws.SendText("Hello."); err != nil {
			logs.Error("server error:", err)
			return
		}

		resp, err := ws.Recv()
		if err != nil {
			logs.Error("server error:", err)
			return
		}
		logs.Info("server received:", string(resp))

		var msg = strings.Repeat("nacho ", 10000)
		ch <- len(msg)
		logs.Info("server sent length:", len(msg))
		if err = ws.SendText(msg); err != nil {
			logs.Error("server error:", err)
			return
		}
	})
	go http.ListenAndServe("", nil)
	time.Sleep(time.Second * 2)

	ws, err := Dial("")
	if err != nil {
		logs.Error(err)
	}
	logs.Info("server:", ws.State())

	rev, err := ws.Recv()
	if err != nil {
		logs.Error("client error:", err)
		return
	}
	logs.Info("client received:", string(rev))

	if err = ws.SendText("Hi."); err != nil {
		logs.Error("client error:", err)
		return
	}

	msg, err := ws.RecvCtx(context.Background())
	if err != nil {
		logs.Error("client error:", err)
		return
	}
	logs.Info("msg.Op, msg.Len:", msg.Op, msg.Len)
	l := <-ch
	logs.Info("client received length:", len(msg.Data))
	if l != len(msg.Data) {
		t.Error("length mismatched")
	}
	time.Sleep(time.Second)
}
