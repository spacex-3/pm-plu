package main

/*
#include <stdint.h>
#include <stdlib.h>

typedef struct {
    void* ptr;
    size_t len;
} cliproxy_buffer;

typedef int (*cliproxy_host_call_fn)(void*, const char*, const uint8_t*, size_t, cliproxy_buffer*);
typedef void (*cliproxy_host_free_fn)(void*, size_t);

typedef struct {
    uint32_t abi_version;
    void* host_ctx;
    cliproxy_host_call_fn call;
    cliproxy_host_free_fn free_buffer;
} cliproxy_host_api;

typedef int (*cliproxy_plugin_call_fn)(char*, uint8_t*, size_t, cliproxy_buffer*);
typedef void (*cliproxy_plugin_free_fn)(void*, size_t);
typedef void (*cliproxy_plugin_shutdown_fn)(void);

typedef struct {
    uint32_t abi_version;
    cliproxy_plugin_call_fn call;
    cliproxy_plugin_free_fn free_buffer;
    cliproxy_plugin_shutdown_fn shutdown;
} cliproxy_plugin_api;

extern int cliproxyPluginCall(char*, uint8_t*, size_t, cliproxy_buffer*);
extern void cliproxyPluginFree(void*, size_t);
extern void cliproxyPluginShutdown(void);

static const cliproxy_host_api* stored_host;

static void store_host_api(const cliproxy_host_api* host) {
    stored_host = host;
}

static int call_host_api(const char* method, const uint8_t* request, size_t request_len, cliproxy_buffer* response) {
    if (stored_host == NULL || stored_host->call == NULL) {
        return 1;
    }
    return stored_host->call(stored_host->host_ctx, method, request, request_len, response);
}

static void free_host_buffer(void* ptr, size_t len) {
    if (stored_host != NULL && stored_host->free_buffer != NULL && ptr != NULL) {
        stored_host->free_buffer(ptr, len);
    }
}
*/
import "C"

import (
	"encoding/json"
	"net/http"
	"unsafe"
)

const abiVersion uint32 = 1
const schemaVersion uint32 = 6

type envelope struct {
	OK     bool            `json:"ok"`
	Result json.RawMessage `json:"result,omitempty"`
	Error  *envelopeError  `json:"error,omitempty"`
}

type envelopeError struct {
	Code       string `json:"code"`
	Message    string `json:"message"`
	HTTPStatus int    `json:"http_status,omitempty"`
}

func main() {}

//export cliproxy_plugin_init
func cliproxy_plugin_init(host *C.cliproxy_host_api, plugin *C.cliproxy_plugin_api) C.int {
	if plugin == nil {
		return 1
	}
	C.store_host_api(host)
	plugin.abi_version = C.uint32_t(abiVersion)
	plugin.call = C.cliproxy_plugin_call_fn(C.cliproxyPluginCall)
	plugin.free_buffer = C.cliproxy_plugin_free_fn(C.cliproxyPluginFree)
	plugin.shutdown = C.cliproxy_plugin_shutdown_fn(C.cliproxyPluginShutdown)
	return 0
}

//export cliproxyPluginCall
func cliproxyPluginCall(method *C.char, request *C.uint8_t, requestLen C.size_t, response *C.cliproxy_buffer) C.int {
	if response != nil {
		response.ptr = nil
		response.len = 0
	}
	if method == nil {
		writeResponse(response, errorEnvelope("invalid_method", "method is required"))
		return 1
	}
	var requestBytes []byte
	if request != nil && requestLen > 0 {
		requestBytes = C.GoBytes(unsafe.Pointer(request), C.int(requestLen))
	}
	raw, errHandle := handleMethod(C.GoString(method), requestBytes)
	if errHandle != nil {
		writeResponse(response, errorEnvelope("plugin_error", errHandle.Error()))
		return 1
	}
	writeResponse(response, raw)
	return 0
}

//export cliproxyPluginFree
func cliproxyPluginFree(ptr unsafe.Pointer, _ C.size_t) {
	if ptr != nil {
		C.free(ptr)
	}
}

//export cliproxyPluginShutdown
func cliproxyPluginShutdown() {}

func handleMethod(method string, request []byte) ([]byte, error) {
	switch method {
	case "plugin.register", "plugin.reconfigure":
		if err := configure(request); err != nil {
			return nil, err
		}
		return okEnvelope(pluginRegistration())
	case "plugin.quiesce", "plugin.shutdown":
		return okEnvelope(map[string]any{})
	case "model.register", "model.static", "model.for_auth":
		return okEnvelope(modelResponse())
	case "auth.identifier", "executor.identifier":
		return okEnvelope(map[string]any{"identifier": providerID})
	case "auth.parse":
		return parseAuth(request)
	case "auth.login.start":
		return okEnvelope(map[string]any{
			"Provider": providerID,
			"URL":      "",
			"State":    "",
			"Message":  "Write a prism auth file with cookie, sandbox_url and sandbox_token. See README_CN.md.",
		})
	case "auth.login.poll":
		return okEnvelope(map[string]any{"Status": "error", "Message": "prism auth is file-based"})
	case "auth.refresh":
		return refreshAuth(request)
	case "executor.execute":
		return execute(request, false)
	case "executor.execute_stream":
		return execute(request, true)
	case "executor.count_tokens":
		return countTokens(request)
	default:
		return errorEnvelope("unknown_method", "unknown method: "+method), nil
	}
}

func pluginRegistration() map[string]any {
	return map[string]any{
		"schema_version": schemaVersion,
		"metadata": map[string]any{
			"Name":             pluginName,
			"Version":          pluginVersion,
			"Author":           "Zhao73",
			"GitHubRepository": "https://github.com/spacex-3/pm-plu",
			"ConfigFields": []any{
				map[string]any{"Name": "base_url", "Type": "string", "Description": "Prism web origin. Defaults to https://prism.openai.com."},
				map[string]any{"Name": "user_agent", "Type": "string", "Description": "User agent sent to Prism. Defaults to a recent Chrome on macOS."},
				map[string]any{"Name": "effort", "Type": "string", "Description": "Default reasoning effort: low, medium or high."},
				map[string]any{"Name": "timeout_seconds", "Type": "integer", "Description": "Budget for one Prism turn. Defaults to 240."},
				map[string]any{"Name": "concurrency", "Type": "integer", "Description": "Sandbox pool size, i.e. parallel Prism turns. Defaults to 1, capped at 60 (the Pro account upstream limit). All sandboxes share one cookie."},
				map[string]any{"Name": "keepalive_seconds", "Type": "integer", "Description": "Ping idle sandboxes every N seconds to keep them warm. Defaults to 600, 0 keeps the default."},
				map[string]any{"Name": "max_transcript_chars", "Type": "integer", "Description": "Transcript budget folded into the single user message. Defaults to 24000."},
				map[string]any{"Name": "model_aliases", "Type": "object", "Description": "Map a client model id to a Prism model. Example: gpt-5.5 -> gpt-5.6-sol."},
			},
		},
		"capabilities": map[string]any{
			"model_registrar":         true,
			"model_provider":          true,
			"auth_provider":           true,
			"executor":                true,
			"executor_model_scope":    "both",
			"executor_input_formats":  []string{"openai-response", "codex"},
			"executor_output_formats": []string{"openai-response", "codex"},
		},
	}
}

func modelResponse() map[string]any {
	if !loadedConfig().enabled() {
		return map[string]any{"Provider": providerID, "Models": []any{}}
	}
	models := make([]any, 0, len(prismModels)*2)
	seen := map[string]bool{}
	add := func(id string, model prismModel) {
		if id == "" || seen[id] {
			return
		}
		seen[id] = true
		models = append(models, modelInfo(id, model))
	}
	for _, model := range prismModels {
		add(model.PublicID, model)
		add(model.UpstreamID, model)
	}
	for _, alias := range configuredAliasIDs() {
		add(alias.ID, alias.Model)
	}
	return map[string]any{"Provider": providerID, "Models": models}
}

func modelInfo(id string, model prismModel) map[string]any {
	return map[string]any{
		"ID":                         id,
		"Object":                     "model",
		"OwnedBy":                    "prism",
		"DisplayName":                model.Display,
		"Description":                "Prism (prism.openai.com) route via the free-astra bridge.",
		"ContextLength":              model.Context,
		"MaxCompletionTokens":        int64(128000),
		"SupportedGenerationMethods": []string{"responses"},
		"SupportedParameters":        []string{"reasoning_effort", "tools"},
		"Thinking": map[string]any{
			"Levels":         reasoningEfforts,
			"ZeroAllowed":    false,
			"DynamicAllowed": false,
		},
		"UserDefined": true,
	}
}

func parseAuth(raw []byte) ([]byte, error) {
	var req struct {
		FileName string `json:"FileName"`
		RawJSON  []byte `json:"RawJSON"`
	}
	if len(raw) > 0 {
		_ = json.Unmarshal(raw, &req)
	}
	record, handled, err := parseAuthFile(req.RawJSON, req.FileName)
	if err != nil {
		return nil, err
	}
	if !handled {
		return okEnvelope(map[string]any{"Handled": false})
	}
	return okEnvelope(map[string]any{"Handled": true, "Auth": record})
}

func refreshAuth(raw []byte) ([]byte, error) {
	var req struct {
		StorageJSON []byte `json:"StorageJSON"`
		FileName    string `json:"FileName"`
	}
	if len(raw) > 0 {
		_ = json.Unmarshal(raw, &req)
	}
	record, err := refreshCredential(req.StorageJSON, req.FileName)
	if err != nil {
		return errorEnvelope("invalid_api_key", err.Error(), http.StatusUnauthorized), nil
	}
	return okEnvelope(map[string]any{"Auth": record, "NextRefreshAfter": record.NextRefreshAfter})
}

func countTokens(raw []byte) ([]byte, error) {
	var req executorRequest
	_ = json.Unmarshal(raw, &req)
	tokens := len(req.Payload) / 4
	payload, _ := marshalCompact(map[string]any{"input_tokens": tokens, "total_tokens": tokens})
	return okEnvelope(map[string]any{"Payload": payload})
}

func callHost(method string, payload any) (json.RawMessage, error) {
	body, err := marshalCompact(payload)
	if err != nil {
		return nil, err
	}
	cMethod := C.CString(method)
	defer C.free(unsafe.Pointer(cMethod))
	var req *C.uint8_t
	if len(body) > 0 {
		copied := C.CBytes(body)
		defer C.free(copied)
		req = (*C.uint8_t)(copied)
	}
	var response C.cliproxy_buffer
	rc := C.call_host_api(cMethod, req, C.size_t(len(body)), &response)
	var raw []byte
	if response.ptr != nil && response.len > 0 {
		raw = C.GoBytes(response.ptr, C.int(response.len))
		C.free_host_buffer(response.ptr, response.len)
	}
	if rc != 0 {
		if len(raw) > 0 {
			if result, errUnwrap := unwrapHost(raw); errUnwrap != nil {
				return nil, errUnwrap
			} else if len(result) > 0 {
				return nil, errString(string(result))
			}
		}
		return nil, errString("host call " + method + " failed")
	}
	return unwrapHost(raw)
}

type errString string

func (e errString) Error() string { return string(e) }

func okEnvelope(value any) ([]byte, error) {
	raw, err := marshalCompact(value)
	if err != nil {
		return nil, err
	}
	return json.Marshal(envelope{OK: true, Result: raw})
}

func errorEnvelope(code, message string, httpStatus ...int) []byte {
	status := 0
	if len(httpStatus) > 0 {
		status = httpStatus[0]
	}
	raw, _ := json.Marshal(envelope{
		OK:    false,
		Error: &envelopeError{Code: code, Message: message, HTTPStatus: status},
	})
	return raw
}

func writeResponse(response *C.cliproxy_buffer, raw []byte) {
	if response == nil || len(raw) == 0 {
		return
	}
	ptr := C.CBytes(raw)
	if ptr == nil {
		return
	}
	response.ptr = ptr
	response.len = C.size_t(len(raw))
}
