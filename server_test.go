package websocket

import (
	"net/http"
	"testing"
	"time"
)

func TestUpgrade(t *testing.T) {
	go http.ListenAndServe("", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ws, err := Upgrade(w, r)
		if err != nil {
			t.Error(err)
			return
		}
		Info(ws)
	}))
	time.Sleep(time.Second * 2)

	ws, err := Dial("")
	if err != nil {
		t.Fatal(err)
	}
	Info(ws)
}
