package websocket

import (
	"net/http"
	"testing"
)

func TestHijack(t *testing.T) {
	go http.ListenAndServe("", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ws, err := Hijack(w, r)
		if err != nil {
			t.Error(err)
			return
		}
		Info(ws)
	}))

	ws, err := Dial("")
	if err != nil {
		t.Fatal(err)
	}
	Info(ws)
}
