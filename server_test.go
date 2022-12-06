package websocket

import (
	"net/http"
	"testing"
	"time"

	"github.com/chiyoi/websocket/internal/logs"
)

func TestUpgrade(t *testing.T) {
	go http.ListenAndServe("", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ws, err := Upgrade(w, r)
		if err != nil {
			t.Error(err)
			return
		}
		logs.Info(ws)
	}))
	time.Sleep(time.Second * 2)

	ws, err := Dial("")
	if err != nil {
		t.Fatal(err)
	}
	logs.Info(ws)
}
