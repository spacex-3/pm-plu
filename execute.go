package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"
)

type executorRequest struct {
	Model       string `json:"Model"`
	Stream      bool   `json:"Stream"`
	Payload     []byte `json:"Payload"`
	StorageJSON []byte `json:"StorageJSON"`
	StreamID    string `json:"stream_id"`
	CallbackID  string `json:"host_callback_id"`
}

func execute(raw []byte, stream bool) ([]byte, error) {
	if !loadedConfig().enabled() {
		return errorEnvelope("plugin_disabled", "prism-plugin is disabled", http.StatusForbidden), nil
	}
	var req executorRequest
	if err := json.Unmarshal(raw, &req); err != nil {
		return errorEnvelope("invalid_request", err.Error(), http.StatusBadRequest), nil
	}
	cred, _, err := parseCredential(req.StorageJSON)
	if err != nil {
		return errorEnvelope("invalid_api_key", err.Error(), http.StatusUnauthorized), nil
	}
	if cred.Cookie == "" || cred.SandboxToken == "" {
		return errorEnvelope("invalid_api_key", "prism auth is not configured (cookie and sandbox_token required)", http.StatusUnauthorized), nil
	}
	source := map[string]any{}
	if len(req.Payload) > 0 {
		object, err := decodeObject(req.Payload)
		if err != nil {
			return errorEnvelope("invalid_request", "request payload is not a JSON object", http.StatusBadRequest), nil
		}
		source = object
	}
	if req.Model != "" && stringField(source, "model") == "" {
		source["model"] = req.Model
	}
	requested := publicModelID(stringField(source, "model"))
	upstream := upstreamModelID(stringField(source, "model"))
	effort := requestEffort(source)
	conv := requestConversation(source)
	tools := clientToolEntries(source)
	budget := time.Duration(loadedConfig().timeoutSeconds()) * time.Second

	if stream {
		return startPrismStream(req, cred, conv, requested, upstream, effort, budget, tools)
	}
	return executeOnce(cred, conv, requested, upstream, effort, budget, tools)
}

func requestConversation(source map[string]any) conversation {
	if _, hasMessages := source["messages"]; hasMessages {
		if _, hasInput := source["input"]; !hasInput {
			return flattenChat(source)
		}
	}
	return flattenResponses(source)
}

func requestEffort(source map[string]any) string {
	if effort := normalizeEffort(source["reasoning_effort"]); effort != "" {
		return effort
	}
	if reasoning, ok := source["reasoning"].(map[string]any); ok {
		if effort := normalizeEffort(reasoning["effort"]); effort != "" {
			return effort
		}
	}
	if effort := effortFromModelSuffix(stringField(source, "model")); effort != "" {
		return effort
	}
	return loadedConfig().effort()
}

// executeOnce runs one Prism turn and returns a completed Responses payload.
func executeOnce(cred prismCredential, conv conversation, requested, upstream, effort string, budget time.Duration, tools []toolEntry) ([]byte, error) {
	text, err := runPrismTurn(cred, conv.System, conv.User, upstream, effort, budget)
	if err != nil {
		return errorEnvelope("upstream_error", err.Error(), http.StatusBadGateway), nil
	}
	item, measured := responseItem(text, tools)
	base := buildResponse(requested, []any{item}, conv, measured)
	encoded, err := marshalCompact(base)
	if err != nil {
		return errorEnvelope("upstream_error", err.Error(), http.StatusBadGateway), nil
	}
	return okEnvelope(map[string]any{
		"Payload": encoded,
		"Headers": map[string][]string{"Content-Type": {"application/json"}},
	})
}

// startPrismStream answers immediately with response.created so clients show
// "thinking", then synthesises the remaining SSE frames once Prism completes.
func startPrismStream(req executorRequest, cred prismCredential, conv conversation, requested, upstream, effort string, budget time.Duration, tools []toolEntry) ([]byte, error) {
	if strings.TrimSpace(req.StreamID) == "" {
		return errorEnvelope("executor_error", "stream_id is required for executor.execute_stream", http.StatusBadRequest), nil
	}
	shell := map[string]any{
		"id": "resp_" + randomHex(12), "object": "response",
		"created_at": nowUnix(), "model": requested,
		"status": "in_progress", "output": []any{}, "usage": nil,
	}
	if err := emitPluginStream(req.StreamID, encodeSSE("response.created", map[string]any{"type": "response.created", "response": shell})); err != nil {
		return errorEnvelope("executor_error", err.Error(), http.StatusInternalServerError), nil
	}
	if err := emitPluginStream(req.StreamID, encodeSSE("response.in_progress", map[string]any{"type": "response.in_progress", "response": shell})); err != nil {
		return errorEnvelope("executor_error", err.Error(), http.StatusInternalServerError), nil
	}

	go func() {
		defer func() {
			if recovered := recover(); recovered != nil {
				closePluginStream(req.StreamID, fmt.Sprintf("stream panic: %v", recovered))
			}
		}()
		text, err := runPrismTurn(cred, conv.System, conv.User, upstream, effort, budget)
		if err != nil {
			closePluginStream(req.StreamID, err.Error())
			return
		}
		item, measured := responseItem(text, tools)
		base := map[string]any{}
		for key, value := range shell {
			base[key] = value
		}
		base["status"] = "completed"
		base["output"] = []any{item}
		inputTokens, outputTokens := estimatedUsage(conv.System, conv.User, measured)
		base["usage"] = usageObject(inputTokens, outputTokens)
		isTool := stringField(item, "type") != "message"
		textOut := ""
		if !isTool {
			textOut = messageItemText(item)
		}
		for _, frame := range synthesizeStreamFrames(base, item, textOut, isTool) {
			if err := emitPluginStream(req.StreamID, frame); err != nil {
				closePluginStream(req.StreamID, err.Error())
				return
			}
		}
		closePluginStream(req.StreamID, "")
	}()
	return okEnvelope(map[string]any{
		"headers": map[string][]string{"Content-Type": {"text/event-stream"}},
	})
}

// responseItem converts the finished Prism text into one Responses output item:
// either a function/custom tool call recovered from the emitter protocol, or an
// assistant message.
func responseItem(text string, tools []toolEntry) (map[string]any, string) {
	var call *parsedToolCall
	if len(tools) > 0 {
		call = parseToolCall(text)
	}
	if call == nil {
		if done, ok := parseDone(text); ok {
			text = done
		}
		item := map[string]any{
			"type": "message", "id": "msg_" + randomHex(12), "role": "assistant",
			"status":  "completed",
			"content": []any{map[string]any{"type": "output_text", "text": text, "annotations": []any{}}},
		}
		return item, text
	}
	name := call.Name
	entry := lookupTool(tools, call.Name)
	if entry != nil {
		name = entry.Name
	}
	if call.Custom {
		item := map[string]any{
			"type": "custom_tool_call", "id": "ctc_" + call.ID, "call_id": call.ID,
			"name": name, "input": call.Input, "status": "completed",
		}
		if entry != nil && entry.Namespace != "" && entry.Namespace != "functions" {
			item["namespace"] = entry.Namespace
		}
		return item, mustCompact(call)
	}
	item := map[string]any{
		"type": "function_call", "id": functionItemID(call.ID), "call_id": call.ID,
		"name": name, "arguments": call.Arguments, "status": "completed",
	}
	if entry != nil && entry.Namespace != "" && entry.Namespace != "functions" {
		item["namespace"] = entry.Namespace
	}
	return item, mustCompact(call)
}

func lookupTool(tools []toolEntry, name string) *toolEntry {
	for i := range tools {
		if tools[i].Name == name {
			return &tools[i]
		}
	}
	if idx := strings.LastIndex(name, "."); idx >= 0 {
		suffix := name[idx+1:]
		for i := range tools {
			if tools[i].Name == suffix {
				return &tools[i]
			}
		}
	}
	return nil
}

func messageItemText(item map[string]any) string {
	content, _ := item["content"].([]any)
	var parts []string
	for _, part := range content {
		if object, ok := part.(map[string]any); ok {
			if text, ok := object["text"].(string); ok {
				parts = append(parts, text)
			}
		}
	}
	return strings.Join(parts, "")
}

func buildResponse(model string, output []any, conv conversation, measured string) map[string]any {
	inputTokens, outputTokens := estimatedUsage(conv.System, conv.User, measured)
	return map[string]any{
		"id": "resp_" + randomHex(12), "object": "response",
		"created_at": nowUnix(), "model": model,
		"status": "completed", "output": output,
		"usage": usageObject(inputTokens, outputTokens),
	}
}

func usageObject(inputTokens, outputTokens int) map[string]any {
	return map[string]any{
		"input_tokens": inputTokens, "output_tokens": outputTokens,
		"total_tokens":          inputTokens + outputTokens,
		"input_tokens_details":  map[string]any{"cached_tokens": 0},
		"output_tokens_details": map[string]any{"reasoning_tokens": 0},
	}
}

func functionItemID(callID string) string {
	candidate := "fc_" + callID
	if len(candidate) <= maxItemIDLength {
		return candidate
	}
	sum := sha256.Sum256([]byte(callID))
	return "fc_" + hex.EncodeToString(sum[:])[:maxItemIDLength-len("fc_")]
}

const maxItemIDLength = 64
