package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
)

type relayHandlerResult struct {
	Code int            `json:"code"`
	Msg  string         `json:"msg"`
	Data map[string]any `json:"data"`
}

func relayTestSetup(t *testing.T) {
	t.Helper()
	oldCfg, oldCfgPath := cfg, cfgPath
	relayStop()
	cfgPath = filepath.Join(t.TempDir(), "config.json")
	cfg = Config{}
	t.Cleanup(func() {
		relayStop()
		cfg, cfgPath = oldCfg, oldCfgPath
	})
}

func decodeRelayResult(t *testing.T, w *httptest.ResponseRecorder) relayHandlerResult {
	t.Helper()
	var got relayHandlerResult
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatalf("bad response JSON: %s", w.Body.String())
	}
	return got
}

func postRelayHandler(t *testing.T, h http.HandlerFunc, body string) *httptest.ResponseRecorder {
	t.Helper()
	w := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(body))
	h(w, r)
	return w
}

func TestRelayCheckUpdatesRequestStatus(t *testing.T) {
	cases := []struct {
		name       string
		statusCode int
		payload    string
		want       string
	}{
		{
			name:       "rejected",
			statusCode: http.StatusOK,
			payload:    `{"code":0,"data":{"status":"rejected"}}`,
			want:       "rejected",
		},
		{
			name:       "missing",
			statusCode: http.StatusNotFound,
			payload:    `{"code":1,"msg":"申请不存在"}`,
			want:       "missing",
		},
		{
			name:       "pending",
			statusCode: http.StatusOK,
			payload:    `{"code":0,"data":{"status":"pending"}}`,
			want:       "pending",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			relayTestSetup(t)
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(tc.statusCode)
				_, _ = w.Write([]byte(tc.payload))
			}))
			defer srv.Close()
			cfg.RelayURL = srv.URL
			cfg.RelayRequestID = "req-1"
			cfg.RelayRequestToken = "req-token"
			cfg.RelayRequests = []RelayRequestRecord{{ID: "req-1", Token: "req-token", URL: srv.URL, Status: "pending"}}

			w := postRelayHandler(t, handleRelayCheck, `{"requestId":"req-1"}`)
			got := decodeRelayResult(t, w)
			if got.Code != 0 {
				t.Fatalf("code=%d msg=%s", got.Code, got.Msg)
			}
			if got.Data["status"] != tc.want {
				t.Fatalf("status=%v, want %s", got.Data["status"], tc.want)
			}
			if cfg.RelayRequests[0].Status != tc.want {
				t.Fatalf("stored status=%s, want %s", cfg.RelayRequests[0].Status, tc.want)
			}
			if tc.want == "rejected" && !strings.Contains(got.Msg, "失败") {
				t.Fatalf("rejected message should say failed: %s", got.Msg)
			}
			if tc.want == "missing" && !strings.Contains(got.Msg, "失败") {
				t.Fatalf("missing message should say failed: %s", got.Msg)
			}
			if tc.want == "pending" && !strings.Contains(got.Msg, "等待审批") {
				t.Fatalf("pending message should say waiting: %s", got.Msg)
			}
		})
	}
}

func TestRelayStopKeepsCredentialsResumeAndClear(t *testing.T) {
	relayTestSetup(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "not found", http.StatusNotFound)
	}))
	defer srv.Close()
	cfg.RelayURL = srv.URL
	cfg.RelayNodeID = "node-1"
	cfg.RelayNodeToken = "node-token"
	cfg.RelayRequestID = "req-1"
	cfg.RelayRequestToken = "req-token"
	cfg.RelayRequests = []RelayRequestRecord{{ID: "req-1", Token: "req-token", URL: srv.URL, Status: "approved"}}
	cfg.RelayEnabled = true

	w := postRelayHandler(t, handleRelayStop, `{}`)
	if got := decodeRelayResult(t, w); got.Code != 0 {
		t.Fatalf("stop code=%d msg=%s", got.Code, got.Msg)
	}
	if cfg.RelayEnabled {
		t.Fatal("stop must disable auto connect")
	}
	if cfg.RelayURL != srv.URL || cfg.RelayNodeID != "node-1" || cfg.RelayNodeToken != "node-token" {
		t.Fatal("stop must keep node credentials")
	}
	if len(cfg.RelayRequests) != 1 {
		t.Fatal("stop must keep request records")
	}

	w = postRelayHandler(t, handleRelayResume, `{}`)
	if got := decodeRelayResult(t, w); got.Code != 0 {
		t.Fatalf("resume code=%d msg=%s", got.Code, got.Msg)
	}
	if !cfg.RelayEnabled {
		t.Fatal("resume must enable auto connect")
	}

	w = postRelayHandler(t, handleRelayClear, `{}`)
	if got := decodeRelayResult(t, w); got.Code != 0 {
		t.Fatalf("clear code=%d msg=%s", got.Code, got.Msg)
	}
	if cfg.RelayEnabled || cfg.RelayURL != "" || cfg.RelayNodeID != "" || cfg.RelayNodeToken != "" ||
		cfg.RelayRequestID != "" || cfg.RelayRequestToken != "" || len(cfg.RelayRequests) != 0 {
		t.Fatalf("clear must remove all local relay state, got %+v", cfg)
	}
}

func TestRelayResumeWithoutCredentialsFails(t *testing.T) {
	relayTestSetup(t)
	w := postRelayHandler(t, handleRelayResume, `{}`)
	got := decodeRelayResult(t, w)
	if got.Code == 0 {
		t.Fatal("resume without saved credentials should fail")
	}
	if cfg.RelayEnabled {
		t.Fatal("failed resume must not enable auto connect")
	}
}
