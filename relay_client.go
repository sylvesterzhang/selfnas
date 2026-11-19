package main

import (
	"encoding/base64"
	"encoding/json"
	"io"
	"log"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket"
)

type relayClientState struct {
	mu      sync.Mutex
	conn    *websocket.Conn
	writeMu sync.Mutex
	stop    chan struct{}
	done    chan struct{}
	state   string
	nodeID  string
}

type relayRequestState struct {
	body *io.PipeWriter
	req  *http.Request
}

type relayFrame struct {
	Type    string              `json:"type"`
	ID      string              `json:"id,omitempty"`
	Method  string              `json:"method,omitempty"`
	Path    string              `json:"path,omitempty"`
	Headers map[string][]string `json:"headers,omitempty"`
	Status  int                 `json:"status,omitempty"`
	Data    string              `json:"data,omitempty"`
	Error   string              `json:"error,omitempty"`
}

var relayClient relayClientState

func relayURL(base string) string {
	u, err := url.Parse(strings.TrimRight(strings.TrimSpace(base), "/"))
	if err != nil {
		return ""
	}
	switch u.Scheme {
	case "http":
		u.Scheme = "ws"
	case "https":
		u.Scheme = "wss"
	default:
		return ""
	}
	u.Path = strings.TrimRight(u.Path, "/") + "/v1/node/connect"
	return u.String()
}

func relayStart(base, nodeID, token string) {
	base, nodeID, token = strings.TrimSpace(base), strings.TrimSpace(nodeID), strings.TrimSpace(token)
	if relayURL(base) == "" || nodeID == "" || token == "" {
		return
	}
	relayClient.mu.Lock()
	if relayClient.stop != nil {
		relayClient.mu.Unlock()
		return
	}
	relayClient.stop = make(chan struct{})
	relayClient.done = make(chan struct{})
	relayClient.nodeID = nodeID
	relayClient.state = "connecting"
	stop, done := relayClient.stop, relayClient.done
	relayClient.mu.Unlock()
	go func() {
		relayLoop(stop, base, nodeID, token)
		close(done)
		relayClient.mu.Lock()
		if relayClient.done == done {
			relayClient.stop, relayClient.done, relayClient.conn = nil, nil, nil
			relayClient.state = "off"
		}
		relayClient.mu.Unlock()
	}()
}

func relayStop() {
	relayClient.mu.Lock()
	stop, done, conn := relayClient.stop, relayClient.done, relayClient.conn
	relayClient.state = "off"
	relayClient.mu.Unlock()
	if stop != nil {
		close(stop)
	}
	if conn != nil {
		_ = conn.Close()
	}
	if done != nil {
		select {
		case <-done:
		case <-time.After(2 * time.Second):
			log.Printf("[relay] stop timed out while waiting for connection loop")
		}
	}
}

func relayLoop(stop chan struct{}, base, nodeID, token string) {
	backoff := time.Second
	for {
		select {
		case <-stop:
			return
		default:
		}
		wsURL := relayURL(base) + "?node_id=" + url.QueryEscape(nodeID)
		header := http.Header{"Authorization": []string{"Bearer " + token}}
		conn, _, err := websocket.DefaultDialer.Dial(wsURL, header)
		if err != nil {
			log.Printf("[relay] websocket connect failed node=%s url=%s err=%v", nodeID, wsURL, err)
			relaySetState("error")
			select {
			case <-stop:
				return
			case <-time.After(backoff):
			}
			if backoff < 30*time.Second {
				backoff *= 2
			}
			continue
		}
		backoff = time.Second
		relayClient.mu.Lock()
		relayClient.conn = conn
		relayClient.state = "online"
		relayClient.mu.Unlock()
		log.Printf("[relay] websocket connected node=%s", nodeID)
		connDone := make(chan struct{})
		go relayPingLoop(stop, connDone, conn)
		err = relayServeConn(stop, conn)
		close(connDone)
		if err != nil {
			log.Printf("[relay] websocket disconnected node=%s err=%v", nodeID, err)
		}
		_ = conn.Close()
		relayClient.mu.Lock()
		if relayClient.conn == conn {
			relayClient.conn = nil
			relayClient.state = "reconnecting"
		}
		relayClient.mu.Unlock()
	}
}

func relayPingLoop(stop, done chan struct{}, conn *websocket.Conn) {
	ticker := time.NewTicker(25 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-stop:
			return
		case <-done:
			return
		case <-ticker.C:
			deadline := time.Now().Add(10 * time.Second)
			relayClient.writeMu.Lock()
			err := conn.WriteControl(websocket.PingMessage, nil, deadline)
			relayClient.writeMu.Unlock()
			if err != nil {
				log.Printf("[relay] websocket ping failed err=%v", err)
				_ = conn.Close()
				return
			}
		}
	}
}

func relayServeConn(stop chan struct{}, conn *websocket.Conn) error {
	requests := map[string]*relayRequestState{}
	var mu sync.Mutex
	for {
		msgType, payload, err := conn.ReadMessage()
		if err != nil {
			return err
		}
		if msgType != websocket.TextMessage {
			continue
		}
		var f relayFrame
		if json.Unmarshal(payload, &f) != nil {
			continue
		}
		switch f.Type {
		case "request_start":
			pr, pw := io.Pipe()
			req, err := http.NewRequest(f.Method, "http://selfnas.local"+f.Path, pr)
			if err != nil {
				_ = pw.CloseWithError(err)
				continue
			}
			for k, values := range f.Headers {
				for _, v := range values {
					req.Header.Add(k, v)
				}
			}
			mu.Lock()
			requests[f.ID] = &relayRequestState{body: pw, req: req}
			mu.Unlock()
			go relayServeRequest(stop, conn, f.ID, req, &mu, requests)
		case "request_body":
			mu.Lock()
			st := requests[f.ID]
			mu.Unlock()
			if st != nil {
				b, e := base64.StdEncoding.DecodeString(f.Data)
				if e != nil {
					_ = st.body.CloseWithError(e)
				} else {
					_, _ = st.body.Write(b)
				}
			}
		case "request_end":
			mu.Lock()
			st := requests[f.ID]
			mu.Unlock()
			if st != nil {
				_ = st.body.Close()
			}
		}
	}
}

func relayServeRequest(stop chan struct{}, conn *websocket.Conn, id string, req *http.Request, mu *sync.Mutex, requests map[string]*relayRequestState) {
	w := &relayResponseWriter{conn: conn, id: id, client: &relayClient}
	mainHandler.ServeHTTP(w, req)
	_ = w.finish()
	mu.Lock()
	delete(requests, id)
	mu.Unlock()
}

type relayResponseWriter struct {
	conn   *websocket.Conn
	id     string
	client *relayClientState
	wrote  bool
	status int
	header http.Header
}

func (w *relayResponseWriter) Header() http.Header {
	if w.header == nil {
		w.header = make(http.Header)
	}
	return w.header
}
func (w *relayResponseWriter) WriteHeader(status int) {
	if w.wrote {
		return
	}
	w.wrote, w.status = true, status
	_ = w.send(relayFrame{Type: "response_start", ID: w.id, Status: status, Headers: w.Header()})
}
func (w *relayResponseWriter) Write(p []byte) (int, error) {
	if !w.wrote {
		w.WriteHeader(http.StatusOK)
	}
	if len(p) == 0 {
		return 0, nil
	}
	err := w.send(relayFrame{Type: "response_body", ID: w.id, Data: base64.StdEncoding.EncodeToString(p)})
	if err != nil {
		return 0, err
	}
	return len(p), nil
}
func (w *relayResponseWriter) send(f relayFrame) error {
	b, err := json.Marshal(f)
	if err != nil {
		return err
	}
	w.client.writeMu.Lock()
	defer w.client.writeMu.Unlock()
	return w.conn.WriteMessage(websocket.TextMessage, b)
}
func (w *relayResponseWriter) finish() error {
	if !w.wrote {
		w.WriteHeader(http.StatusOK)
	}
	return w.send(relayFrame{Type: "response_end", ID: w.id})
}

func relaySetState(state string) {
	relayClient.mu.Lock()
	defer relayClient.mu.Unlock()
	if relayClient.stop != nil {
		relayClient.state = state
	}
}
func relaySnapshot() map[string]any {
	relayClient.mu.Lock()
	defer relayClient.mu.Unlock()
	return map[string]any{"state": relayClient.state, "node_id": relayClient.nodeID}
}
