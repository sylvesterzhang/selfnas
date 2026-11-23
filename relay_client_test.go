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

func TestRelayClientBodyDoesNotBlockFollowingRequests(t *testing.T) {
	oldHandler := mainHandler
	defer func() { mainHandler = oldHandler; relayStop() }()
	mainHandler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			// Simulate middleware/handler returning without consuming the body.
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		_, _ = w.Write([]byte("following-request-ok"))
	})
	upgrader := websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}
	got := make(chan bool, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer c.Close()
		frames := []relayFrame{
			{Type: "request_start", ID: "post-1", Method: http.MethodPost, Path: "/ignored-body"},
			{Type: "request_body", ID: "post-1", Data: base64.StdEncoding.EncodeToString([]byte("body-that-handler-does-not-read"))},
			{Type: "request_end", ID: "post-1"},
			{Type: "request_start", ID: "get-2", Method: http.MethodGet, Path: "/next"},
			{Type: "request_end", ID: "get-2"},
		}
		for _, f := range frames {
			b, _ := json.Marshal(f)
			if err := c.WriteMessage(websocket.TextMessage, b); err != nil {
				got <- false
				return
			}
		}
		for {
			_, payload, err := c.ReadMessage()
			if err != nil {
				got <- false
				return
			}
			var f relayFrame
			if json.Unmarshal(payload, &f) == nil && f.ID == "get-2" && f.Type == "response_end" {
				got <- true
				return
			}
		}
	}))
	defer srv.Close()
	relayStart(srv.URL, "node-test", "token-test")
	select {
	case ok := <-got:
		if !ok {
			t.Fatal("websocket closed before following request completed")
		}
	case <-time.After(3 * time.Second):
		t.Fatal("body from an unread request blocked the next websocket request")
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
