package main

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
)

// ---------------------------------------------------------------------------
// prompt assembly (port of freeastra.py assemble/flatten/flatten_responses)

const toolProtocol = `<role>next-action emitter</role>

You are ONE COMPONENT IN A PIPELINE. A separate executor process runs commands on
the user's machine. You do not. You never execute anything, you never touch a
filesystem, and you never report work as done from your own knowledge.

Your entire contract: read the task plus the transcript of what the executor has
already run, then emit ONE JSON action. The executor runs it and appends the result
to the transcript, then asks you again. Prose output breaks the pipeline and is
discarded, so never explain, never apologise, never use markdown fences.

Available actions:
%s

Shell guidance:
- ` + "`apply_patch`" + ` is NOT a shell command. Never pipe into it, never call it from a
  shell. Use it only if it appears as an action in the list above.
- Do NOT use heredocs (<<EOF). The executor's shell often cannot create the temp
  file they need.
- To write a file, redirect printf:
    printf '%%s\n' 'first line' 'second line' > path/to/file
- To read a file use ` + "`cat`" + `, to search use ` + "`rg`" + `. Verify with ` + "`cat`" + ` after writing.
- A command failing does not mean the workspace is read-only. Diagnose the actual
  error text before concluding anything about permissions.
- File paths in arguments are paths on the EXECUTOR's machine, shown above. Never
  guess one, and never pass a path you have not seen in the transcript.
- When a schema says two parameters are mutually exclusive, send only one.

Emit exactly one of:
  {"tool_call":{"name":"<action>","arguments":{...}}}
  {"done":"<one line summary>"}     <- ONLY when the TRANSCRIPT already proves the
                                       task is complete. An empty transcript never
                                       proves anything.`

const toolReminder = `

================================================================================
Emit ONE JSON action now. Actions: %s
{"tool_call":{"name":"...","arguments":{...}}}  or  {"done":"..."}
Nothing else. Start your reply with { .
================================================================================`

// Codex puts the working directory in <cwd>/<filesystem> tags, inside the same
// user message as the plugin list we drop as noise - so pull these out first.
var (
	envBlockRe = regexp.MustCompile(`(?s)<environment_context>.*?</environment_context>`)
	envTagRe   = regexp.MustCompile(`(?s)<cwd>.*?</cwd>|(?s)<filesystem>.*?</filesystem>`)
	cwdLineRe  = regexp.MustCompile(`(?im)^.{0,40}(cwd|current working directory)\s*[:=].*$`)
	noiseRe    = regexp.MustCompile(`^<(recommended_plugins|plugin_instructions|skills_instructions|apps_instructions)\b`)
	latinRe    = regexp.MustCompile(`[A-Za-z0-9_]+`)
	tokenRe    = regexp.MustCompile(`[A-Za-z0-9_]+|[\x{3400}-\x{4dbf}\x{4e00}-\x{9fff}\x{f900}-\x{faff}\x{3040}-\x{30ff}\x{ac00}-\x{d7af}]|[^\s]`)
	cjkRe      = regexp.MustCompile(`[\x{3400}-\x{4dbf}\x{4e00}-\x{9fff}\x{f900}-\x{faff}\x{3040}-\x{30ff}\x{ac00}-\x{d7af}]`)
)

type toolEntry struct {
	Name        string
	Namespace   string
	Type        string // "function" or "custom"
	Description string
	Parameters  any
}

type conversation struct {
	System   string
	User     string
	EnvParts []string
}

// clientToolEntries keeps the tools we can emulate, mirroring tool_specs plus
// additional_tool_specs in freeastra.py. MCP namespaces are skipped: their
// surface is too big to inline into the prompt.
func clientToolEntries(source map[string]any) []toolEntry {
	if strings.EqualFold(strings.TrimSpace(stringField(source, "tool_choice")), "none") {
		return nil
	}
	var out []toolEntry
	var appendTool func(namespace string, object map[string]any)
	appendTool = func(namespace string, object map[string]any) {
		name := strings.TrimSpace(stringField(object, "name"))
		kind := strings.ToLower(strings.TrimSpace(stringField(object, "type")))
		// Chat Completions nests the function schema under "function".
		if fn, ok := object["function"].(map[string]any); ok && name == "" {
			name = strings.TrimSpace(stringField(fn, "name"))
			object = map[string]any{"type": kind, "name": name, "description": fn["description"], "parameters": fn["parameters"]}
		}
		if kind == "web_search" || name == "" {
			return
		}
		if kind == "function" || kind == "custom" {
			description := stringField(object, "description")
			if kind == "function" {
				// description first paragraph, 300 chars
				if idx := strings.Index(description, "\n\n"); idx >= 0 {
					description = description[:idx]
				}
				description = truncateRunes(strings.TrimSpace(description), 300)
				params := object["parameters"]
				if params == nil {
					params = object["input_schema"]
				}
				if params == nil {
					params = object["inputSchema"]
				}
				out = append(out, toolEntry{Name: name, Namespace: namespace, Type: "function", Description: description, Parameters: params})
				return
			}
			// custom: freeform, no schema
			description = truncateRunes(strings.TrimSpace(strings.SplitN(description, "\n", 2)[0]), 160)
			out = append(out, toolEntry{Name: name, Namespace: namespace, Type: "custom", Description: description, Parameters: nil})
			return
		}
		if kind == "namespace" {
			nested := namespace
			if nested == "" {
				nested = name
			}
			if strings.HasPrefix(nested, "mcp__") {
				return // MCP surface is too big to inline into the prompt
			}
			if tools, ok := object["tools"].([]any); ok {
				for _, t := range tools {
					if inner, ok := t.(map[string]any); ok {
						appendTool(nested, inner)
					}
				}
			}
		}
	}
	for _, item := range asArrayOrEmpty(source["tools"]) {
		if object, ok := item.(map[string]any); ok {
			appendTool("", object)
		}
	}
	for _, item := range asArrayOrEmpty(source["input"]) {
		object, ok := item.(map[string]any)
		if !ok || !strings.EqualFold(stringField(object, "type"), "additional_tools") {
			continue
		}
		if tools, ok := object["tools"].([]any); ok {
			for _, t := range tools {
				if inner, ok := t.(map[string]any); ok {
					appendTool("", inner)
				}
			}
		}
	}
	return out
}

func asArrayOrEmpty(value any) []any {
	if array, ok := value.([]any); ok {
		return array
	}
	return nil
}

// envContext extracts just the shell environment blocks, nothing else.
func envContext(parts []string) string {
	var out []string
	for _, c := range parts {
		if c == "" {
			continue
		}
		for _, m := range envBlockRe.FindAllString(c, -1) {
			out = append(out, m)
		}
		for _, m := range envTagRe.FindAllString(c, -1) {
			out = append(out, m)
		}
	}
	if len(out) == 0 {
		for _, c := range parts {
			for i, m := range cwdLineRe.FindAllString(c, -1) {
				if i >= 3 {
					break
				}
				out = append(out, strings.TrimSpace(m))
			}
		}
	}
	seen := map[string]bool{}
	var uniq []string
	for _, o := range out {
		if !seen[o] {
			seen[o] = true
			uniq = append(uniq, o)
		}
	}
	joined := strings.Join(uniq, "\n")
	return truncateRunes(joined, 1500)
}

// clampTranscript keeps the most recent entries that fit, oldest first, noting
// what was cut.
func clampTranscript(entries []string, budget int) []string {
	if budget <= 0 {
		budget = defaultMaxTranscript
	}
	if len(entries) == 0 {
		return nil
	}
	var kept []string
	total := 0
	for i := len(entries) - 1; i >= 0; i-- {
		if total+len(entries[i]) > budget {
			break
		}
		kept = append([]string{entries[i]}, kept...)
		total += len(entries[i])
	}
	if len(kept) == 0 {
		last := entries[len(entries)-1]
		if len(last) > budget {
			last = last[len(last)-budget:]
		}
		return []string{"...(truncated)...\n" + last}
	}
	dropped := len(entries) - len(kept)
	if dropped > 0 {
		kept = append([]string{fmt.Sprintf("...(%d earlier steps omitted)...", dropped)}, kept...)
	}
	return kept
}

// assemble folds everything into one system + one user message. Upstream keeps
// only the last user message, so the transcript has to travel inside it.
func assemble(sysParts, convo, envParts []string, tools []toolEntry) conversation {
	if len(tools) == 0 {
		system := strings.Join(nonEmpty(sysParts), "\n\n")
		user := strings.Join(nonEmpty(convo), "\n\n")
		if user == "" {
			user = "(no user message)"
		}
		if strings.Count(user, "[user]") > 1 {
			user = "Conversation so far. Respond to the FINAL message.\n\n" + user
		}
		return conversation{System: system, User: user}
	}

	catalog := make([]string, 0, len(tools))
	for _, t := range tools {
		catalog = append(catalog, fmt.Sprintf("- %s\n    params: %s\n    %s",
			t.Name, truncateRunes(mustCompact(t.Parameters), 700), t.Description))
	}
	system := fmt.Sprintf(toolProtocol, strings.Join(catalog, "\n"))
	if ctx := envContext(envParts); ctx != "" {
		system += "\n\nThe executor runs here. Use these real paths - never invent a " +
			"sandbox path like /codex_workspace/...:\n" + ctx
	}

	lastIdx := -1
	for i, c := range convo {
		if strings.HasPrefix(c, "[user]\n") {
			lastIdx = i
		}
	}
	task := ""
	var rest []string
	if lastIdx >= 0 {
		task = strings.TrimSpace(strings.TrimPrefix(convo[lastIdx], "[user]\n"))
		rest = append(append([]string{}, convo[:lastIdx]...), convo[lastIdx+1:]...)
	} else {
		rest = append([]string{}, convo...)
	}
	rest = clampTranscript(rest, loadedConfig().maxTranscriptChars())
	transcript := strings.Join(nonEmpty(rest), "\n\n")
	if transcript == "" {
		transcript = "(empty - the executor has run nothing yet)"
	}
	names := make([]string, 0, len(tools))
	for _, t := range tools {
		names = append(names, t.Name)
	}
	user := fmt.Sprintf("TASK:\n%s\n\nTRANSCRIPT SO FAR:\n%s", orNone(task), transcript) + fmt.Sprintf(toolReminder, strings.Join(names, ", "))
	return conversation{System: system, User: user}
}

func nonEmpty(values []string) []string {
	var out []string
	for _, v := range values {
		if v != "" {
			out = append(out, v)
		}
	}
	return out
}

func orNone(value string) string {
	if strings.TrimSpace(value) == "" {
		return "(none)"
	}
	return value
}

// flattenResponses folds a Responses-API request into (system, user, envParts).
func flattenResponses(source map[string]any) conversation {
	var sysParts, convo, envParts []string
	if instructions := strings.TrimSpace(stringField(source, "instructions")); instructions != "" {
		sysParts = append(sysParts, instructions)
		envParts = append(envParts, instructions)
	}
	var items []any
	switch typed := source["input"].(type) {
	case string:
		items = []any{typed}
	case map[string]any:
		items = []any{typed}
	case []any:
		items = typed
	}
	for _, it := range items {
		if text, ok := it.(string); ok {
			convo = append(convo, "[user]\n"+text)
			continue
		}
		object, ok := it.(map[string]any)
		if !ok {
			continue
		}
		itemType := strings.ToLower(strings.TrimSpace(stringField(object, "type")))
		switch itemType {
		case "function_call":
			convo = append(convo, fmt.Sprintf("[executor ran]\n%s %s", stringField(object, "name"), jsonString(object["arguments"])))
		case "function_call_output":
			out := object["output"]
			if _, isStr := out.(string); !isStr {
				out = mustCompact(out)
			}
			text, _ := out.(string)
			convo = append(convo, "[result]\n"+truncateRunes(text, 8000))
		case "custom_tool_call":
			convo = append(convo, "[executor ran]\n"+truncateRunes(stringField(object, "input"), 4000))
		case "custom_tool_call_output":
			out := object["output"]
			if _, isStr := out.(string); !isStr {
				out = mustCompact(out)
			}
			text, _ := out.(string)
			convo = append(convo, "[result]\n"+truncateRunes(text, 8000))
		case "additional_tools", "item_reference", "reasoning":
			continue
		default:
			role := stringField(object, "role")
			if role == "" {
				role = "user"
			}
			c := contentText(object["content"])
			envParts = append(envParts, c)
			switch {
			case role == "developer" || role == "system":
				sysParts = append(sysParts, c)
			case role == "user" && noiseRe.MatchString(strings.TrimLeft(c, " \t\r\n")):
				// Codex scaffolding, not something the user typed
			case role == "assistant":
				convo = append(convo, "[assistant]\n"+c)
			default:
				convo = append(convo, "[user]\n"+c)
			}
		}
	}
	return assemble(sysParts, convo, envParts, clientToolEntries(source))
}

func flattenChat(source map[string]any) conversation {
	var sysParts, convo []string
	for _, item := range asArrayOrEmpty(source["messages"]) {
		message, ok := item.(map[string]any)
		if !ok {
			continue
		}
		role := stringField(message, "role")
		content := message["content"]
		if content == nil {
			content = ""
		}
		if _, isStr := content.(string); !isStr {
			content = contentText(content)
		}
		text, _ := content.(string)
		switch role {
		case "system", "developer":
			sysParts = append(sysParts, text)
		case "tool":
			convo = append(convo, "[tool result]\n"+truncateRunes(text, 8000))
		case "assistant":
			if calls, ok := message["tool_calls"].([]any); ok && len(calls) > 0 {
				convo = append(convo, "[executor ran]\n"+mustCompact(calls))
			}
			if text != "" {
				convo = append(convo, "[assistant]\n"+text)
			}
		default:
			convo = append(convo, "[user]\n"+text)
		}
	}
	return assemble(sysParts, convo, nil, clientToolEntries(source))
}

func contentText(value any) string {
	switch typed := value.(type) {
	case string:
		return typed
	case []any:
		var parts []string
		for _, part := range typed {
			if object, ok := part.(map[string]any); ok {
				if text, ok := object["text"].(string); ok {
					parts = append(parts, text)
				}
			}
		}
		return strings.Join(parts, "")
	default:
		return ""
	}
}

// ---------------------------------------------------------------------------
// tolerant reply parsing (port of _strip_fence/_loads/parse_tool_call/parse_done)

func stripFence(text string) string {
	s := strings.TrimSpace(text)
	if strings.HasPrefix(s, "```") {
		s = regexp.MustCompile(`^`+"```"+`[a-zA-Z]*\n?`).ReplaceAllString(s, "")
		s = strings.TrimSpace(strings.TrimSuffix(regexp.MustCompile("```$").ReplaceAllString(s, ""), ""))
	}
	if i := strings.Index(s, "{"); i > 0 && i < 40 {
		s = s[i:]
	}
	return s
}

// loadsJSON tolerates models dropping trailing braces on long nested arguments.
func loadsJSON(s string) (map[string]any, bool) {
	if !strings.HasPrefix(s, "{") {
		return nil, false
	}
	for extra := 0; extra <= 3; extra++ {
		var value any
		if err := decodeJSONUseNumber([]byte(s+strings.Repeat("}", extra)), &value); err == nil {
			if object, ok := value.(map[string]any); ok {
				return object, true
			}
			return nil, false
		}
	}
	// trailing prose after a valid object
	dec := json.NewDecoder(strings.NewReader(s))
	dec.UseNumber()
	var value any
	if err := dec.Decode(&value); err == nil {
		if object, ok := value.(map[string]any); ok {
			return object, true
		}
	}
	return nil, false
}

type parsedToolCall struct {
	ID        string
	Name      string
	Arguments string // compact JSON
	Input     string // custom tool payload
	Custom    bool
}

func parseToolCall(text string) *parsedToolCall {
	object, ok := loadsJSON(stripFence(text))
	if !ok {
		return nil
	}
	tc, ok := object["tool_call"].(map[string]any)
	if !ok {
		return nil
	}
	name := strings.TrimSpace(stringField(tc, "name"))
	if name == "" {
		return nil
	}
	args := tc["arguments"]
	if argText, isStr := args.(string); isStr {
		if repaired, ok := loadsJSON(argText); ok {
			return &parsedToolCall{
				ID:        "call_" + randomHex(8),
				Name:      name,
				Arguments: mustCompact(repaired),
			}
		}
		return &parsedToolCall{
			ID:        "call_" + randomHex(8),
			Name:      name,
			Arguments: mustCompact(argText),
		}
	}
	if args != nil {
		return &parsedToolCall{
			ID:        "call_" + randomHex(8),
			Name:      name,
			Arguments: mustCompact(args),
		}
	}
	// Custom (freeform) tools carry a raw input string instead of arguments.
	if input, isStr := tc["input"].(string); isStr {
		return &parsedToolCall{ID: "call_" + randomHex(8), Name: name, Input: input, Custom: true}
	}
	return &parsedToolCall{ID: "call_" + randomHex(8), Name: name, Arguments: "{}"}
}

func parseDone(text string) (string, bool) {
	object, ok := loadsJSON(stripFence(text))
	if !ok {
		return "", false
	}
	done, ok := object["done"].(string)
	return done, ok
}

func randomHex(n int) string {
	buf := make([]byte, n)
	if _, err := rand.Read(buf); err != nil {
		for i := range buf {
			buf[i] = byte(i)
		}
	}
	return hex.EncodeToString(buf)
}

// ---------------------------------------------------------------------------
// usage estimation (port of estimate_tokens/estimated_usage)

func estimateTokens(value any) int {
	text, ok := value.(string)
	if !ok {
		if value == nil {
			return 0
		}
		text = mustCompact(value)
	}
	count := 0
	for _, piece := range tokenRe.FindAllString(text, -1) {
		if latinRe.MatchString(piece) {
			count += (len(piece) + 3) / 4
			if count == 0 {
				count = 1
			}
		} else {
			count++
		}
	}
	return count
}

func estimatedUsage(system, user, output string) (int, int) {
	input := estimateTokens(system) + estimateTokens(user) + 4
	if input < 1 {
		input = 1
	}
	outputTokens := estimateTokens(output) + 1
	if outputTokens < 1 {
		outputTokens = 1
	}
	return input, outputTokens
}

var _ = cjkRe // reserved: finer-grained CJK weighting if Prism ever reports real usage
