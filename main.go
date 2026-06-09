package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/yexzf/claude-openai-converter/internal/apicompat"
	"github.com/yexzf/claude-openai-converter/internal/upstreamurl"
)

type Config struct {
	ListenAddr      string
	UpstreamBaseURL string
	UpstreamAPIKey  string
	UpstreamModel   string
	ModelMap        map[string]string
	HTTPClient      *http.Client
}

func main() {
	cfg, err := loadConfig()
	if err != nil {
		log.Fatalf("config error: %v", err)
	}

	srv := &Server{cfg: cfg}
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", srv.handleHealthz)
	mux.HandleFunc("/v1/messages", srv.handleMessages)
	mux.HandleFunc("/v1/models", srv.handleModels)
	mux.HandleFunc("/", srv.handleRoot)

	server := &http.Server{
		Addr:              cfg.ListenAddr,
		Handler:           mux,
		ReadHeaderTimeout: 10 * time.Second,
	}

	log.Printf("claude-openai-converter listening on %s, upstream=%s", cfg.ListenAddr, cfg.UpstreamBaseURL)
	if err := server.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		log.Fatal(err)
	}
}

type Server struct {
	cfg Config
}

func loadConfig() (Config, error) {
	listenAddr := strings.TrimSpace(getenvDefault("LISTEN_ADDR", ":8787"))
	upstreamBaseURL := strings.TrimSpace(os.Getenv("UPSTREAM_BASE_URL"))
	if upstreamBaseURL == "" {
		return Config{}, fmt.Errorf("UPSTREAM_BASE_URL is required")
	}

	modelMap, err := parseModelMap()
	if err != nil {
		return Config{}, err
	}

	client := &http.Client{
		Timeout: 0,
		Transport: &http.Transport{
			Proxy:                 http.ProxyFromEnvironment,
			ForceAttemptHTTP2:     true,
			MaxIdleConns:          100,
			MaxIdleConnsPerHost:   20,
			MaxConnsPerHost:       0,
			IdleConnTimeout:       90 * time.Second,
			TLSHandshakeTimeout:   10 * time.Second,
			ExpectContinueTimeout: 1 * time.Second,
		},
	}

	return Config{
		ListenAddr:      listenAddr,
		UpstreamBaseURL: upstreamBaseURL,
		UpstreamAPIKey:  strings.TrimSpace(os.Getenv("UPSTREAM_API_KEY")),
		UpstreamModel:   strings.TrimSpace(os.Getenv("UPSTREAM_MODEL")),
		ModelMap:        modelMap,
		HTTPClient:      client,
	}, nil
}

func parseModelMap() (map[string]string, error) {
	raw := strings.TrimSpace(os.Getenv("MODEL_MAP_JSON"))
	if raw == "" {
		return nil, nil
	}
	var m map[string]string
	if err := json.Unmarshal([]byte(raw), &m); err != nil {
		return nil, fmt.Errorf("parse MODEL_MAP_JSON: %w", err)
	}
	cleaned := make(map[string]string, len(m))
	for k, v := range m {
		k = strings.TrimSpace(k)
		v = strings.TrimSpace(v)
		if k == "" || v == "" {
			continue
		}
		cleaned[k] = v
	}
	return cleaned, nil
}

func getenvDefault(key, fallback string) string {
	if v := strings.TrimSpace(os.Getenv(key)); v != "" {
		return v
	}
	return fallback
}

func (s *Server) handleRoot(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path == "/" {
		writeJSON(w, http.StatusOK, map[string]string{
			"name":     "claude-openai-converter",
			"status":   "ok",
			"upstream": s.cfg.UpstreamBaseURL,
		})
		return
	}
	writeAnthropicError(w, http.StatusNotFound, "not_found_error", "not found")
}

func (s *Server) handleHealthz(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

func (s *Server) handleModels(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeAnthropicError(w, http.StatusMethodNotAllowed, "invalid_request_error", "method not allowed")
		return
	}
	upstream, err := s.proxyUpstream(r.Context(), http.MethodGet, "/v1/models", nil, "application/json")
	if err != nil {
		writeAnthropicError(w, http.StatusBadGateway, "api_error", err.Error())
		return
	}
	defer upstream.Body.Close()
	copyUpstreamResponse(w, upstream)
}

func (s *Server) handleMessages(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeAnthropicError(w, http.StatusMethodNotAllowed, "invalid_request_error", "method not allowed")
		return
	}

	body, err := io.ReadAll(r.Body)
	if err != nil {
		writeAnthropicError(w, http.StatusBadRequest, "invalid_request_error", "failed to read request body")
		return
	}
	if len(bytes.TrimSpace(body)) == 0 {
		writeAnthropicError(w, http.StatusBadRequest, "invalid_request_error", "request body is empty")
		return
	}

	var anthropicReq apicompat.AnthropicRequest
	if err := json.Unmarshal(body, &anthropicReq); err != nil {
		writeAnthropicError(w, http.StatusBadRequest, "invalid_request_error", "failed to parse request body")
		return
	}
	originalModel := strings.TrimSpace(anthropicReq.Model)
	if originalModel == "" {
		writeAnthropicError(w, http.StatusBadRequest, "invalid_request_error", "model is required")
		return
	}

	mappedModel := s.mapModel(originalModel)
	anthropicReq.Model = mappedModel

	responsesReq, err := apicompat.AnthropicToResponses(&anthropicReq)
	if err != nil {
		writeAnthropicError(w, http.StatusBadRequest, "invalid_request_error", err.Error())
		return
	}

	chatReq, err := apicompat.ResponsesToChatCompletionsRequest(responsesReq)
	if err != nil {
		writeAnthropicError(w, http.StatusBadRequest, "invalid_request_error", err.Error())
		return
	}
	chatReq.Model = mappedModel
	if anthropicReq.Stream {
		chatReq.StreamOptions = &apicompat.ChatStreamOptions{IncludeUsage: true}
	}

	upstreamBody, err := json.Marshal(chatReq)
	if err != nil {
		writeAnthropicError(w, http.StatusInternalServerError, "api_error", "failed to marshal upstream request")
		return
	}

	upstream, err := s.proxyUpstream(r.Context(), http.MethodPost, "/v1/chat/completions", upstreamBody, streamAcceptHeader(anthropicReq.Stream))
	if err != nil {
		writeAnthropicError(w, http.StatusBadGateway, "api_error", err.Error())
		return
	}
	defer upstream.Body.Close()

	if upstream.StatusCode >= 400 {
		s.writeUpstreamError(w, upstream)
		return
	}

	if anthropicReq.Stream {
		s.handleStream(w, upstream.Body, originalModel)
		return
	}
	s.handleNonStream(w, upstream.Body, originalModel)
}

func (s *Server) mapModel(requested string) string {
	requested = strings.TrimSpace(requested)
	if requested == "" {
		return requested
	}
	if s.cfg.ModelMap != nil {
		if mapped := strings.TrimSpace(s.cfg.ModelMap[requested]); mapped != "" {
			return mapped
		}
		if mapped := strings.TrimSpace(s.cfg.ModelMap["*"]); mapped != "" {
			return mapped
		}
	}
	if s.cfg.UpstreamModel != "" {
		return s.cfg.UpstreamModel
	}
	return requested
}

func (s *Server) proxyUpstream(ctx context.Context, method, endpoint string, body []byte, accept string) (*http.Response, error) {
	url := upstreamurl.BuildOpenAIEndpointURL(s.cfg.UpstreamBaseURL, endpoint)
	req, err := http.NewRequestWithContext(ctx, method, url, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	if accept != "" {
		req.Header.Set("Accept", accept)
	}
	if len(body) > 0 {
		req.Header.Set("Content-Type", "application/json")
	}
	if s.cfg.UpstreamAPIKey != "" {
		req.Header.Set("Authorization", "Bearer "+s.cfg.UpstreamAPIKey)
	}
	return s.cfg.HTTPClient.Do(req)
}

func (s *Server) writeUpstreamError(w http.ResponseWriter, upstream *http.Response) {
	body, _ := io.ReadAll(upstream.Body)
	message := extractErrorMessage(body)
	if message == "" {
		message = strings.TrimSpace(string(body))
	}
	if message == "" {
		message = http.StatusText(upstream.StatusCode)
	}
	status := upstream.StatusCode
	if status < 400 {
		status = http.StatusBadGateway
	}
	errType := "api_error"
	switch status {
	case http.StatusBadRequest, http.StatusUnprocessableEntity:
		errType = "invalid_request_error"
	case http.StatusUnauthorized:
		errType = "authentication_error"
	case http.StatusForbidden:
		errType = "permission_error"
	case http.StatusTooManyRequests:
		errType = "rate_limit_error"
	}
	writeAnthropicError(w, status, errType, message)
}

func (s *Server) handleNonStream(w http.ResponseWriter, body io.Reader, originalModel string) {
	respBody, err := io.ReadAll(body)
	if err != nil {
		writeAnthropicError(w, http.StatusBadGateway, "api_error", "failed to read upstream response")
		return
	}

	var ccResp apicompat.ChatCompletionsResponse
	if err := json.Unmarshal(respBody, &ccResp); err != nil {
		writeAnthropicError(w, http.StatusBadGateway, "api_error", "failed to parse upstream response")
		return
	}

	responsesResp := apicompat.ChatCompletionsResponseToResponses(&ccResp, originalModel)
	anthropicResp := apicompat.ResponsesToAnthropic(responsesResp, originalModel)
	writeJSON(w, http.StatusOK, anthropicResp)
}

func (s *Server) handleStream(w http.ResponseWriter, body io.Reader, originalModel string) {
	flusher, _ := w.(http.Flusher)
	headersWritten := false
	writeStreamHeaders := func() {
		if headersWritten {
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
		w.Header().Set("Connection", "keep-alive")
		w.Header().Set("X-Accel-Buffering", "no")
		w.WriteHeader(http.StatusOK)
		headersWritten = true
	}

	chatState := apicompat.NewChatCompletionsToResponsesStreamState(originalModel)
	anthState := apicompat.NewResponsesEventToAnthropicState()
	anthState.Model = originalModel

	writeAnthropicEvents := func(events []apicompat.AnthropicStreamEvent) {
		if len(events) == 0 {
			return
		}
		writeStreamHeaders()
		for _, evt := range events {
			sse, err := apicompat.ResponsesAnthropicEventToSSE(evt)
			if err != nil {
				continue
			}
			if _, err := io.WriteString(w, sse); err != nil {
				return
			}
		}
		if flusher != nil {
			flusher.Flush()
		}
	}

	scanner := bufio.NewScanner(body)
	scanner.Buffer(make([]byte, 0, 64*1024), 32*1024*1024)

	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || !strings.HasPrefix(line, "data:") {
			continue
		}
		payload := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if payload == "[DONE]" {
			break
		}

		var chunk apicompat.ChatCompletionsChunk
		if err := json.Unmarshal([]byte(payload), &chunk); err != nil {
			continue
		}
		respEvents := apicompat.ChatCompletionsChunkToResponsesEvents(&chunk, chatState)
		for _, respEvent := range respEvents {
			anthEvents := apicompat.ResponsesEventToAnthropicEvents(&respEvent, anthState)
			writeAnthropicEvents(anthEvents)
		}
	}

	for _, respEvent := range apicompat.FinalizeChatCompletionsResponsesStream(chatState) {
		anthEvents := apicompat.ResponsesEventToAnthropicEvents(&respEvent, anthState)
		writeAnthropicEvents(anthEvents)
	}
	writeAnthropicEvents(apicompat.FinalizeResponsesAnthropicStream(anthState))
}

func copyUpstreamResponse(w http.ResponseWriter, resp *http.Response) {
	for k, vv := range resp.Header {
		for _, v := range vv {
			w.Header().Add(k, v)
		}
	}
	w.WriteHeader(resp.StatusCode)
	_, _ = io.Copy(w, resp.Body)
}

func writeAnthropicError(w http.ResponseWriter, status int, errType, message string) {
	writeJSON(w, status, map[string]any{
		"type": "error",
		"error": map[string]string{
			"type":    errType,
			"message": message,
		},
	})
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func extractErrorMessage(body []byte) string {
	var payload struct {
		Error struct {
			Message string `json:"message"`
			Type    string `json:"type"`
		} `json:"error"`
		Message string `json:"message"`
	}
	if err := json.Unmarshal(body, &payload); err == nil {
		if msg := strings.TrimSpace(payload.Error.Message); msg != "" {
			return msg
		}
		if msg := strings.TrimSpace(payload.Message); msg != "" {
			return msg
		}
	}
	return ""
}

func streamAcceptHeader(stream bool) string {
	if stream {
		return "text/event-stream"
	}
	return "application/json"
}
