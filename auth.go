package main

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strings"
	"time"
)

// Prism auth is file-based: the user re-imports a "Copy as cURL" capture when
// the browser cookie expires (~12h). There is no refresh token to exchange, so
// auth.refresh only re-validates the stored record.

type authRecord struct {
	Provider         string            `json:"Provider"`
	ID               string            `json:"ID"`
	FileName         string            `json:"FileName"`
	Label            string            `json:"Label"`
	StorageJSON      []byte            `json:"StorageJSON"`
	Metadata         map[string]any    `json:"Metadata,omitempty"`
	Attributes       map[string]string `json:"Attributes,omitempty"`
	NextRefreshAfter time.Time         `json:"NextRefreshAfter"`
}

var acceptedAuthTypes = map[string]bool{
	"prism":      true,
	"free-astra": true,
	"freeastra":  true,
	"prism2api":  true,
}

type prismCredential struct {
	Type             string
	Cookie           string
	SandboxURL       string
	SandboxToken     string
	ProjectID        string
	UserID           string
	NextRefreshAfter time.Time
	raw              map[string]any
}

func parseAuthFile(raw []byte, fileName string) (authRecord, bool, error) {
	cred, ok, err := parseCredential(raw)
	if err != nil || !ok {
		return authRecord{}, ok, err
	}
	record, err := cred.record(fileName)
	return record, true, err
}

func parseCredential(raw []byte) (prismCredential, bool, error) {
	if len(strings.TrimSpace(string(raw))) == 0 {
		return prismCredential{}, false, nil
	}
	var payload map[string]any
	if err := decodeJSONUseNumber(raw, &payload); err != nil {
		return prismCredential{}, false, nil
	}
	authType := strings.ToLower(strings.TrimSpace(stringField(payload, "type")))
	if !acceptedAuthTypes[authType] {
		return prismCredential{}, false, nil
	}
	cred := prismCredential{
		Type:         providerID,
		Cookie:       firstString(payload, "cookie", "cookies"),
		SandboxURL:   firstString(payload, "sandbox_url", "sandboxUrl"),
		SandboxToken: firstString(payload, "sandbox_token", "sandboxToken"),
		ProjectID:    firstString(payload, "project_id", "projectId"),
		UserID:       firstString(payload, "user_id", "userId"),
		raw:          payload,
	}
	if cred.Cookie == "" {
		return prismCredential{}, true, fmt.Errorf("prism auth requires cookie")
	}
	if cred.SandboxToken == "" {
		return prismCredential{}, true, fmt.Errorf("prism auth requires sandbox_token (capture the POST /api/llm/response_with_tools_start request)")
	}
	cred.NextRefreshAfter = time.Now().Add(6 * time.Hour).UTC()
	return cred, true, nil
}

func (c prismCredential) record(fileName string) (authRecord, error) {
	storage, err := c.storageJSON()
	if err != nil {
		return authRecord{}, err
	}
	sum := sha256.Sum256([]byte(c.Cookie))
	id := "prism-" + hex.EncodeToString(sum[:])[:8]
	label := "Prism session"
	if c.ProjectID != "" {
		label = "Prism project " + c.ProjectID
	}
	if strings.TrimSpace(fileName) == "" {
		fileName = id + ".json"
	}
	return authRecord{
		Provider:         providerID,
		ID:               id,
		FileName:         fileName,
		Label:            label,
		StorageJSON:      storage,
		Metadata:         map[string]any{"type": providerID},
		Attributes:       map[string]string{},
		NextRefreshAfter: c.NextRefreshAfter,
	}, nil
}

func (c prismCredential) storageJSON() ([]byte, error) {
	payload := map[string]any{
		"type":          providerID,
		"cookie":        c.Cookie,
		"sandbox_url":   c.SandboxURL,
		"sandbox_token": c.SandboxToken,
	}
	if c.ProjectID != "" {
		payload["project_id"] = c.ProjectID
	}
	if c.UserID != "" {
		payload["user_id"] = c.UserID
	}
	return marshalCompact(payload)
}

func refreshCredential(raw []byte, fileName string) (authRecord, error) {
	cred, ok, err := parseCredential(raw)
	if err != nil {
		return authRecord{}, err
	}
	if !ok {
		return authRecord{}, fmt.Errorf("prism auth refresh did not recognize the credential")
	}
	// No refresh token exists: the browser cookie must be re-captured by hand.
	// Re-recording keeps CPA happy and preserves the same fields.
	return cred.record(fileName)
}
