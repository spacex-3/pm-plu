package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
	"strings"
)

func decodeJSONUseNumber(raw []byte, dest any) error {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	if err := dec.Decode(dest); err != nil {
		return err
	}
	return nil
}

func decodeObject(raw []byte) (map[string]any, error) {
	var value any
	if err := decodeJSONUseNumber(raw, &value); err != nil {
		return nil, err
	}
	object, ok := value.(map[string]any)
	if !ok {
		return nil, fmt.Errorf("json value is not an object")
	}
	return object, nil
}

func marshalCompact(value any) ([]byte, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(value); err != nil {
		return nil, err
	}
	return bytes.TrimRight(buf.Bytes(), "\n"), nil
}

func mustCompact(value any) string {
	raw, err := marshalCompact(value)
	if err != nil {
		return ""
	}
	return string(raw)
}

func canonicalJSON(value any) string {
	raw, err := marshalCompact(canonicalize(value))
	if err != nil {
		return ""
	}
	return string(raw)
}

func canonicalize(value any) any {
	switch typed := value.(type) {
	case map[string]any:
		keys := make([]string, 0, len(typed))
		for key := range typed {
			keys = append(keys, key)
		}
		sort.Strings(keys)
		out := make([]any, 0, len(keys)*2)
		for _, key := range keys {
			out = append(out, key, canonicalize(typed[key]))
		}
		return sortedObject(out)
	case []any:
		out := make([]any, len(typed))
		for i := range typed {
			out[i] = canonicalize(typed[i])
		}
		return out
	default:
		return value
	}
}

// sortedObject keeps canonical key order when encoded.
type sortedObject []any

func (o sortedObject) MarshalJSON() ([]byte, error) {
	var buf bytes.Buffer
	buf.WriteByte('{')
	for i := 0; i+1 < len(o); i += 2 {
		if i > 0 {
			buf.WriteByte(',')
		}
		key, _ := marshalCompact(o[i])
		val, err := marshalCompact(o[i+1])
		if err != nil {
			return nil, err
		}
		buf.Write(key)
		buf.WriteByte(':')
		buf.Write(val)
	}
	buf.WriteByte('}')
	return buf.Bytes(), nil
}

func asObject(value any) (map[string]any, bool) {
	object, ok := value.(map[string]any)
	return object, ok
}

func asArray(value any) ([]any, bool) {
	array, ok := value.([]any)
	return array, ok
}

func stringField(object map[string]any, key string) string {
	text, _ := object[key].(string)
	return text
}

func firstString(object map[string]any, keys ...string) string {
	for _, key := range keys {
		if text, ok := object[key].(string); ok && strings.TrimSpace(text) != "" {
			return strings.TrimSpace(text)
		}
	}
	return ""
}

func cloneValue(value any) any {
	raw, err := marshalCompact(value)
	if err != nil {
		return value
	}
	var out any
	if err := decodeJSONUseNumber(raw, &out); err != nil {
		return value
	}
	return out
}

func parseJSONValue(text string) (any, error) {
	var value any
	if err := decodeJSONUseNumber([]byte(text), &value); err != nil {
		return nil, err
	}
	return value, nil
}

func jsonString(value any) string {
	switch typed := value.(type) {
	case string:
		return typed
	case nil:
		return ""
	default:
		return mustCompact(typed)
	}
}

func truncate(text string, limit int) string {
	if limit <= 0 || len(text) <= limit {
		return text
	}
	return text[:limit]
}

func truncateRunes(text string, limit int) string {
	if limit <= 0 {
		return ""
	}
	runes := []rune(text)
	if len(runes) <= limit {
		return text
	}
	return string(runes[:limit])
}

func atoiDefault(text string, fallback int) int {
	parsed, err := strconv.Atoi(strings.TrimSpace(text))
	if err != nil {
		return fallback
	}
	return parsed
}
