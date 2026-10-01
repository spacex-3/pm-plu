package main

import (
	"bytes"
	"time"
)

// Prism has no token stream: the finished answer arrives from the poll loop.
// Streaming therefore means synthesising Responses-API SSE frames around the
// complete result, exactly like freeastra.py does for its HTTP clients. The one
// real improvement over a plain wait is emitting response.created immediately,
// so clients show "thinking" instead of hanging on a silent connection.

func encodeSSE(name string, payload any) []byte {
	body, err := marshalCompact(payload)
	if err != nil {
		return nil
	}
	var buf bytes.Buffer
	if name != "" {
		buf.WriteString("event: ")
		buf.WriteString(name)
		buf.WriteByte('\n')
	}
	buf.WriteString("data: ")
	buf.Write(body)
	buf.WriteString("\n\n")
	return buf.Bytes()
}

func emitPluginStream(streamID string, payload []byte) error {
	_, err := doHost(hostStreamEmit, map[string]any{"stream_id": streamID, "payload": payload})
	return err
}

func closePluginStream(streamID, message string) {
	_, _ = doHost(hostStreamClose, map[string]any{"stream_id": streamID, "error": message})
}

// streamTextDeltaSize matches freeastra.py's 600-char emission chunks.
const streamTextDeltaSize = 600

// synthesizeStreamFrames builds the full SSE frame sequence for one completed
// turn. created/in_progress frames are emitted separately (before the Prism
// call) by the caller.
func synthesizeStreamFrames(base map[string]any, item map[string]any, text string, isToolCall bool) [][]byte {
	var out [][]byte
	out = append(out, encodeSSE("response.output_item.added", map[string]any{
		"type": "response.output_item.added", "output_index": 0,
		"item": withStatus(item, "in_progress"),
	}))
	if isToolCall {
		valueKey, deltaEvent, doneEvent := "arguments", "response.function_call_arguments.delta", "response.function_call_arguments.done"
		value := item[valueKey]
		out = append(out, encodeSSE(deltaEvent, map[string]any{
			"type": deltaEvent, "output_index": 0, "item_id": item["id"], "delta": value,
		}))
		out = append(out, encodeSSE(doneEvent, map[string]any{
			"type": doneEvent, "output_index": 0, "item_id": item["id"], valueKey: value,
		}))
	} else if text != "" {
		itemID, _ := item["id"].(string)
		for i := 0; i < len(text); i += streamTextDeltaSize {
			end := i + streamTextDeltaSize
			if end > len(text) {
				end = len(text)
			}
			out = append(out, encodeSSE("response.output_text.delta", map[string]any{
				"type": "response.output_text.delta", "item_id": itemID,
				"output_index": 0, "content_index": 0, "delta": text[i:end],
			}))
		}
		out = append(out, encodeSSE("response.output_text.done", map[string]any{
			"type": "response.output_text.done", "item_id": itemID,
			"output_index": 0, "content_index": 0, "text": text,
		}))
	}
	out = append(out, encodeSSE("response.output_item.done", map[string]any{
		"type": "response.output_item.done", "output_index": 0,
		"item": withStatus(item, "completed"),
	}))
	out = append(out, encodeSSE("response.completed", map[string]any{
		"type": "response.completed", "response": base,
	}))
	out = append(out, []byte("data: [DONE]\n\n"))
	return out
}

func withStatus(item map[string]any, status string) map[string]any {
	cloned := map[string]any{}
	for key, value := range item {
		cloned[key] = value
	}
	cloned["status"] = status
	return cloned
}

func nowUnix() int64 {
	return time.Now().Unix()
}
