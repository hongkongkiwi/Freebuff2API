package main

import (
	"bytes"
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// defaultMaxRequestBodyBytes caps client request bodies unless
// MAX_REQUEST_BODY_MB is configured.
const defaultMaxRequestBodyBytes = 32 << 20

// defaultUpstreamStop is the stop sentinel the free-tier backend expects when
// the client did not provide one (a JSON string containing double quotes).
const defaultUpstreamStop = "\"cb_easp\""

// maxProxyAttempts bounds session/run-invalid retries and cross-pool failover
// for a single client request.
const maxProxyAttempts = 3

var (
	retryAfterMsPattern = regexp.MustCompile(`"retryAfterMs"\s*:\s*(\d+)`)
	tryAgainTextPattern = regexp.MustCompile(`(?i)try again in(?:\s+(\d+)\s*h)?(?:\s+(\d+)\s*m)?(?:\s+(\d+)\s*s)?`)
)

const (
	defaultRateLimitCooldown = 5 * time.Minute
	otherErrorCooldown       = time.Minute
	maxUpstreamCooldown      = 6 * time.Hour
)

// parseUpstreamCooldown extracts a pool cooldown from a rate-limit error body:
// a retryAfterMs field, a "try again in Xh Ym Zs" hint, or a status default.
func parseUpstreamCooldown(body []byte, status int) time.Duration {
	if match := retryAfterMsPattern.FindSubmatch(body); match != nil {
		if ms, err := strconv.ParseInt(string(match[1]), 10, 64); err == nil && ms > 0 {
			return clampCooldown(time.Duration(ms) * time.Millisecond)
		}
	}
	if match := tryAgainTextPattern.FindSubmatch(body); match != nil {
		hours := atoiSubmatch(match[1])
		minutes := atoiSubmatch(match[2])
		seconds := atoiSubmatch(match[3])
		if total := time.Duration(hours)*time.Hour + time.Duration(minutes)*time.Minute + time.Duration(seconds)*time.Second; total > 0 {
			return clampCooldown(total)
		}
	}
	if status == http.StatusTooManyRequests {
		return defaultRateLimitCooldown
	}
	return otherErrorCooldown
}

func clampCooldown(d time.Duration) time.Duration {
	if d > maxUpstreamCooldown {
		return maxUpstreamCooldown
	}
	if d <= 0 {
		return defaultRateLimitCooldown
	}
	return d
}

func atoiSubmatch(match []byte) int {
	value, _ := strconv.Atoi(string(match))
	return value
}

type Server struct {
	cfg      Config
	logger   *log.Logger
	client   *UpstreamClient
	runs     *RunManager
	registry *ModelRegistry
	started  time.Time
}

func NewServer(cfg Config, logger *log.Logger, registry *ModelRegistry) *Server {
	client := NewUpstreamClient(cfg)
	runManager := NewRunManager(cfg, client, logger)

	return &Server{
		cfg:      cfg,
		logger:   logger,
		client:   client,
		runs:     runManager,
		registry: registry,
		started:  time.Now(),
	}
}

func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", s.handleHealthz)
	mux.HandleFunc("/v1/models", s.handleModels)
	mux.HandleFunc("/v1/chat/completions", s.handleChatCompletions)
	mux.HandleFunc("/v1/messages", s.handleClaudeMessages)
	mux.HandleFunc("/v1/messages/count_tokens", s.handleClaudeCountTokens)
	return s.withMiddleware(mux)
}

func (s *Server) Start(ctx context.Context) {
	s.runs.Start(ctx, s.registry.AgentIDs())
}

func (s *Server) Shutdown(ctx context.Context) {
	s.runs.Close(ctx)
}

func (s *Server) withMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if len(s.cfg.APIKeys) > 0 && !s.authorized(r) {
			if isClaudeRequestPath(r.URL.Path) {
				writeClaudeError(w, http.StatusUnauthorized, "invalid proxy api key", "authentication_error")
			} else {
				writeOpenAIError(w, http.StatusUnauthorized, "invalid proxy api key", "authentication_error", "")
			}
			return
		}
		if r.Method == http.MethodPost && r.Body != nil {
			r.Body = http.MaxBytesReader(w, r.Body, s.maxBodyBytes())
		}
		next.ServeHTTP(w, r)
	})
}

func (s *Server) maxBodyBytes() int64 {
	if s.cfg.MaxRequestBodyMB > 0 {
		return int64(s.cfg.MaxRequestBodyMB) * 1024 * 1024
	}
	return defaultMaxRequestBodyBytes
}

func isBodyTooLarge(err error) bool {
	var maxBytesErr *http.MaxBytesError
	return errors.As(err, &maxBytesErr)
}

func readOpenAIBody(w http.ResponseWriter, r *http.Request) ([]byte, bool) {
	requestBody, err := io.ReadAll(r.Body)
	if err != nil {
		if isBodyTooLarge(err) {
			writeOpenAIError(w, http.StatusRequestEntityTooLarge, "request body too large", "invalid_request_error", "")
		} else {
			writeOpenAIError(w, http.StatusBadRequest, "failed to read request body", "invalid_request_error", "")
		}
		return nil, false
	}
	return requestBody, true
}

func readClaudeBody(w http.ResponseWriter, r *http.Request) ([]byte, bool) {
	requestBody, err := io.ReadAll(r.Body)
	if err != nil {
		if isBodyTooLarge(err) {
			writeClaudeError(w, http.StatusRequestEntityTooLarge, "request body too large", "invalid_request_error")
		} else {
			writeClaudeError(w, http.StatusBadRequest, "failed to read request body", "invalid_request_error")
		}
		return nil, false
	}
	return requestBody, true
}

func (s *Server) authorized(r *http.Request) bool {
	provided := strings.TrimSpace(r.Header.Get("x-api-key"))
	if provided == "" {
		authorization := strings.TrimSpace(r.Header.Get("Authorization"))
		const prefix = "Bearer "
		if strings.HasPrefix(authorization, prefix) {
			provided = strings.TrimSpace(strings.TrimPrefix(authorization, prefix))
		}
	}
	if provided == "" {
		return false
	}
	return containsKeyConstantTime(s.cfg.APIKeys, provided)
}

// containsKeyConstantTime compares the provided key against every configured
// key in constant time so response latency does not leak which prefix matched.
func containsKeyConstantTime(keys []string, provided string) bool {
	var matched bool
	for _, key := range keys {
		if subtle.ConstantTimeCompare([]byte(key), []byte(provided)) == 1 {
			matched = true
		}
	}
	return matched
}

func isClaudeRequestPath(path string) bool {
	return strings.HasPrefix(path, "/v1/messages")
}

func (s *Server) handleHealthz(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeOpenAIError(w, http.StatusMethodNotAllowed, "method not allowed", "invalid_request_error", "")
		return
	}

	response := map[string]any{
		"ok":             true,
		"started_at":     s.started.UTC(),
		"uptime_sec":     int(time.Since(s.started).Seconds()),
		"model_registry": s.registry.Status(),
		"token_state":    s.runs.Snapshots(),
	}
	writeJSON(w, http.StatusOK, response)
}

func (s *Server) handleModels(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeOpenAIError(w, http.StatusMethodNotAllowed, "method not allowed", "invalid_request_error", "")
		return
	}

	created := s.started.Unix()
	modelsList := s.registry.Models()
	models := make([]map[string]any, 0, len(modelsList))
	for _, model := range modelsList {
		models = append(models, map[string]any{
			"id":         model,
			"object":     "model",
			"created":    created,
			"owned_by":   "Freebuff2API",
			"root":       model,
			"permission": []any{},
		})
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"object": "list",
		"data":   models,
	})
}

func (s *Server) handleChatCompletions(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeOpenAIError(w, http.StatusMethodNotAllowed, "method not allowed", "invalid_request_error", "")
		return
	}

	requestBody, ok := readOpenAIBody(w, r)
	if !ok {
		return
	}

	var payload map[string]any
	if err := json.Unmarshal(requestBody, &payload); err != nil {
		writeOpenAIError(w, http.StatusBadRequest, "request body must be valid JSON", "invalid_request_error", "")
		return
	}

	requestedModel, _ := payload["model"].(string)
	requestedModel = strings.TrimSpace(requestedModel)
	if requestedModel == "" {
		requestedModel = s.registry.DefaultModel()
	}

	clientStream := boolValue(payload["stream"])

	s.proxyChatRequest(
		w,
		r,
		payload,
		requestedModel,
		"invalid_request_error",
		"server_error",
		writeOpenAIError,
		writePassthroughError,
		func(w http.ResponseWriter, resp *http.Response) error {
			return s.writeOpenAISuccess(w, resp, clientStream)
		},
	)
}

func (s *Server) handleClaudeMessages(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeClaudeError(w, http.StatusMethodNotAllowed, "method not allowed", "invalid_request_error")
		return
	}

	requestBody, ok := readClaudeBody(w, r)
	if !ok {
		return
	}

	payload, requestedModel, stream, err := convertClaudeMessagesRequestToOpenAI(requestBody)
	if err != nil {
		writeClaudeError(w, http.StatusBadRequest, err.Error(), "invalid_request_error")
		return
	}

	if _, ok := s.registry.AgentForModel(requestedModel); !ok {
		writeClaudeError(w, http.StatusBadRequest, fmt.Sprintf("unsupported model %q", requestedModel), "invalid_request_error")
		return
	}

	s.proxyChatRequest(
		w,
		r,
		payload,
		requestedModel,
		"invalid_request_error",
		"api_error",
		func(w http.ResponseWriter, statusCode int, message, errorType, _ string) {
			writeClaudeError(w, statusCode, message, errorType)
		},
		writeClaudePassthroughError,
		func(w http.ResponseWriter, resp *http.Response) error {
			return s.writeClaudeSuccess(w, resp, requestedModel, stream)
		},
	)
}

func (s *Server) handleClaudeCountTokens(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeClaudeError(w, http.StatusMethodNotAllowed, "method not allowed", "invalid_request_error")
		return
	}

	requestBody, ok := readClaudeBody(w, r)
	if !ok {
		return
	}

	payload, requestedModel, _, err := convertClaudeMessagesRequestToOpenAI(requestBody)
	if err != nil {
		writeClaudeError(w, http.StatusBadRequest, err.Error(), "invalid_request_error")
		return
	}

	if !s.registry.HasModel(requestedModel) {
		writeClaudeError(w, http.StatusBadRequest, fmt.Sprintf("unsupported model %q", requestedModel), "invalid_request_error")
		return
	}

	count, err := countOpenAIPayloadTokens(requestedModel, payload)
	if err != nil {
		s.logger.Printf("count_tokens failed for model %s: %v", requestedModel, err)
		writeClaudeError(w, http.StatusBadGateway, "failed to estimate input tokens", "api_error")
		return
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"input_tokens": count,
	})
}

func (s *Server) proxyChatRequest(
	w http.ResponseWriter,
	r *http.Request,
	payload map[string]any,
	requestedModel string,
	invalidRequestType string,
	serverErrorType string,
	writeError func(http.ResponseWriter, int, string, string, string),
	writeUpstreamError func(http.ResponseWriter, int, []byte),
	writeSuccess func(http.ResponseWriter, *http.Response) error,
) {
	startTime := time.Now()

	var lastUpstreamErrStatus int
	var lastUpstreamErrBody []byte
	recordUpstreamError := func(status int, body []byte) {
		lastUpstreamErrStatus = status
		lastUpstreamErrBody = body
	}
	// surfaceUpstreamError re-emits the most recent upstream error (with its
	// Retry-After) when retrying on other pools did not produce a response.
	surfaceUpstreamError := func() bool {
		if lastUpstreamErrStatus == 0 {
			return false
		}
		if cooldown := parseUpstreamCooldown(lastUpstreamErrBody, lastUpstreamErrStatus); cooldown > 0 {
			w.Header().Set("Retry-After", fmt.Sprintf("%.0f", cooldown.Seconds()))
		}
		writeUpstreamError(w, lastUpstreamErrStatus, lastUpstreamErrBody)
		return true
	}

	agentID, ok := s.registry.AgentForModel(requestedModel)
	if !ok {
		writeError(w, http.StatusBadRequest, fmt.Sprintf("unsupported model %q", requestedModel), invalidRequestType, "model_not_found")
		return
	}

	for attempt := 0; attempt < maxProxyAttempts; attempt++ {
		lease, err := s.runs.Acquire(r.Context(), agentID)
		if err != nil {
			var waitingErr *waitingRoomError
			if errors.As(err, &waitingErr) {
				if waitingErr.RetryAfter > 0 {
					w.Header().Set("Retry-After", fmt.Sprintf("%.0f", waitingErr.RetryAfter.Seconds()))
				}
				writeError(w, http.StatusServiceUnavailable, waitingErr.Error(), serverErrorType, "waiting_room_queued")
				return
			}
			if surfaceUpstreamError() {
				return
			}
			writeError(w, http.StatusBadGateway, "no healthy upstream auth token available", serverErrorType, "")
			return
		}

		s.logger.Printf("[%s] Routing request (model: %s) via run: %s", lease.pool.name, requestedModel, lease.run.id)

		sessionInstanceID, err := lease.pool.ensureSession(r.Context())
		if err != nil {
			s.runs.Release(lease)
			var waitingErr *waitingRoomError
			if errors.As(err, &waitingErr) {
				if waitingErr.RetryAfter > 0 {
					w.Header().Set("Retry-After", fmt.Sprintf("%.0f", waitingErr.RetryAfter.Seconds()))
				}
				writeError(w, http.StatusServiceUnavailable, waitingErr.Error(), serverErrorType, "waiting_room_queued")
				return
			}
			writeError(w, http.StatusBadGateway, "failed to acquire upstream free session", serverErrorType, "")
			return
		}

		upstreamBody, err := s.injectUpstreamMetadata(lease.pool, payload, requestedModel, lease.run.id, sessionInstanceID)
		if err != nil {
			s.runs.Release(lease)
			writeError(w, http.StatusBadRequest, err.Error(), invalidRequestType, "")
			return
		}

		// One in-flight upstream chat per account: wait for this pool's slot
		// and the minimum inter-call gap before touching the upstream.
		slotRelease, err := lease.pool.acquireChatSlot(r.Context())
		if err != nil {
			s.runs.Release(lease)
			writeError(w, http.StatusGatewayTimeout, "timed out waiting for the account's upstream slot", serverErrorType, "")
			return
		}

		resp, errorBody, err := s.client.ChatCompletions(r.Context(), lease.pool.token, upstreamBody)
		if err != nil {
			slotRelease()
			s.runs.Release(lease)
			writeError(w, http.StatusBadGateway, err.Error(), serverErrorType, "")
			return
		}

		if resp.StatusCode >= 200 && resp.StatusCode < 300 {
			s.runs.ReportSuccess(lease)
			writeErr := writeSuccess(w, resp)
			if closeErr := resp.Body.Close(); closeErr != nil {
				s.logger.Printf("[%s] upstream body close failed: %v", lease.pool.name, closeErr)
			}
			slotRelease()
			if errors.Is(writeErr, errBlankUpstreamStream) && attempt < maxProxyAttempts-1 {
				s.logger.Printf("[%s] upstream returned a blank stream, refreshing session and retrying", lease.pool.name)
				lease.pool.invalidateSession("blank upstream stream")
				s.runs.Release(lease)
				continue
			}
			if writeErr != nil && !errors.Is(writeErr, context.Canceled) {
				s.logger.Printf("[%s] proxy response copy failed: %v", lease.pool.name, writeErr)
			}
			s.logger.Printf("[%s] Request completed in %v (status: %d)", lease.pool.name, time.Since(startTime).Round(time.Millisecond), resp.StatusCode)
			s.runs.Release(lease)
			return
		}

		if isSessionInvalid(resp.StatusCode, errorBody) {
			s.logger.Printf("%s: free session invalid, refreshing and retrying", lease.pool.name)
			lease.pool.invalidateSession(strings.TrimSpace(string(errorBody)))
			slotRelease()
			s.runs.Release(lease)
			continue
		}

		if isRunInvalid(resp.StatusCode, errorBody) {
			s.logger.Printf("%s: run %s invalid, rotating and retrying", lease.pool.name, lease.run.id)
			s.runs.Invalidate(lease, strings.TrimSpace(string(errorBody)))
			slotRelease()
			s.runs.Release(lease)
			continue
		}

		if resp.StatusCode == http.StatusUnauthorized {
			s.runs.Cooldown(lease, 30*time.Minute, "upstream auth rejected token")
			lease.pool.invalidateSession("upstream auth rejected token")
			slotRelease()
			s.runs.Release(lease)
			writeUpstreamError(w, resp.StatusCode, errorBody)
			return
		}

		if resp.StatusCode == http.StatusTooManyRequests {
			cooldown := parseUpstreamCooldown(errorBody, resp.StatusCode)
			s.logger.Printf("%s: rate limited by upstream, cooling down for %s", lease.pool.name, cooldown.Round(time.Second))
			s.runs.Cooldown(lease, cooldown, fmt.Sprintf("rate limited by upstream (cooldown %s)", cooldown.Round(time.Second)))
			lease.pool.invalidateSession("rate limited by upstream")
			slotRelease()
			s.runs.Release(lease)
			recordUpstreamError(resp.StatusCode, errorBody)
			continue
		}

		// Any other upstream failure counts toward the per-pool circuit breaker.
		s.runs.ReportFailure(lease, fmt.Sprintf("upstream status %d", resp.StatusCode))

		slotRelease()
		s.runs.Release(lease)
		s.logger.Printf("[%s] upstream error response: %s", lease.pool.name, string(errorBody))
		writeUpstreamError(w, resp.StatusCode, errorBody)
		return
	}

	surfaceUpstreamError()
}

// writeOpenAISuccess serves a chat completion: streaming clients get the SSE
// relay (with camouflage stripped per chunk), non-streaming clients get a
// response reassembled from the forced upstream stream.
func (s *Server) writeOpenAISuccess(w http.ResponseWriter, resp *http.Response, clientStream bool) error {
	if clientStream {
		return writeOpenAIStreamingResponse(w, resp, s.cfg.ToolCamouflage)
	}
	final, err := reassembleOpenAIStreamResponse(resp.Body, s.cfg.ToolCamouflage)
	if err != nil {
		return err
	}
	body, err := json.Marshal(final)
	if err != nil {
		return err
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(resp.StatusCode)
	_, err = w.Write(body)
	return err
}

// writeClaudeSuccess serves a Claude Messages response converted from the
// forced upstream OpenAI stream.
func (s *Server) writeClaudeSuccess(w http.ResponseWriter, resp *http.Response, requestedModel string, clientStream bool) error {
	if clientStream {
		return writeClaudeStreamingResponse(w, resp, requestedModel)
	}
	final, err := reassembleOpenAIStreamResponse(resp.Body, false)
	if err != nil {
		return err
	}
	body, err := json.Marshal(final)
	if err != nil {
		return err
	}
	converted, err := convertOpenAINonStreamResponseToClaude(body)
	if err != nil {
		return err
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(resp.StatusCode)
	_, err = w.Write(converted)
	return err
}

func (s *Server) injectUpstreamMetadata(pool *tokenPool, payload map[string]any, requestedModel, runID, sessionInstanceID string) ([]byte, error) {
	cloned := cloneMap(payload)
	cloned["model"] = requestedModel

	// Normalize tool parameter schemas into a conservative subset the upstream
	// backend can parse. This keeps LobeChat-style schemas working without
	// changing non-tool requests.
	if tools, ok := cloned["tools"].([]any); ok {
		normalizeToolSchemas(tools)
	}

	// Defaults the free-tier backend expects; clients usually omit them.
	if value, ok := cloned["stop"]; !ok || value == nil {
		cloned["stop"] = []any{defaultUpstreamStop}
	}
	cloned["provider"] = map[string]any{"data_collection": "deny"}

	if s.cfg.HarnessRewrites {
		rewriteHarnessPrompts(cloned)
	}
	if s.cfg.BuffyGuard {
		ensureBuffySystemPrompt(cloned)
	}
	if s.cfg.ForceUpstreamStream {
		forceUpstreamStreaming(cloned)
	}
	if tools, ok := cloned["tools"].([]any); ok && len(tools) > 0 && s.cfg.ToolCamouflage {
		camouflageToolsForUpstream(cloned)
	}
	// Narrow the client's reasoning effort to the ladder the upstream catalog
	// publishes for this model (applies after Claude thinking→effort mapping).
	if effort, ok := cloned["reasoning_effort"].(string); ok {
		cloned["reasoning_effort"] = s.registry.ClampReasoningEffort(requestedModel, effort)
	}

	metadata, ok := cloned["codebuff_metadata"].(map[string]any)
	if !ok || metadata == nil {
		metadata = make(map[string]any)
	}
	metadata["run_id"] = runID
	metadata["cost_mode"] = "free"
	metadata["client_id"] = pool.clientID
	metadata["trace_session_id"] = pool.traceSession()
	if strings.TrimSpace(sessionInstanceID) != "" {
		metadata["freebuff_instance_id"] = sessionInstanceID
	}
	cloned["codebuff_metadata"] = metadata

	body, err := json.Marshal(cloned)
	if err != nil {
		return nil, fmt.Errorf("marshal upstream request: %w", err)
	}
	return body, nil
}

func isSessionInvalid(statusCode int, errorBody []byte) bool {
	if statusCode < 400 {
		return false
	}
	var payload struct {
		Error string `json:"error"`
	}
	if err := json.Unmarshal(errorBody, &payload); err != nil {
		return false
	}
	switch strings.TrimSpace(payload.Error) {
	case "freebuff_update_required", "waiting_room_required", "waiting_room_queued", "session_superseded", "session_expired", "session_model_mismatch", "model_locked":
		return true
	default:
		return false
	}
}

// normalizeToolSchemas rewrites tool parameter schemas into a conservative JSON
// Schema subset. Today that means resolving local $ref values and simplifying
// common nullable constructs emitted by clients like LobeChat.
func normalizeToolSchemas(tools []any) {
	for _, tool := range tools {
		toolMap, ok := tool.(map[string]any)
		if !ok {
			continue
		}
		fn, ok := toolMap["function"].(map[string]any)
		if !ok {
			continue
		}
		params, ok := fn["parameters"].(map[string]any)
		if !ok {
			continue
		}
		fn["parameters"] = normalizeSchemaMap(params, extractDefinitions(params), 12)
	}
}

// extractDefinitions returns the combined definitions map from "definitions" and "$defs".
func extractDefinitions(schema map[string]any) map[string]any {
	merged := make(map[string]any)
	if d, ok := schema["definitions"].(map[string]any); ok {
		for key, value := range d {
			merged[key] = value
		}
	}
	if d, ok := schema["$defs"].(map[string]any); ok {
		for key, value := range d {
			merged[key] = value
		}
	}
	if len(merged) == 0 {
		return nil
	}
	return merged
}

func mergeDefinitions(parent, local map[string]any) map[string]any {
	if len(parent) == 0 {
		return local
	}
	if len(local) == 0 {
		return parent
	}
	merged := make(map[string]any, len(parent)+len(local))
	for key, value := range parent {
		merged[key] = value
	}
	for key, value := range local {
		merged[key] = value
	}
	return merged
}

func normalizeSchemaValue(value any, defs map[string]any, maxDepth int) any {
	switch typed := value.(type) {
	case map[string]any:
		return normalizeSchemaMap(typed, defs, maxDepth)
	case []any:
		return normalizeSchemaSlice(typed, defs, maxDepth)
	default:
		return value
	}
}

func normalizeSchemaMap(node map[string]any, defs map[string]any, maxDepth int) map[string]any {
	if maxDepth <= 0 {
		return cloneMap(node)
	}

	defs = mergeDefinitions(defs, extractDefinitions(node))
	if replaced := tryResolveRef(node, defs); replaced != nil {
		if replacedMap, ok := replaced.(map[string]any); ok {
			return normalizeSchemaMap(replacedMap, defs, maxDepth-1)
		}
		return cloneMap(node)
	}

	normalized := make(map[string]any, len(node))
	for key, value := range node {
		normalized[key] = normalizeSchemaValue(value, defs, maxDepth-1)
	}

	delete(normalized, "definitions")
	delete(normalized, "$defs")
	delete(normalized, "nullable")

	normalized = simplifyNullableCombinator(normalized, "anyOf")
	normalized = simplifyNullableCombinator(normalized, "oneOf")
	normalizeTypeField(normalized)
	normalizeEnumField(normalized)
	normalizeConstField(normalized)

	return normalized
}

func normalizeSchemaSlice(slice []any, defs map[string]any, maxDepth int) []any {
	if maxDepth <= 0 {
		return cloneSlice(slice)
	}
	normalized := make([]any, len(slice))
	for i, value := range slice {
		normalized[i] = normalizeSchemaValue(value, defs, maxDepth-1)
	}
	return normalized
}

func simplifyNullableCombinator(schema map[string]any, key string) map[string]any {
	rawOptions, ok := schema[key].([]any)
	if !ok {
		return schema
	}

	filtered := make([]any, 0, len(rawOptions))
	for _, option := range rawOptions {
		if optionMap, ok := option.(map[string]any); ok && isNullSchema(optionMap) {
			continue
		}
		filtered = append(filtered, option)
	}

	if len(filtered) == 0 {
		delete(schema, key)
		return schema
	}

	if len(filtered) == 1 {
		if optionMap, ok := filtered[0].(map[string]any); ok {
			merged := make(map[string]any, len(schema)+len(optionMap))
			for existingKey, existingValue := range schema {
				if existingKey == key {
					continue
				}
				merged[existingKey] = existingValue
			}
			for optionKey, optionValue := range optionMap {
				merged[optionKey] = optionValue
			}
			return merged
		}
	}

	schema[key] = filtered
	return schema
}

func normalizeTypeField(schema map[string]any) {
	rawType, ok := schema["type"]
	if !ok {
		return
	}
	if _, ok := rawType.(string); ok {
		return
	}
	types, ok := rawType.([]any)
	if !ok {
		return
	}
	nonNullTypes := make([]string, 0, len(types))
	for _, entry := range types {
		typeName, ok := entry.(string)
		if !ok || typeName == "null" || strings.TrimSpace(typeName) == "" {
			continue
		}
		nonNullTypes = append(nonNullTypes, typeName)
	}
	switch len(nonNullTypes) {
	case 0:
		delete(schema, "type")
	case 1:
		schema["type"] = nonNullTypes[0]
	default:
		// Upstream expects a single primitive type. Keep the first non-null type
		// rather than failing the whole request.
		schema["type"] = nonNullTypes[0]
	}
}

func normalizeEnumField(schema map[string]any) {
	enumValues, ok := schema["enum"].([]any)
	if !ok {
		return
	}
	filtered := make([]any, 0, len(enumValues))
	seen := make(map[string]struct{}, len(enumValues))
	for _, entry := range enumValues {
		if entry == nil {
			continue
		}
		key := fmt.Sprintf("%T:%v", entry, entry)
		if _, exists := seen[key]; exists {
			continue
		}
		seen[key] = struct{}{}
		filtered = append(filtered, entry)
	}
	if len(filtered) == 0 {
		delete(schema, "enum")
		return
	}
	schema["enum"] = filtered
}

func normalizeConstField(schema map[string]any) {
	if value, ok := schema["const"]; ok && value == nil {
		delete(schema, "const")
	}
}

func isNullSchema(schema map[string]any) bool {
	if typeName, ok := schema["type"].(string); ok && typeName == "null" {
		return true
	}
	if constValue, ok := schema["const"]; ok && constValue == nil {
		return true
	}
	if enumValues, ok := schema["enum"].([]any); ok && len(enumValues) == 1 && enumValues[0] == nil {
		return true
	}
	return false
}

// tryResolveRef checks if a node is a $ref object like {"$ref": "#/definitions/Foo"}
// and returns the cloned definition if found.
func tryResolveRef(node map[string]any, defs map[string]any) any {
	ref, ok := node["$ref"].(string)
	if !ok || len(node) != 1 {
		return nil
	}
	// Support both "#/definitions/X" and "#/$defs/X"
	var name string
	if strings.HasPrefix(ref, "#/definitions/") {
		name = strings.TrimPrefix(ref, "#/definitions/")
	} else if strings.HasPrefix(ref, "#/$defs/") {
		name = strings.TrimPrefix(ref, "#/$defs/")
	}
	if name == "" {
		return nil
	}
	def, ok := defs[name]
	if !ok {
		return nil
	}
	// Clone to avoid mutating the original definition
	if defMap, ok := def.(map[string]any); ok {
		return cloneMap(defMap)
	}
	return def
}

func cloneMap(input map[string]any) map[string]any {
	output := make(map[string]any, len(input))
	for key, value := range input {
		switch typed := value.(type) {
		case map[string]any:
			output[key] = cloneMap(typed)
		case []any:
			output[key] = cloneSlice(typed)
		default:
			output[key] = value
		}
	}
	return output
}

func cloneSlice(input []any) []any {
	output := make([]any, len(input))
	for index, value := range input {
		switch typed := value.(type) {
		case map[string]any:
			output[index] = cloneMap(typed)
		case []any:
			output[index] = cloneSlice(typed)
		default:
			output[index] = value
		}
	}
	return output
}

func isRunInvalid(statusCode int, body []byte) bool {
	if statusCode != http.StatusBadRequest {
		return false
	}
	message := strings.ToLower(string(body))
	return strings.Contains(message, "runid not found") || strings.Contains(message, "runid not running")
}

func writePassthroughError(w http.ResponseWriter, statusCode int, body []byte) {
	trimmed := bytes.TrimSpace(body)
	if len(trimmed) > 0 && json.Valid(trimmed) {
		message, errorType, code := extractUpstreamError(trimmed)
		writeOpenAIError(w, statusCode, message, errorType, code)
		return
	}
	writeOpenAIError(w, statusCode, strings.TrimSpace(string(trimmed)), "upstream_error", "")
}

func extractUpstreamError(body []byte) (message, errorType, code string) {
	var payload map[string]any
	if err := json.Unmarshal(body, &payload); err != nil {
		return strings.TrimSpace(string(body)), "upstream_error", ""
	}

	errorType = "upstream_error"

	if rawError, ok := payload["error"]; ok {
		switch typed := rawError.(type) {
		case string:
			code = typed
		case map[string]any:
			if value, ok := typed["message"].(string); ok && strings.TrimSpace(value) != "" {
				message = value
			}
			if value, ok := typed["type"].(string); ok && strings.TrimSpace(value) != "" {
				errorType = value
			}
			if value, ok := typed["code"].(string); ok && strings.TrimSpace(value) != "" {
				code = value
			}
		}
	}

	if value, ok := payload["message"].(string); ok && strings.TrimSpace(value) != "" {
		message = value
	}
	if message == "" {
		message = strings.TrimSpace(string(body))
	}
	return message, errorType, code
}

func writeOpenAIError(w http.ResponseWriter, statusCode int, message, errorType, code string) {
	if message == "" {
		message = http.StatusText(statusCode)
	}
	payload := map[string]any{
		"error": map[string]any{
			"message": message,
			"type":    errorType,
		},
	}
	if code != "" {
		payload["error"].(map[string]any)["code"] = code
	}
	writeJSON(w, statusCode, payload)
}

func writeJSON(w http.ResponseWriter, statusCode int, payload any) {
	body, err := json.Marshal(payload)
	if err != nil {
		http.Error(w, `{"error":{"message":"failed to encode response","type":"server_error"}}`, http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(statusCode)
	_, _ = w.Write(body)
}
