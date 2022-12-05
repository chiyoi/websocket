package websocket

import (
	"context"
	"log"
	"net/http"
	"os"
	"runtime"
	"strings"
	"testing"
	"time"
)

var (
	Error = log.New(os.Stderr, "[ERROR] ", log.LstdFlags|log.Lshortfile|log.Lmsgprefix).Println
	Info  = log.New(os.Stdout, "[INFO] ", log.LstdFlags|log.Lshortfile|log.Lmsgprefix).Println
	Debug = log.New(os.Stdout, "[DEBUG] ", log.LstdFlags|log.Lshortfile|log.Lmsgprefix).Println
)

func TestPing(t *testing.T) {
	http.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		ws, err := Upgrade(w, r)
		if err != nil {
			Error(err)
			return
		}
		defer func() {
			Info("server close:", ws.CloseMsg([]byte("nyan"), AbnormalClosure))
		}()
		Info("server:", ws.State())

		for i := 2; i > 0; i-- {
			time.Sleep(time.Second)
			var resp []byte
			resp, err = ws.Ping()
			if err != nil {
				Error("server error:", err)
				return
			}
			Info("server received:", string(resp))
		}
	})
	go http.ListenAndServe("", nil)
	time.Sleep(time.Second * 2)

	ws, err := Dial("")
	if err != nil {
		Error(err)
	}
	defer func() {
		if err = ws.Close(); err != nil {
			if _, ok := err.(ConnectionCloseError); ok {
				Info("client close:", err)
				runtime.Gosched()
				return
			}
			Error("client error:", err)
		}
	}()
	Info("client:", ws.State())

	for {
		var rev []byte
		rev, err = ws.Recv()
		if err != nil {
			if cce, ok := err.(ConnectionCloseError); ok {
				Info("client close:", string(cce.Msg()))
				return
			}
			Error("client error:", err)
			break
		}
		Info("client received:", string(rev))
	}

	time.Sleep(time.Second)
}

func TestMessage(t *testing.T) {
	var ch = make(chan int, 1)
	http.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		ws, err := Upgrade(w, r)
		if err != nil {
			Error(err)
		}
		Info("server:", ws.State())

		if err = ws.SendText("Hello."); err != nil {
			Error("server error:", err)
			return
		}

		resp, err := ws.Recv()
		if err != nil {
			Error("server error:", err)
			return
		}
		Info("server received:", string(resp))

		var msg = strings.Repeat("nacho ", 10000)
		ch <- len(msg)
		Info("server sent length:", len(msg))
		if err = ws.SendText(msg); err != nil {
			Error("server error:", err)
			return
		}
	})
	go http.ListenAndServe("", nil)
	time.Sleep(time.Second * 2)

	ws, err := Dial("")
	if err != nil {
		Error(err)
	}
	Info("server:", ws.State())

	rev, err := ws.Recv()
	if err != nil {
		Error("client error:", err)
		return
	}
	Info("client received:", string(rev))

	if err = ws.SendText("Hi."); err != nil {
		Error("client error:", err)
		return
	}

	msg, err := ws.RecvCtx(context.Background())
	if err != nil {
		Error("client error:", err)
		return
	}
	Info("msg.Op, msg.Len:", msg.Op, msg.Len)
	l := <-ch
	Info("client received length:", len(msg.Data))
	if l != len(msg.Data) {
		t.Error("length mismatched")
	}
	time.Sleep(time.Second)
}
