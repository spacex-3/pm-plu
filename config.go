package main

import (
	"strings"
	"sync/atomic"

	"gopkg.in/yaml.v3"
)

const (
	pluginName = "prism-plugin"

	defaultBaseURL       = "https://prism.openai.com"
	defaultUserAgent     = "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/152.0.0.0 Safari/537.36"
	defaultEffort        = "medium"
	defaultTimeout       = 240
	defaultConcurrency   = 1
	defaultMaxTranscript = 24000
)

// pluginVersion is overwritten by release builds with -X main.pluginVersion.
var pluginVersion = "0.1.0"

type pluginConfig struct {
	Enabled            *bool             `yaml:"enabled"`
	BaseURL            string            `yaml:"base_url"`
	UserAgent          string            `yaml:"user_agent"`
	Effort             string            `yaml:"effort"`
	TimeoutSeconds     int               `yaml:"timeout_seconds"`
	Concurrency        int               `yaml:"concurrency"`
	KeepaliveSeconds   int               `yaml:"keepalive_seconds"`
	MaxTranscriptChars int               `yaml:"max_transcript_chars"`
	ModelAliases       map[string]string `yaml:"model_aliases"`
}

func (c pluginConfig) enabled() bool {
	return c.Enabled == nil || *c.Enabled
}

func (c pluginConfig) baseURL() string {
	if strings.TrimSpace(c.BaseURL) == "" {
		return defaultBaseURL
	}
	return strings.TrimSpace(c.BaseURL)
}

func (c pluginConfig) userAgent() string {
	if strings.TrimSpace(c.UserAgent) == "" {
		return defaultUserAgent
	}
	return strings.TrimSpace(c.UserAgent)
}

func (c pluginConfig) effort() string {
	if effort := normalizeEffort(c.Effort); effort != "" {
		return effort
	}
	return defaultEffort
}

func (c pluginConfig) timeoutSeconds() int {
	if c.TimeoutSeconds > 0 {
		return c.TimeoutSeconds
	}
	return defaultTimeout
}

func (c pluginConfig) concurrency() int {
	if c.Concurrency >= 1 {
		return c.Concurrency
	}
	return defaultConcurrency
}

func (c pluginConfig) maxTranscriptChars() int {
	if c.MaxTranscriptChars > 0 {
		return c.MaxTranscriptChars
	}
	return defaultMaxTranscript
}

func (c pluginConfig) modelAlias(name string) (string, bool) {
	name = strings.TrimSpace(name)
	if name == "" || len(c.ModelAliases) == 0 {
		return "", false
	}
	target := strings.TrimSpace(c.ModelAliases[name])
	if target == "" || target == name {
		return "", false
	}
	return target, true
}

var currentConfig atomic.Value

func init() {
	currentConfig.Store(pluginConfig{})
}

func loadedConfig() pluginConfig {
	value := currentConfig.Load()
	if value == nil {
		return pluginConfig{}
	}
	cfg, _ := value.(pluginConfig)
	return cfg
}

func configure(raw []byte) error {
	var req struct {
		ConfigYAML []byte `json:"config_yaml"`
	}
	if len(raw) > 0 {
		if err := decodeJSONUseNumber(raw, &req); err != nil {
			return err
		}
	}
	cfg := pluginConfig{}
	if len(req.ConfigYAML) > 0 {
		if err := yaml.Unmarshal(req.ConfigYAML, &cfg); err != nil {
			return err
		}
	}
	currentConfig.Store(cfg)
	startKeepalive()
	return nil
}
