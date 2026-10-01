package main

import (
	"sort"
	"strings"
	"sync"
)

const (
	providerID         = "prism"
	defaultPublicModel = "gpt-6-astra"
)

type prismModel struct {
	PublicID   string
	UpstreamID string
	Display    string
	Context    int64
}

// Prism's allowlist changes without warning - gpt-6-astra was dropped
// mid-session on 2026-09-17. The fallback table below mirrors freeastra.py.
var prismModels = []prismModel{
	{PublicID: "gpt-6-astra", UpstreamID: "gpt-6-astra", Display: "GPT 6 Astra", Context: 272000},
	{PublicID: "gpt-5.6-sol", UpstreamID: "gpt-5.6-sol", Display: "5.6 Sol", Context: 272000},
	{PublicID: "gpt-5.6-terra", UpstreamID: "gpt-5.6-terra", Display: "5.6 Terra", Context: 272000},
	{PublicID: "prism-astra", UpstreamID: "gpt-6-astra", Display: "GPT 6 Astra (prism alias)", Context: 272000},
	{PublicID: "prism-sol", UpstreamID: "gpt-5.6-sol", Display: "5.6 Sol (prism alias)", Context: 272000},
	{PublicID: "prism-terra", UpstreamID: "gpt-5.6-terra", Display: "5.6 Terra (prism alias)", Context: 272000},
}

var reasoningEfforts = []string{"low", "medium", "high"}

var reasoningAliases = map[string]string{
	"x-high":     "high",
	"xhigh":      "high",
	"extra-high": "high",
	"extra_high": "high",
	"max":        "high",
	"ultra":      "high",
	"minimal":    "low",
	"none":       "low",
}

var prismFallbacks = map[string][]string{
	"gpt-6-astra":   {"gpt-5.6-sol", "gpt-5.6-terra"},
	"gpt-5.6-sol":   {"gpt-5.6-terra", "gpt-6-astra"},
	"gpt-5.6-terra": {"gpt-5.6-sol", "gpt-6-astra"},
}

var (
	substituteMu  sync.Mutex
	substituteMap = map[string]string{}
)

func knownModel(publicID string) (prismModel, bool) {
	for _, model := range prismModels {
		if model.PublicID == publicID || model.UpstreamID == publicID {
			return model, true
		}
	}
	return prismModel{}, false
}

// splitModelSuffix strips a "(high)" or ":high" reasoning suffix, but only when
// the suffix is a known effort so custom ids survive intact.
func splitModelSuffix(model string) (string, string) {
	model = strings.TrimSpace(model)
	model = strings.TrimPrefix(model, providerID+"/")
	if open := strings.LastIndex(model, "("); open >= 0 && strings.HasSuffix(model, ")") && open < len(model)-1 {
		if effort := normalizeEffort(strings.TrimSpace(model[open+1 : len(model)-1])); effort != "" {
			return strings.TrimSpace(model[:open]), effort
		}
		return model, ""
	}
	if idx := strings.LastIndex(model, ":"); idx >= 0 && idx < len(model)-1 {
		if effort := normalizeEffort(strings.TrimSpace(model[idx+1:])); effort != "" {
			return strings.TrimSpace(model[:idx]), effort
		}
	}
	return model, ""
}

func publicModelID(model string) string {
	name, _ := splitModelSuffix(model)
	if name == "" {
		return defaultPublicModel
	}
	return name
}

func upstreamModelID(model string) string {
	name, _ := splitModelSuffix(model)
	if name == "" {
		return defaultPublicModel
	}
	name = applyModelAlias(name)
	if known, ok := knownModel(name); ok {
		return known.UpstreamID
	}
	return name
}

func normalizeEffort(value any) string {
	text, ok := value.(string)
	if !ok {
		return ""
	}
	normalized := strings.ToLower(strings.TrimSpace(text))
	if alias, ok := reasoningAliases[normalized]; ok {
		normalized = alias
	}
	for _, effort := range reasoningEfforts {
		if normalized == effort {
			return effort
		}
	}
	return ""
}

func effortFromModelSuffix(model string) string {
	_, suffix := splitModelSuffix(model)
	return suffix
}

func applyModelAlias(name string) string {
	seen := map[string]bool{}
	for hops := 0; hops < 4; hops++ {
		if seen[name] {
			return name
		}
		seen[name] = true
		target, ok := loadedConfig().modelAlias(name)
		if !ok {
			return name
		}
		name = target
	}
	return name
}

type aliasModel struct {
	ID    string
	Model prismModel
}

func configuredAliasIDs() []aliasModel {
	cfg := loadedConfig()
	if len(cfg.ModelAliases) == 0 {
		return nil
	}
	keys := make([]string, 0, len(cfg.ModelAliases))
	for key := range cfg.ModelAliases {
		key = strings.TrimSpace(key)
		if key != "" {
			keys = append(keys, key)
		}
	}
	sort.Strings(keys)
	out := make([]aliasModel, 0, len(keys))
	for _, key := range keys {
		target, ok := cfg.modelAlias(key)
		if !ok {
			continue
		}
		model, ok := modelForAliasTarget(target)
		if !ok {
			model = prismModel{PublicID: target, UpstreamID: target, Display: key, Context: 272000}
		}
		out = append(out, aliasModel{ID: key, Model: model})
	}
	return out
}

func modelForAliasTarget(target string) (prismModel, bool) {
	return knownModel(strings.TrimSpace(target))
}

func fallbacksFor(model string) []string {
	return prismFallbacks[model]
}

// substituteFor sticks with a fallback model for the process lifetime once the
// primary one was rejected by Prism, mirroring freeastra's _substitute map.
func substituteFor(wanted string) string {
	substituteMu.Lock()
	defer substituteMu.Unlock()
	if model, ok := substituteMap[wanted]; ok {
		return model
	}
	return wanted
}

func rememberSubstitute(wanted, accepted string) {
	substituteMu.Lock()
	defer substituteMu.Unlock()
	substituteMap[wanted] = accepted
}
