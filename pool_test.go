package main

import (
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// withConcurrency installs a config and restores it afterwards.
func withConcurrency(t *testing.T, yamlBody string) {
	t.Helper()
	previous := currentConfig.Load()
	raw, err := json.Marshal(map[string]any{"config_yaml": []byte(yamlBody)})
	if err != nil {
		t.Fatalf("config marshal: %v", err)
	}
	if err := configure(raw); err != nil {
		t.Fatalf("configure: %v", err)
	}
	t.Cleanup(func() { currentConfig.Store(previous) })
}

// dropPools removes pool state created by a test.
func dropPools(t *testing.T, cookie string) {
	t.Helper()
	t.Cleanup(func() {
		poolsMu.Lock()
		delete(pools, cookie)
		poolsMu.Unlock()
	})
}

// stubPrismHost fakes host.http.do for /new and the start endpoint, counting
// mints and returning a scripted completed turn.
func stubPrismHost(t *testing.T, mints *atomic.Int64, turnText string) {
	t.Helper()
	previous := doHost
	doHost = func(method string, payload any) (json.RawMessage, error) {
		if method != hostHTTPDo {
			return json.RawMessage(`{}`), nil
		}
		request, _ := payload.(map[string]any)
		url, _ := request["url"].(string)
		if strings.HasSuffix(url, prismNewPath) {
			n := mints.Add(1)
			body := map[string]any{
				"url":   "https://sandbox-" + strconv.FormatInt(n, 10) + ".example/",
				"token": "tok-" + strconv.FormatInt(n, 10),
			}
			return hostBody(t, http.StatusOK, mustCompact(body)), nil
		}
		if strings.HasSuffix(url, prismStartPath) {
			started := map[string]any{
				"request_id": "req_1",
				"status":     "completed",
				"response": map[string]any{
					"status": "success",
					"payload": map[string]any{
						"output": []any{map[string]any{
							"type": "message", "role": "assistant",
							"content": []any{map[string]any{"type": "output_text", "text": turnText}},
						}},
					},
				},
			}
			return hostBody(t, http.StatusOK, mustCompact(started)), nil
		}
		return hostBody(t, http.StatusNotFound, `{"error":"unexpected host call"}`), nil
	}
	t.Cleanup(func() { doHost = previous })
}

func hostBody(t *testing.T, status int, body string) json.RawMessage {
	t.Helper()
	raw, err := json.Marshal(struct {
		StatusCode int    `json:"status_code"`
		Body       []byte `json:"body"`
	}{StatusCode: status, Body: []byte(body)})
	if err != nil {
		t.Fatalf("host body marshal: %v", err)
	}
	return raw
}

func TestPoolSeedThenMintThenReuse(t *testing.T) {
	withConcurrency(t, "concurrency: 3\n")
	var mints atomic.Int64
	stubPrismHost(t, &mints, "unused")
	cookie := "pool-cookie-reuse"
	dropPools(t, cookie)

	cred := prismCredential{Cookie: cookie, SandboxURL: "https://seed.example/", SandboxToken: "seed-tok"}
	pool := poolFor(cred)
	// 30s: comfortably above the 10s mint guard so the pool may grow.
	deadline := time.Now().Add(30 * time.Second)

	first, err := pool.acquire(deadline) // the imported seed sandbox
	if err != nil {
		t.Fatalf("first acquire: %v", err)
	}
	if first.Token != "seed-tok" {
		t.Fatalf("seed not handed out first: %+v", first)
	}
	second, err := pool.acquire(deadline) // pool grows by minting
	if err != nil {
		t.Fatalf("second acquire: %v", err)
	}
	if mints.Load() != 1 {
		t.Fatalf("mints = %d, want 1", mints.Load())
	}
	pool.release(first, false)
	pool.release(second, false)

	third, err := pool.acquire(deadline) // reused, no new mint
	if err != nil {
		t.Fatalf("third acquire: %v", err)
	}
	if mints.Load() != 1 {
		t.Fatalf("mints after reuse = %d, want 1", mints.Load())
	}
	pool.release(third, false)
}

func TestPoolCapBlocks(t *testing.T) {
	withConcurrency(t, "concurrency: 2\n")
	var mints atomic.Int64
	stubPrismHost(t, &mints, "unused")
	cookie := "pool-cookie-cap"
	dropPools(t, cookie)

	cred := prismCredential{Cookie: cookie, SandboxURL: "https://seed.example/", SandboxToken: "seed-tok"}
	pool := poolFor(cred)
	deadline := time.Now().Add(30 * time.Second)

	one, err := pool.acquire(deadline)
	if err != nil {
		t.Fatalf("acquire 1: %v", err)
	}
	two, err := pool.acquire(deadline)
	if err != nil {
		t.Fatalf("acquire 2: %v", err)
	}
	if _, err := pool.acquire(time.Now().Add(250 * time.Millisecond)); err == nil {
		t.Fatal("third acquire should hit the cap and time out")
	}
	pool.release(one, false)
	again, err := pool.acquire(deadline)
	if err != nil {
		t.Fatalf("acquire after release: %v", err)
	}
	pool.release(two, false)
	pool.release(again, false)
	if mints.Load() != 1 {
		t.Fatalf("mints = %d, want 1", mints.Load())
	}
}

func TestPoolDeadReleaseRegrows(t *testing.T) {
	withConcurrency(t, "concurrency: 2\n")
	var mints atomic.Int64
	stubPrismHost(t, &mints, "unused")
	cookie := "pool-cookie-dead"
	dropPools(t, cookie)

	cred := prismCredential{Cookie: cookie, SandboxURL: "https://seed.example/", SandboxToken: "seed-tok"}
	pool := poolFor(cred)

	entry, err := pool.acquire(time.Now().Add(30 * time.Second))
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}
	pool.release(entry, true) // sandbox died upstream

	next, err := pool.acquire(time.Now().Add(30 * time.Second))
	if err != nil {
		t.Fatalf("acquire after dead release: %v", err)
	}
	if mints.Load() != 1 {
		t.Fatalf("dead entry should force a fresh mint, mints = %d", mints.Load())
	}
	pool.release(next, false)
}

func TestRunPrismTurnEndToEnd(t *testing.T) {
	withConcurrency(t, "concurrency: 1\n")
	var mints atomic.Int64
	stubPrismHost(t, &mints, "pool answer")
	cookie := "pool-cookie-turn"
	dropPools(t, cookie)

	cred := prismCredential{Cookie: cookie, SandboxURL: "https://seed.example/", SandboxToken: "seed-tok"}
	text, err := runPrismTurn(cred, "", "hello", "gpt-5.6-sol", "medium", 30*time.Second)
	if err != nil {
		t.Fatalf("runPrismTurn: %v", err)
	}
	if text != "pool answer" {
		t.Fatalf("text = %q", text)
	}
	if mints.Load() != 0 {
		t.Fatalf("seed should serve the first turn, mints = %d", mints.Load())
	}
}

func TestRunPrismTurnWithoutSandbox(t *testing.T) {
	// No seed sandbox and minting disabled by an immediate deadline: the turn
	// must fail with a queue error, not deadlock.
	withConcurrency(t, "concurrency: 1\n")
	cookie := "pool-cookie-noseed"
	dropPools(t, cookie)

	cred := prismCredential{Cookie: cookie} // no sandbox_url/token, mint stub absent
	previous := doHost
	doHost = func(method string, payload any) (json.RawMessage, error) {
		return json.RawMessage(`{}`), nil
	}
	t.Cleanup(func() { doHost = previous })

	if _, err := runPrismTurn(cred, "", "hi", "gpt-5.6-sol", "medium", time.Second); err == nil {
		t.Fatal("expected an error when no sandbox can be obtained")
	}
}
