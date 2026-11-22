package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"strings"
	"time"
)

const sampleSystem = `You are answering a request sent by an interactive web page (an "artifact") on behalf of the person viewing it. You see only the input below; you have no memory, no browsing and no tools. Follow the page's instructions about format exactly.`

const sampleJSONSystem = sampleSystem + ` Your reply will be machine-parsed as one JSON value: reply with only that JSON value, no prose and no code fences.`

type sampleReq struct {
	Input     json.RawMessage `json:"input"`
	ModelTier string          `json:"modelTier"`
	Model     string          `json:"model"`
	JSON      bool            `json:"json"`
}

func sampleErr(w http.ResponseWriter, status int, code, msg string) {
	writeJSON(w, status, map[string]any{"error": map[string]string{"code": code, "message": msg}})
}

// apiSample proxies a page's sample() call to an Anthropic-Messages-compatible
// gateway (on exe.dev: the LLM integration, no API key) and streams text back as SSE.
func (s *Server) apiSample(w http.ResponseWriter, r *http.Request) {
	ss, a, _ := s.loadForAPI(w, r, lvView)
	if a == nil {
		return
	}
	if a.Caps().Sample == nil {
		sampleErr(w, 403, "not_declared", "this artifact does not declare sample")
		return
	}
	if s.cfg.LLMURL == "" {
		sampleErr(w, 503, "sampling_disabled", "no model is configured on this server")
		return
	}
	var req sampleReq
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 300<<10)).Decode(&req); err != nil {
		sampleErr(w, 400, "prompt_too_large", "input over 256 KiB or not JSON")
		return
	}
	type msg struct {
		Role    string `json:"role"`
		Content string `json:"content"`
	}
	var msgs []msg
	var one string
	if json.Unmarshal(req.Input, &one) == nil {
		msgs = []msg{{"user", one}}
	} else if err := json.Unmarshal(req.Input, &msgs); err != nil || len(msgs) == 0 {
		sampleErr(w, 400, "invalid_request", "input must be a string or a list of turns")
		return
	}
	var merged []msg // consecutive same-role turns read as one
	for _, m := range msgs {
		if (m.Role != "user" && m.Role != "assistant") || strings.TrimSpace(m.Content) == "" {
			sampleErr(w, 400, "invalid_request", "turns need role user|assistant and non-empty content")
			return
		}
		if n := len(merged); n > 0 && merged[n-1].Role == m.Role {
			merged[n-1].Content += "\n\n" + m.Content
		} else {
			merged = append(merged, m)
		}
	}
	if merged[0].Role != "user" || merged[len(merged)-1].Role != "user" {
		sampleErr(w, 400, "invalid_request", "turns must start and end with a user turn")
		return
	}
	key := ss.User.ID
	if !s.allow("sample:"+key, float64(s.cfg.SampleRPM)/60, float64(s.cfg.SampleRPM)) {
		sampleErr(w, 429, "rate_limited", "too many model calls, try again shortly")
		return
	}
	s.mu.Lock()
	if s.sampleBusy[key] >= 3 {
		s.mu.Unlock()
		sampleErr(w, 429, "rate_limited", "too many model calls at once")
		return
	}
	s.sampleBusy[key]++
	s.mu.Unlock()
	defer func() { s.mu.Lock(); s.sampleBusy[key]--; s.mu.Unlock() }()

	tier := req.ModelTier
	if tier == "" || tier == "inherit" {
		tier = "default"
	}
	model := map[string]string{"quick": s.cfg.ModelQuick, "default": s.cfg.ModelDefault, "complex": s.cfg.ModelComplex}[tier]
	if model == "" {
		sampleErr(w, 400, "invalid_request", "unknown modelTier "+tier)
		return
	}
	system := sampleSystem
	if req.JSON {
		system = sampleJSONSystem
	}
	call := func(model string) (*http.Response, error) {
		body, _ := json.Marshal(map[string]any{"model": model, "max_tokens": 16000, "stream": true, "system": system, "messages": merged})
		up, _ := http.NewRequestWithContext(r.Context(), "POST", s.cfg.LLMURL+"/v1/messages", bytes.NewReader(body))
		up.Header.Set("Content-Type", "application/json")
		up.Header.Set("anthropic-version", "2023-06-01")
		if h := s.cfg.LLMHeader; h != "" {
			if k, v, ok := strings.Cut(h, ":"); ok {
				up.Header.Set(strings.TrimSpace(k), strings.TrimSpace(v))
			}
		}
		return (&http.Client{Timeout: 6 * time.Minute}).Do(up)
	}
	modelApplied := ""
	var resp *http.Response
	var err error
	if strings.HasPrefix(req.Model, "claude-") {
		// a named model answers if the gateway has it; otherwise the tier does
		resp, err = call(req.Model)
		if err == nil && resp.StatusCode == 200 {
			modelApplied = req.Model
		} else if resp != nil {
			resp.Body.Close()
			resp = nil
		}
	}
	if resp == nil {
		resp, err = call(model)
	}
	if err != nil {
		sampleErr(w, 502, "upstream_error", "model gateway unreachable")
		return
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		var b bytes.Buffer
		b.ReadFrom(resp.Body)
		log.Printf("sample: gateway %d: %.300s", resp.StatusCode, b.String())
		code := "upstream_error"
		if resp.StatusCode == 429 {
			code = "rate_limited"
		}
		sampleErr(w, 502, code, fmt.Sprintf("model gateway answered %d", resp.StatusCode))
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Accel-Buffering", "no")
	fl, _ := w.(http.Flusher)
	send := func(v any) {
		b, _ := json.Marshal(v)
		fmt.Fprintf(w, "data: %s\n\n", b)
		if fl != nil {
			fl.Flush()
		}
	}
	sc := bufio.NewScanner(resp.Body)
	sc.Buffer(make([]byte, 1<<20), 1<<20)
	stop := ""
	got := false
	for sc.Scan() {
		line := sc.Text()
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		var ev struct {
			Type  string `json:"type"`
			Delta struct {
				Type       string `json:"type"`
				Text       string `json:"text"`
				StopReason string `json:"stop_reason"`
			} `json:"delta"`
			Error struct {
				Type    string `json:"type"`
				Message string `json:"message"`
			} `json:"error"`
		}
		if json.Unmarshal([]byte(strings.TrimSpace(line[5:])), &ev) != nil {
			continue
		}
		switch ev.Type {
		case "content_block_delta":
			if ev.Delta.Type == "text_delta" && ev.Delta.Text != "" {
				got = true
				send(map[string]string{"delta": ev.Delta.Text})
			}
		case "message_delta":
			if ev.Delta.StopReason != "" {
				stop = ev.Delta.StopReason
			}
		case "error":
			code := "upstream_error"
			if ev.Error.Type == "rate_limit_error" || ev.Error.Type == "overloaded_error" {
				code = "rate_limited"
			}
			send(map[string]any{"error": map[string]string{"code": code, "message": ev.Error.Message}})
			return
		}
	}
	if err := sc.Err(); err != nil && r.Context().Err() == nil {
		send(map[string]any{"error": map[string]string{"code": "upstream_error", "message": "stream interrupted"}})
		return
	}
	switch {
	case stop == "refusal":
		send(map[string]any{"error": map[string]string{"code": "refused", "message": "the model declined this input"}})
	case !got:
		send(map[string]any{"error": map[string]string{"code": "empty_completion", "message": "the model produced no text"}})
	default:
		send(map[string]any{"done": true, "truncated": stop == "max_tokens", "modelTierApplied": tier, "modelApplied": modelApplied})
	}
}
