package main

import (
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

func TestRelayURL(t *testing.T) {
	cases := map[string]string{
		"https://relay.example.com": "wss://relay.example.com/v1/node/connect",
		"http://127.0.0.1:8080/":    "ws://127.0.0.1:8080/v1/node/connect",
	}
	for in, want := range cases {
		if got := relayURL(in); got != want {
			t.Fatalf("relayURL(%q)=%q, want %q", in, got, want)
		}
	}
}

func TestRelayClientServesRequestFrame(t *testing.T) {
	oldHandler := mainHandler
	defer func() { mainHandler = oldHandler; relayStop() }()
	mainHandler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		_, _ = w.Write([]byte("relay-ok:" + r.URL.Path))
	})
	upgrader := websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}
	got := make(chan string, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer c.Close()
		start := relayFrame{Type: "request_start", ID: "req-1", Method: "GET", Path: "/files", Headers: map[string][]string{"Accept": {"text/plain"}}}
		b, _ := json.Marshal(start)
		_ = c.WriteMessage(websocket.TextMessage, b)
		end, _ := json.Marshal(relayFrame{Type: "request_end", ID: "req-1"})
		_ = c.WriteMessage(websocket.TextMessage, end)
		for {
			_, payload, err := c.ReadMessage()
			if err != nil {
				return
			}
			var f relayFrame
			if json.Unmarshal(payload, &f) != nil {
				continue
			}
			if f.Type == "response_body" {
				decoded, _ := base64.StdEncoding.DecodeString(f.Data)
				got <- string(decoded)
				return
			}
		}
	}))
	defer srv.Close()
	relayStart(srv.URL, "node-test", "token-test")
	select {
	case body := <-got:
		if !strings.Contains(body, "relay-ok") {
			t.Fatalf("unexpected body frame: %q", body)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("timed out waiting for client response")
	}
}
