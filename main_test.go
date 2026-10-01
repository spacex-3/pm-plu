package main

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func TestParseToolCallTolerant(t *testing.T) {
	cases := []struct {
		name string
		text string
		want string
		ok   bool
	}{
		{"fenced", "```json\n{\"tool_call\":{\"name\":\"shell\",\"arguments\":{\"command\":[\"ls\"]}}}\n```", "shell", true},
		{"prose-prefix", "Sure!\n{\"tool_call\":{\"name\":\"shell\",\"arguments\":{}}}", "shell", true},
		{"missing-brace", `{"tool_call":{"name":"exec_command","arguments":{"cmd":"ls","max_output_tokens":1000}}`, "exec_command", true},
		{"string-args", `{"tool_call":{"name":"x","arguments":"{\"a\":1}"}}`, "x", true},
		{"prose-not-a-call", "just prose", "", false},
		{"no-tool-call", `{"not_a_tool":1}`, "", false},
		{"done-not-tool", `{"done":"ok"}`, "", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := parseToolCall(tc.text)
			if tc.ok && got == nil {
				t.Fatalf("expected a tool call, got none")
			}
			if !tc.ok && got != nil {
				t.Fatalf("expected no tool call, got %+v", got)
			}
			if tc.ok && got.Name != tc.want {
				t.Fatalf("name = %q, want %q", got.Name, tc.want)
			}
		})
	}
}

func TestParseToolCallRepairedArguments(t *testing.T) {
	broken := `{"tool_call":{"name":"exec_command","arguments":{"cmd":"ls","max_output_tokens":1000}}`
	got := parseToolCall(broken)
	if got == nil {
		t.Fatal("expected repair")
	}
	var args map[string]any
	if err := json.Unmarshal([]byte(got.Arguments), &args); err != nil {
		t.Fatalf("arguments not json: %v", err)
	}
	if args["cmd"] != "ls" {
		t.Fatalf("cmd = %v", args["cmd"])
	}
}

func TestParseToolCallCustomInput(t *testing.T) {
	got := parseToolCall(`{"tool_call":{"name":"apply_patch","input":"*** Begin Patch"}}`)
	if got == nil || !got.Custom || got.Input != "*** Begin Patch" {
		t.Fatalf("custom parse failed: %+v", got)
	}
}

func TestParseDone(t *testing.T) {
	if done, ok := parseDone(`{"done":"ok"} trailing prose`); !ok || done != "ok" {
		t.Fatalf("parseDone = %q %v", done, ok)
	}
	if _, ok := parseDone(`{"tool_call":{"name":"x"}}`); ok {
		t.Fatal("tool call is not a done marker")
	}
}

func TestFlattenResponses(t *testing.T) {
	source := map[string]any{
		"input": []any{
			map[string]any{"type": "message", "role": "developer", "content": []any{map[string]any{"type": "input_text", "text": "<environment_context>cwd=/tmp</environment_context>"}}},
			map[string]any{"type": "message", "role": "user", "content": []any{map[string]any{"type": "input_text", "text": "<recommended_plugins>noise</recommended_plugins>\n<cwd>/srv/app</cwd>"}}},
			map[string]any{"type": "function_call", "name": "exec_command", "arguments": "{}"},
			map[string]any{"type": "function_call_output", "output": "a.txt"},
			map[string]any{"type": "message", "role": "user", "content": []any{map[string]any{"type": "input_text", "text": "make it work"}}},
		},
		"tools": []any{
			map[string]any{"type": "function", "name": "exec_command", "parameters": map[string]any{"cmd": "string"}},
			map[string]any{"type": "namespace", "name": "mcp__x", "tools": []any{}},
			map[string]any{"type": "web_search"},
		},
	}
	conv := flattenResponses(source)
	if !strings.HasPrefix(conv.System, "<role>next-action emitter</role>") {
		t.Fatalf("system = %.80s", conv.System)
	}
	if !strings.Contains(conv.System, "- exec_command\n") {
		t.Fatal("tool catalog missing exec_command")
	}
	if strings.Contains(conv.System, "mcp__x") {
		t.Fatal("mcp namespace should be skipped")
	}
	if !strings.Contains(conv.System, "cwd=/tmp") {
		t.Fatal("environment_context lost")
	}
	if !strings.Contains(conv.System, "<cwd>/srv/app</cwd>") {
		t.Fatal("cwd inside dropped scaffolding message lost")
	}
	if strings.Contains(conv.User, "recommended_plugins") {
		t.Fatal("scaffolding leaked into the user message")
	}
	if !strings.HasPrefix(conv.User, "TASK:\nmake it work") {
		t.Fatalf("task not spotlighted: %.60s", conv.User)
	}
	if !strings.Contains(conv.User, "[executor ran]") || !strings.Contains(conv.User, "a.txt") {
		t.Fatal("results after the task not kept")
	}
	if !strings.HasSuffix(strings.TrimRight(conv.User, "\n"), strings.Repeat("=", 80)) {
		t.Fatal("tool reminder missing")
	}
}

func TestFlattenResponsesAdditionalTools(t *testing.T) {
	at := map[string]any{
		"input": []any{
			map[string]any{"type": "additional_tools", "role": "developer", "tools": []any{
				map[string]any{"type": "namespace", "name": "functions", "tools": []any{
					map[string]any{"type": "custom", "name": "apply_patch", "description": "freeform"},
					map[string]any{"type": "function", "name": "wait", "parameters": map[string]any{}},
				}},
				map[string]any{"type": "namespace", "name": "mcp__big", "tools": []any{
					map[string]any{"type": "function", "name": "noisy", "parameters": map[string]any{}},
				}},
			}},
			map[string]any{"type": "message", "role": "user", "content": []any{map[string]any{"type": "input_text", "text": "do it"}}},
			map[string]any{"type": "custom_tool_call", "name": "exec", "input": "await tools.exec_command({cmd:'ls'})"},
			map[string]any{"type": "custom_tool_call_output", "output": "a.txt"},
		},
	}
	conv := flattenResponses(at)
	if !strings.HasPrefix(conv.System, "<role>next-action emitter</role>") {
		t.Fatalf("system = %.80s", conv.System)
	}
	if !strings.Contains(conv.System, "- wait\n") {
		t.Fatal("additional function tool missing")
	}
	if strings.Contains(conv.System, "noisy") {
		t.Fatal("mcp tool should be skipped")
	}
	if !strings.HasPrefix(conv.User, "TASK:\ndo it") || !strings.Contains(conv.User, "[executor ran]") || !strings.Contains(conv.User, "a.txt") {
		t.Fatalf("transcript wrong: %.120s", conv.User)
	}
}

func TestFlattenChatPlain(t *testing.T) {
	conv := flattenChat(map[string]any{"messages": []any{
		map[string]any{"role": "system", "content": "be terse"},
		map[string]any{"role": "user", "content": "hi"},
		map[string]any{"role": "assistant", "content": "hello"},
		map[string]any{"role": "user", "content": "bye"},
	}})
	if conv.System != "be terse" {
		t.Fatalf("system = %q", conv.System)
	}
	if strings.Count(conv.User, "[user]") != 2 || !strings.HasSuffix(strings.TrimRight(conv.User, "\n"), "bye") {
		t.Fatalf("user = %.80s", conv.User)
	}
	if !strings.Contains(conv.User, "Respond to the FINAL message") {
		t.Fatal("multi-turn hint missing")
	}
}

func TestClampTranscript(t *testing.T) {
	var big []string
	for i := 0; i < 100; i++ {
		big = append(big, strings.Repeat("x", 500))
	}
	clamped := clampTranscript(big, 2000)
	total := 0
	for _, c := range clamped {
		total += len(c)
	}
	if total > 3000 {
		t.Fatalf("clamp kept %d bytes", total)
	}
	if !strings.Contains(clamped[0], "earlier steps omitted") || clamped[len(clamped)-1] != big[len(big)-1] {
		t.Fatal("clamp must keep newest, note the cut")
	}
	if clampTranscript(nil, 100) != nil {
		t.Fatal("empty stays empty")
	}
	only := clampTranscript([]string{strings.Repeat("y", 5000)}, 1000)
	if !strings.HasPrefix(only[0], "...(truncated)...") || len(only[0]) > 1200 {
		t.Fatalf("oversize entry: %d", len(only[0]))
	}
}

func TestSplitModelSuffix(t *testing.T) {
	cases := []struct{ in, name, effort string }{
		{"gpt-6-astra", "gpt-6-astra", ""},
		{"gpt-6-astra(high)", "gpt-6-astra", "high"},
		{"gpt-6-astra:high", "gpt-6-astra", "high"},
		{"prism/gpt-5.6-sol:low", "gpt-5.6-sol", "low"},
		{"gpt-6-astra(9)", "gpt-6-astra(9)", ""},
	}
	for _, tc := range cases {
		name, effort := splitModelSuffix(tc.in)
		if name != tc.name || effort != tc.effort {
			t.Fatalf("splitModelSuffix(%q) = %q,%q want %q,%q", tc.in, name, effort, tc.name, tc.effort)
		}
	}
}

func TestUpstreamModelAlias(t *testing.T) {
	if got := upstreamModelID("prism-sol"); got != "gpt-5.6-sol" {
		t.Fatalf("prism-sol -> %q", got)
	}
	if got := upstreamModelID("gpt-6-astra(high)"); got != "gpt-6-astra" {
		t.Fatalf("suffix not stripped: %q", got)
	}
}

func TestClassifyTerminal(t *testing.T) {
	ok := classifyTerminal(map[string]any{"response": map[string]any{
		"status":  "success",
		"payload": map[string]any{"output": []any{map[string]any{"type": "message", "content": []any{map[string]any{"type": "output_text", "text": "hi"}}}}},
	}}, "")
	if ok.kind != passOK || ok.text != "hi" {
		t.Fatalf("success misclassified: %+v", ok)
	}
	dead := classifyTerminal(map[string]any{"response": map[string]any{
		"status":  "error",
		"payload": map[string]any{"reason": "sandbox_reconnecting"},
	}}, "")
	if dead.kind != passDead {
		t.Fatalf("sandbox_reconnecting should be passDead, got %v", dead.kind)
	}
	unsup := classifyTerminal(map[string]any{"response": map[string]any{
		"status":  "error",
		"payload": map[string]any{"reason": "x", "codexRequestDebug": map[string]any{"error": map[string]any{"bodyText": "Unsupported assistant model"}}},
	}}, "")
	if unsup.kind != passUnsupported {
		t.Fatalf("unsupported should be passUnsupported, got %v", unsup.kind)
	}
}

func TestParseCredential(t *testing.T) {
	valid := []byte(`{"type":"prism","cookie":"a=b","sandbox_url":"https://sb.example/","sandbox_token":"tok","project_id":"p1","user_id":"u1"}`)
	cred, ok, err := parseCredential(valid)
	if err != nil || !ok {
		t.Fatalf("valid rejected: %v", err)
	}
	if cred.Cookie != "a=b" || cred.ProjectID != "p1" {
		t.Fatalf("fields lost: %+v", cred)
	}
	if _, ok, _ := parseCredential([]byte(`{"type":"openai"}`)); ok {
		t.Fatal("foreign auth types must not be handled")
	}
	if _, _, err := parseCredential([]byte(`{"type":"prism","cookie":"a=b"}`)); err == nil {
		t.Fatal("missing sandbox_token must error")
	}
}

func TestCredentialRoundTrip(t *testing.T) {
	raw := []byte(`{"type":"prism","cookie":"a=b","sandbox_url":"https://sb.example/","sandbox_token":"tok"}`)
	cred, ok, err := parseCredential(raw)
	if err != nil || !ok {
		t.Fatalf("parse: %v", err)
	}
	record, err := cred.record("prism-test.json")
	if err != nil {
		t.Fatalf("record: %v", err)
	}
	if record.Provider != providerID || record.ID == "" || !strings.HasPrefix(record.FileName, "prism") && record.FileName != "prism-test.json" {
		t.Fatalf("record shape: %+v", record)
	}
	if record.NextRefreshAfter.IsZero() || record.NextRefreshAfter.Before(time.Now()) {
		t.Fatal("NextRefreshAfter must be in the future")
	}
	parsed, ok, err := parseCredential(record.StorageJSON)
	if err != nil || !ok || parsed.SandboxToken != "tok" {
		t.Fatalf("storage round trip failed: %v", err)
	}
}

func TestEstimateTokens(t *testing.T) {
	if n := estimateTokens("hello"); n < 1 || n > 3 {
		t.Fatalf("latin estimate = %d", n)
	}
	if n := estimateTokens("你好"); n != 2 {
		t.Fatalf("cjk estimate = %d, want 2", n)
	}
	if estimateTokens(nil) != 0 {
		t.Fatal("nil is zero")
	}
}

func TestLoadsJSONTrailing(t *testing.T) {
	object, ok := loadsJSON(`{"a":1} and some trailing prose`)
	if !ok || len(object) != 1 {
		t.Fatalf("raw_decode fallback failed: %v %v", object, ok)
	}
}

func TestEnvContextNoise(t *testing.T) {
	noise := "## Memory\n" + strings.Repeat("notes about another project ", 400) + "\ncwd stuff"
	if got := envContext([]string{noise}); strings.Contains(got, "another project") {
		t.Fatalf("long noise leaked: %.80s", got)
	}
	if got := envContext([]string{"<environment_context>cwd=/srv</environment_context>"}); got != "<environment_context>cwd=/srv</environment_context>" {
		t.Fatalf("env block = %q", got)
	}
	if got := envContext([]string{"junk <cwd>/a/b</cwd> junk"}); got != "<cwd>/a/b</cwd>" {
		t.Fatalf("cwd line = %q", got)
	}
	if got := envContext([]string{"<cwd>/a</cwd>", "<cwd>/a</cwd>"}); got != "<cwd>/a</cwd>" {
		t.Fatalf("dedupe failed: %q", got)
	}
}
