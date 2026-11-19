package main

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"time"
)

func handleRelayApply(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSON(w, 405, "只支持 POST", nil)
		return
	}
	if cfg.RelayNodeID != "" || cfg.RelayEnabled {
		writeJSON(w, 409, "当前已有 Relay 连接，请先断开并清除后再申请", nil)
		return
	}
	for _, item := range cfg.RelayRequests {
		if item.Status == "pending" {
			writeJSON(w, 409, "已有一条待审批申请，请先检查该申请结果", nil)
			return
		}
	}
	if len(cfg.RelayRequests) == 0 && cfg.RelayRequestID != "" {
		writeJSON(w, 409, "已有一条历史申请，请先检查该申请结果", nil)
		return
	}
	var b struct {
		RelayURL   string `json:"url"`
		DeviceName string `json:"deviceName"`
		Hostname   string `json:"hostname"`
		Platform   string `json:"platform"`
		Version    string `json:"version"`
	}
	if err := json.NewDecoder(r.Body).Decode(&b); err != nil {
		writeJSON(w, 400, "参数格式不对", nil)
		return
	}
	b.RelayURL = strings.TrimRight(strings.TrimSpace(b.RelayURL), "/")
	if b.RelayURL == "" || b.DeviceName == "" {
		writeJSON(w, 400, "需要 Relay 地址和设备名称", nil)
		return
	}
	body, _ := json.Marshal(map[string]string{"device_name": b.DeviceName, "hostname": b.Hostname, "platform": b.Platform, "version": b.Version})
	resp, err := http.Post(b.RelayURL+"/api/enrollment/requests", "application/json", bytes.NewReader(body))
	if err != nil {
		writeJSON(w, 502, "连接 Relay 失败："+err.Error(), nil)
		return
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(resp.Body)
	if resp.StatusCode == http.StatusOK {
		var envelope struct {
			Data struct {
				RequestID    string `json:"request_id"`
				RequestToken string `json:"request_token"`
				Status       string `json:"status"`
			} `json:"data"`
		}
		if json.Unmarshal(data, &envelope) == nil && envelope.Data.RequestID != "" {
			now := time.Now().Format(time.RFC3339)
			cfg.RelayURL, cfg.RelayRequestID, cfg.RelayRequestToken = b.RelayURL, envelope.Data.RequestID, envelope.Data.RequestToken
			cfg.RelayRequests = append([]RelayRequestRecord{{
				ID: envelope.Data.RequestID, Token: envelope.Data.RequestToken, URL: b.RelayURL,
				Device: b.DeviceName, Status: envelope.Data.Status, CreatedAt: now,
			}}, cfg.RelayRequests...)
			if len(cfg.RelayRequests) > 20 {
				cfg.RelayRequests = cfg.RelayRequests[:20]
			}
			_ = saveConfig()
		}
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(resp.StatusCode)
	_, _ = w.Write(data)
}

func handleRelayCheck(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSON(w, 405, "只支持 POST", nil)
		return
	}
	var input struct {
		RequestID string `json:"requestId"`
	}
	if err := json.NewDecoder(r.Body).Decode(&input); err != nil || strings.TrimSpace(input.RequestID) == "" {
		writeJSON(w, 400, "需要申请 ID", nil)
		return
	}
	requestID, requestToken, relayURL := "", "", ""
	for _, item := range cfg.RelayRequests {
		if item.ID == input.RequestID {
			requestID, requestToken, relayURL = item.ID, item.Token, item.URL
			break
		}
	}
	if requestID == "" && cfg.RelayRequestID == input.RequestID {
		requestID, requestToken, relayURL = cfg.RelayRequestID, cfg.RelayRequestToken, cfg.RelayURL
	}
	if requestID == "" {
		writeJSON(w, 404, "本机没有这条申请记录", nil)
		return
	}
	if requestToken == "" || relayURL == "" {
		writeJSON(w, 400, "申请凭证不完整，请清除记录后重新申请", nil)
		return
	}
	req, err := http.NewRequest(http.MethodGet, strings.TrimRight(relayURL, "/")+"/api/enrollment/requests/"+requestID, nil)
	if err != nil {
		writeJSON(w, 400, "Relay 地址无效", nil)
		return
	}
	req.Header.Set("X-Request-Token", requestToken)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		writeJSON(w, 502, "连接 Relay 失败："+err.Error(), nil)
		return
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(resp.Body)
	var envelope struct {
		Code int    `json:"code"`
		Msg  string `json:"msg"`
		Data struct {
			Status    string `json:"status"`
			NodeID    string `json:"node_id"`
			NodeToken string `json:"node_token"`
		} `json:"data"`
	}
	if json.Unmarshal(data, &envelope) != nil {
		writeJSON(w, 502, "Relay 返回了无法识别的状态", nil)
		return
	}
	status := envelope.Data.Status
	if resp.StatusCode == http.StatusNotFound || envelope.Msg == "申请不存在" {
		status = "missing"
	} else if resp.StatusCode < 200 || resp.StatusCode >= 300 || envelope.Code != 0 {
		writeJSON(w, 502, "查询申请失败："+envelope.Msg, nil)
		return
	}
	if status == "" {
		writeJSON(w, 502, "Relay 返回的申请状态为空", nil)
		return
	}
	requestUpdated := false
	for i := range cfg.RelayRequests {
		if cfg.RelayRequests[i].ID == requestID {
			cfg.RelayRequests[i].Status = status
			requestUpdated = true
		}
	}
	if !requestUpdated && cfg.RelayRequestID == requestID {
		cfg.RelayRequests = append([]RelayRequestRecord{{
			ID: requestID, Token: requestToken, URL: relayURL, Device: "SelfNAS", Status: status,
			CreatedAt: "（历史申请，时间未记录）",
		}}, cfg.RelayRequests...)
	}
	if status == "approved" {
		if envelope.Data.NodeID == "" || envelope.Data.NodeToken == "" {
			writeJSON(w, 502, "服务端批准了申请，但没有返回完整节点凭证", nil)
			return
		}
		cfg.RelayURL = relayURL
		cfg.RelayNodeID, cfg.RelayNodeToken, cfg.RelayEnabled = envelope.Data.NodeID, envelope.Data.NodeToken, true
		relayStart(relayURL, cfg.RelayNodeID, cfg.RelayNodeToken)
	}
	if err := saveConfig(); err != nil {
		writeJSON(w, 500, "状态已查询，但保存本机结果失败："+err.Error(), nil)
		return
	}
	message := "当前状态：" + status
	if status == "rejected" {
		message = "服务端已拒绝申请，这条申请失败"
	} else if status == "missing" {
		message = "服务端已找不到这条申请记录，这条申请失败"
	} else if status == "pending" {
		message = "服务端仍有这条申请，当前等待审批"
	}
	writeJSON(w, 0, message, map[string]string{"status": status})
}

func handleRelayDelete(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSON(w, 405, "只支持 POST", nil)
		return
	}
	var input struct {
		RequestID string `json:"requestId"`
	}
	if err := json.NewDecoder(r.Body).Decode(&input); err != nil || strings.TrimSpace(input.RequestID) == "" {
		writeJSON(w, 400, "需要申请 ID", nil)
		return
	}
	found := false
	kept := make([]RelayRequestRecord, 0, len(cfg.RelayRequests))
	for _, item := range cfg.RelayRequests {
		if item.ID == input.RequestID {
			found = true
			continue
		}
		kept = append(kept, item)
	}
	if !found && cfg.RelayRequestID == input.RequestID {
		found = true
	}
	if !found {
		writeJSON(w, 404, "申请记录不存在", nil)
		return
	}
	cfg.RelayRequests = kept
	if cfg.RelayRequestID == input.RequestID {
		cfg.RelayRequestID, cfg.RelayRequestToken = "", ""
	}
	if err := saveConfig(); err != nil {
		writeJSON(w, 500, "删除本机申请记录失败："+err.Error(), nil)
		return
	}
	writeJSON(w, 0, "已从本机列表删除申请记录", map[string]bool{"deleted": true})
}

func handleRelayStatus(w http.ResponseWriter, r *http.Request) {
	d := relaySnapshot()
	d["enabled"] = cfg.RelayEnabled
	d["auto_connect"] = cfg.RelayEnabled
	d["has_node"] = cfg.RelayURL != "" && cfg.RelayNodeID != "" && cfg.RelayNodeToken != ""
	d["url"] = cfg.RelayURL
	d["node_id"] = cfg.RelayNodeID
	if cfg.RelayURL != "" && cfg.RelayNodeID != "" {
		d["access_url"] = strings.TrimRight(cfg.RelayURL, "/") + "/n/" + cfg.RelayNodeID + "/files"
	}
	requests := cfg.RelayRequests
	if len(requests) == 0 && cfg.RelayRequestID != "" {
		requests = []RelayRequestRecord{{ID: cfg.RelayRequestID, URL: cfg.RelayURL, Device: "SelfNAS", Status: "pending", CreatedAt: "（历史申请，时间未记录）"}}
	}
	items := make([]map[string]string, 0, len(requests))
	for _, item := range requests {
		items = append(items, map[string]string{"id": item.ID, "url": item.URL, "device": item.Device, "status": item.Status, "created_at": item.CreatedAt})
	}
	d["requests"] = items
	writeJSON(w, 0, "ok", d)
}

func handleRelayStart(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSON(w, 405, "只支持 POST", nil)
		return
	}
	var b struct {
		URL    string `json:"url"`
		NodeID string `json:"nodeId"`
		Token  string `json:"token"`
	}
	if err := json.NewDecoder(r.Body).Decode(&b); err != nil {
		writeJSON(w, 400, "参数格式不对", nil)
		return
	}
	b.URL, b.NodeID, b.Token = strings.TrimSpace(b.URL), strings.TrimSpace(b.NodeID), strings.TrimSpace(b.Token)
	if b.URL == "" || b.NodeID == "" || b.Token == "" {
		writeJSON(w, 400, "需要 Relay 地址、节点 ID 和节点凭证", nil)
		return
	}
	cfg.RelayURL, cfg.RelayNodeID, cfg.RelayNodeToken, cfg.RelayEnabled = b.URL, b.NodeID, b.Token, true
	if err := saveConfig(); err != nil {
		writeJSON(w, 500, "保存配置失败："+err.Error(), nil)
		return
	}
	relayStart(b.URL, b.NodeID, b.Token)
	writeJSON(w, 0, "正在连接 Relay", relaySnapshot())
}

func handleRelayStop(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSON(w, 405, "只支持 POST", nil)
		return
	}
	relayStop()
	// 用户主动断开 = 不想连了，重启后也不能自动恢复。
	// 但这里不是“删除连接”：本机地址、节点凭证和申请记录都保留，方便之后恢复。
	cfg.RelayEnabled = false
	if err := saveConfig(); err != nil {
		writeJSON(w, 500, "已断开，但保存本机配置失败："+err.Error(), nil)
		return
	}
	writeJSON(w, 0, "已断开，程序重启后不会自动恢复；本机连接信息已保留", relaySnapshot())
}

func handleRelayResume(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSON(w, 405, "只支持 POST", nil)
		return
	}
	if relayURL(cfg.RelayURL) == "" || cfg.RelayNodeID == "" || cfg.RelayNodeToken == "" {
		writeJSON(w, 400, "本机没有可恢复的 Relay 连接信息，请删除后重新申请", nil)
		return
	}
	cfg.RelayEnabled = true
	if err := saveConfig(); err != nil {
		writeJSON(w, 500, "保存自动恢复设置失败："+err.Error(), nil)
		return
	}
	relayStart(cfg.RelayURL, cfg.RelayNodeID, cfg.RelayNodeToken)
	writeJSON(w, 0, "正在恢复 Relay，并已恢复重启后自动连接", relaySnapshot())
}

func handleRelayClear(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSON(w, 405, "只支持 POST", nil)
		return
	}
	relayStop()
	cfg.RelayEnabled = false
	cfg.RelayURL = ""
	cfg.RelayNodeID = ""
	cfg.RelayNodeToken = ""
	cfg.RelayRequestID = ""
	cfg.RelayRequestToken = ""
	cfg.RelayRequests = nil
	if err := saveConfig(); err != nil {
		writeJSON(w, 500, "已断开，但清除本机 Relay 信息失败："+err.Error(), nil)
		return
	}
	writeJSON(w, 0, "已删除本机 Relay 连接与申请记录；服务端节点不会因此自动撤销", nil)
}
