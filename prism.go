package main

import (
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"time"
)

const (
	prismNewPath    = "/api/backend/1/new"
	prismStartPath  = "/api/llm/response_with_tools_start"
	prismStatusPath = "/api/llm/response_with_tools_status"

	maxPoolSize      = 60
	defaultKeepalive = 600
	pollInterval     = 1500 * time.Millisecond
)

type poolEntry struct {
	URL   string
	Token string
}

// attemptMeta carries everything one Prism call needs. It is rebuilt from the
// auth record on every executor request, so rotating the auth file is enough.
type attemptMeta struct {
	BaseURL   string
	UserAgent string
	Cookie    string
	ProjectID string
	UserID    string
	SeedURL   string
	SeedToken string
}

type passKind int

const (
	passOK passKind = iota
	passAuth
	passUnsupported
	passDead
	passTransient
	passFailed
	passTimeout
)

type passOutcome struct {
	kind passKind
	text string
	why  string
}

func metaFor(cred prismCredential) attemptMeta {
	cfg := loadedConfig()
	return attemptMeta{
		BaseURL:   cfg.baseURL(),
		UserAgent: cfg.userAgent(),
		Cookie:    cred.Cookie,
		ProjectID: cred.ProjectID,
		UserID:    cred.UserID,
		SeedURL:   normalizeSandboxURL(cred.SandboxURL),
		SeedToken: cred.SandboxToken,
	}
}

func normalizeSandboxURL(url string) string {
	url = strings.TrimSpace(url)
	if url == "" {
		return ""
	}
	if !strings.HasSuffix(url, "/") {
		url += "/"
	}
	return url
}

// ---------------------------------------------------------------------------
// sandbox pool

type sandboxPool struct {
	idle   chan poolEntry
	mu     sync.Mutex
	size   int
	max    int
	cred   attemptMeta
	seed   poolEntry
	seeded bool
}

var (
	poolsMu sync.Mutex
	pools   = map[string]*sandboxPool{}
)

// poolFor returns the pool for one cookie (one account). Multiple auth files
// get independent pools; every sandbox inside one pool shares that cookie.
func poolFor(cred prismCredential) *sandboxPool {
	m := metaFor(cred)
	poolsMu.Lock()
	defer poolsMu.Unlock()
	max := loadedConfig().concurrency()
	if max > maxPoolSize {
		max = maxPoolSize
	}
	if max < 1 {
		max = 1
	}
	if p, ok := pools[cred.Cookie]; ok {
		if p.max != max { // config changed: rebuild the pool lazily
			p = &sandboxPool{
				idle: make(chan poolEntry, max),
				max:  max,
				cred: m,
				seed: poolEntry{URL: m.SeedURL, Token: m.SeedToken},
			}
			pools[cred.Cookie] = p
		} else {
			p.mu.Lock()
			p.cred = m
			newSeed := poolEntry{URL: m.SeedURL, Token: m.SeedToken}
			if newSeed != p.seed {
				// The auth file was re-imported with a fresh sandbox: hand it
				// out exactly once so two requests never share one sandbox.
				p.seed = newSeed
				p.seeded = false
			}
			p.mu.Unlock()
		}
		return p
	}
	p := &sandboxPool{
		idle: make(chan poolEntry, max),
		max:  max,
		cred: m,
		seed: poolEntry{URL: m.SeedURL, Token: m.SeedToken},
	}
	pools[cred.Cookie] = p
	return p
}

func (p *sandboxPool) acquire(deadline time.Time) (poolEntry, error) {
	select {
	case e := <-p.idle:
		return e, nil
	default:
	}

	p.mu.Lock()
	seed := p.seed
	seeded := p.seeded
	p.mu.Unlock()

	if !seeded && seed.Token != "" && seed.URL != "" {
		p.mu.Lock()
		if !p.seeded {
			p.seeded = true
			p.size++ // the imported seed occupies pool capacity too
			p.mu.Unlock()
			return seed, nil
		}
		p.mu.Unlock()
	}

	// Growing the pool means a multi-second cold mint; only start one when the
	// caller still has time to spare for it.
	if time.Until(deadline) > 10*time.Second {
		p.mu.Lock()
		canMint := p.size < p.max
		if canMint {
			p.size++
		}
		p.mu.Unlock()
		if canMint {
			e, err := mintSandbox(p.cred)
			if err == nil {
				return e, nil
			}
			p.mu.Lock()
			if p.size > 0 {
				p.size--
			}
			p.mu.Unlock()
		}
	}

	remaining := time.Until(deadline)
	if remaining <= 0 {
		return poolEntry{}, fmt.Errorf("timed out waiting for a free Prism sandbox")
	}
	timer := time.NewTimer(remaining)
	defer timer.Stop()
	select {
	case e := <-p.idle:
		return e, nil
	case <-timer.C:
		p.mu.Lock()
		size, max := p.size, p.max
		p.mu.Unlock()
		return poolEntry{}, fmt.Errorf("timed out waiting for a free Prism sandbox (%d/%d busy)", size, max)
	}
}

func (p *sandboxPool) release(e poolEntry, dead bool) {
	if dead {
		p.mu.Lock()
		if p.size > 0 {
			p.size--
		}
		p.mu.Unlock()
		return
	}
	select {
	case p.idle <- e:
	default:
		// Idle queue full means the entry is lost; keep size honest.
		p.mu.Lock()
		if p.size > 0 {
			p.size--
		}
		p.mu.Unlock()
	}
}

func mintSandbox(m attemptMeta) (poolEntry, error) {
	status, decoded, err := prismPost(m, prismNewPath, map[string]any{}, mintTimeoutFor())
	if err != nil {
		return poolEntry{}, fmt.Errorf("sandbox mint failed: %s", err.Error())
	}
	if status != 200 {
		return poolEntry{}, fmt.Errorf("sandbox mint failed: http=%d %s", status, truncate(strings.TrimSpace(jsonString(decoded)), 200))
	}
	token := stringField(decoded, "token")
	url := stringField(decoded, "url")
	if token == "" || url == "" {
		return poolEntry{}, fmt.Errorf("sandbox mint response had no token/url")
	}
	return poolEntry{URL: normalizeSandboxURL(url), Token: token}, nil
}

func mintTimeoutFor() time.Duration {
	if t := time.Duration(loadedConfig().timeoutSeconds()) * time.Second; t > 0 && t < 60*time.Second {
		return t
	}
	return 60 * time.Second
}

// ---------------------------------------------------------------------------
// keepalive

var keepaliveOnce sync.Once

func startKeepalive() {
	keepaliveOnce.Do(func() {
		go keepaliveLoop()
	})
}

func keepaliveLoop() {
	for {
		period := defaultKeepalive
		if cfg := loadedConfig(); cfg.KeepaliveSeconds != 0 {
			period = cfg.KeepaliveSeconds
		}
		if period < 60 {
			period = 60
		}
		time.Sleep(time.Duration(period) * time.Second)

		poolsMu.Lock()
		snapshot := make([]*sandboxPool, 0, len(pools))
		for _, p := range pools {
			snapshot = append(snapshot, p)
		}
		poolsMu.Unlock()
		for _, p := range snapshot {
			p.pingIdle()
		}
	}
}

func (p *sandboxPool) pingIdle() {
	for {
		var e poolEntry
		select {
		case e = <-p.idle:
		default:
			return
		}
		if pingSandbox(p.cred, e) {
			p.release(e, false)
		} else {
			p.release(e, true)
		}
	}
}

func pingSandbox(m attemptMeta, entry poolEntry) bool {
	model := prismModels[0].UpstreamID
	deadline := time.Now().Add(90 * time.Second)
	out := prismPass(m, entry, "", "ping", model, "low", deadline)
	switch out.kind {
	case passOK:
		return true
	case passDead:
		return false
	default:
		return true // auth/model problems are not sandbox problems
	}
}

// ---------------------------------------------------------------------------
// one turn

func runPrismTurn(cred prismCredential, system, user, wantedModel, effort string, budget time.Duration) (string, error) {
	pool := poolFor(cred)
	m := pool.cred
	model := substituteFor(wantedModel)

	// One budget covers queue wait + turn: a request that queued for a minute
	// should not also get the full timeout on top of it.
	turnDeadline := time.Now().Add(budget)

	text, why := turnWithModel(m, pool, system, user, model, effort, turnDeadline)
	if text != "" || why == "" {
		return text, nil
	}

	if unsupportedReason(why) {
		for _, alt := range fallbacksFor(wantedModel) {
			if alt == model {
				continue
			}
			text, why = turnWithModel(m, pool, system, user, alt, effort, turnDeadline)
			if text != "" {
				rememberSubstitute(wantedModel, alt)
				return text, nil
			}
			if !unsupportedReason(why) {
				break
			}
		}
	}
	return "", fmt.Errorf("%s. Re-import a fresh Prism session (auth file) and retry", why)
}

// turnWithModel runs up to three start+poll passes, swapping in a fresh sandbox
// whenever the current one dies. It acquires its own entry and always releases
// it (possibly as dead) before returning. Returns text or a reason, never both.
func turnWithModel(m attemptMeta, pool *sandboxPool, system, user, model, effort string, deadline time.Time) (string, string) {
	last := ""
	var entry poolEntry
	hasEntry := false
	release := func(dead bool) {
		if hasEntry {
			pool.release(entry, dead)
			hasEntry = false
		}
	}
	defer release(false)
	for attempt := 0; attempt < 3; attempt++ {
		if time.Until(deadline) <= 5*time.Second {
			if last == "" {
				last = "out of time"
			}
			return "", last
		}
		if !hasEntry {
			e, err := pool.acquire(deadline)
			if err != nil {
				return "", err.Error()
			}
			entry = e
			hasEntry = true
		}
		out := prismPass(m, entry, system, user, model, effort, deadline)
		switch out.kind {
		case passOK:
			release(false)
			return out.text, ""
		case passAuth, passUnsupported, passFailed, passTimeout:
			release(false)
			return "", out.why
		case passDead:
			release(true)
			last = out.why
			sleepMin(2*time.Second, deadline)
		case passTransient:
			// The sandbox may be fine; hand it back and take another one.
			release(false)
			last = out.why
			sleepMin(3*time.Second, deadline)
		}
	}
	if last == "" {
		last = "no response"
	}
	return "", last
}

func sleepMin(d time.Duration, deadline time.Time) {
	if time.Until(deadline) < d {
		d = time.Until(deadline)
	}
	if d > 0 {
		time.Sleep(d)
	}
}

// prismPass is one start(+poll) pass against a specific sandbox.
func prismPass(m attemptMeta, entry poolEntry, system, user, model, effort string, deadline time.Time) passOutcome {
	inp := []any{}
	if system != "" {
		inp = append(inp, map[string]any{
			"type": "message", "role": "system",
			"content": []any{map[string]any{"type": "input_text", "text": system}},
		})
	}
	inp = append(inp, map[string]any{
		"type": "message", "role": "user",
		"content": []any{map[string]any{"type": "input_text", "text": user}},
	})

	meta := map[string]any{
		"model":            model,
		"reasoning_effort": effort,
		"frontend_origin":  strings.TrimRight(m.BaseURL, "/"),
		"sandbox_url":      entry.URL,
		"sandbox_token":    entry.Token,
	}
	if m.ProjectID != "" {
		meta["projectId"] = m.ProjectID
	}
	if m.UserID != "" {
		meta["userId"] = m.UserID
	}

	startTimeout := time.Until(deadline)
	if startTimeout > 180*time.Second {
		startTimeout = 180 * time.Second
	}
	if startTimeout < 60*time.Second {
		startTimeout = 60 * time.Second
	}
	status, decoded, err := prismPost(m, prismStartPath, map[string]any{"input": inp, "metadata": meta}, startTimeout)
	if err != nil {
		return passOutcome{kind: passTransient, why: "start error: " + err.Error()}
	}
	if status != 200 || stringField(decoded, "request_id") == "" {
		why := fmt.Sprintf("start http=%d %s", status, truncate(strings.TrimSpace(jsonString(decoded)), 300))
		if status == 400 || status == 401 || status == 403 {
			return passOutcome{kind: passAuth, why: why} // auth-shaped: retrying cannot help
		}
		return passOutcome{kind: passTransient, why: why}
	}

	// `start` can finish the turn outright, and then it carries no turn_state.
	st := stringField(decoded, "status")
	_, hasTurn := decoded["turn_state"]
	if st == "completed" || st == "error" || st == "failed" || !hasTurn {
		return classifyTerminal(decoded, "start returned "+st+" with no turn_state")
	}

	reqState := map[string]any{
		"request_id": decoded["request_id"],
		"turn_state": decoded["turn_state"],
	}
	for time.Until(deadline) > 0 {
		pollTimeout := time.Until(deadline)
		if pollTimeout > 60*time.Second {
			pollTimeout = 60 * time.Second
		}
		if pollTimeout < 5*time.Second {
			pollTimeout = 5 * time.Second
		}
		status, d, err := prismPost(m, prismStatusPath, reqState, pollTimeout)
		if err != nil || status != 200 {
			body := ""
			if err != nil {
				body = err.Error()
			} else {
				body = truncate(strings.TrimSpace(jsonString(d)), 300)
			}
			return passOutcome{kind: passFailed, why: fmt.Sprintf("status http=%d %s", status, body)}
		}
		if ts, ok := d["turn_state"]; ok && ts != nil {
			reqState["turn_state"] = ts
		}
		switch stringField(d, "status") {
		case "completed", "error", "failed":
			return classifyTerminal(d, "")
		}
		time.Sleep(pollInterval)
	}
	return passOutcome{kind: passTimeout, why: fmt.Sprintf("timed out after %ds", loadedConfig().timeoutSeconds())}
}

func classifyTerminal(decoded map[string]any, fallbackWhy string) passOutcome {
	rs, _ := decoded["response"].(map[string]any)
	if rs == nil {
		return passOutcome{kind: passTransient, why: fallbackWhy}
	}
	if stringField(rs, "status") == "success" {
		return passOutcome{kind: passOK, text: responseOutputTextFrom(rs)}
	}
	pay, _ := rs["payload"].(map[string]any)
	body := ""
	if debug, ok := pay["codexRequestDebug"].(map[string]any); ok {
		if errObj, ok := debug["error"].(map[string]any); ok {
			body = stringField(errObj, "bodyText")
		}
	}
	reason := stringField(pay, "reason")
	why := truncate(strings.TrimSpace(reason+": "+stringField(pay, "message")+" "+body), 400)
	if why == "" || why == ": " {
		why = fallbackWhy
	}
	if unsupportedReason(body) {
		return passOutcome{kind: passUnsupported, why: why}
	}
	if reason == "sandbox_reconnecting" || numberField(pay, "httpStatus") == 504 {
		return passOutcome{kind: passDead, why: why} // cold or dead sandbox
	}
	return passOutcome{kind: passTransient, why: why}
}

func unsupportedReason(reason string) bool {
	return strings.Contains(strings.ToLower(reason), "unsupported assistant model")
}

func responseOutputTextFrom(response map[string]any) string {
	pay, _ := response["payload"].(map[string]any)
	out, _ := pay["output"].([]any)
	var parts []string
	for _, item := range out {
		object, ok := item.(map[string]any)
		if !ok || stringField(object, "type") != "message" {
			continue
		}
		content, _ := object["content"].([]any)
		for _, part := range content {
			if piece, ok := part.(map[string]any); ok {
				if text, ok := piece["text"].(string); ok {
					parts = append(parts, text)
				}
			}
		}
	}
	return strings.Join(parts, "")
}

func numberField(object map[string]any, key string) int64 {
	switch v := object[key].(type) {
	case json.Number:
		n, err := v.Int64()
		if err != nil {
			return 0
		}
		return n
	case float64:
		return int64(v)
	case int64:
		return v
	default:
		return 0
	}
}

// ---------------------------------------------------------------------------
// host HTTP plumbing

func prismPost(m attemptMeta, path string, body any, timeout time.Duration) (int, map[string]any, error) {
	payload, err := marshalCompact(body)
	if err != nil {
		return 0, nil, err
	}
	base := strings.TrimRight(m.BaseURL, "/")
	req := map[string]any{
		"method": "POST",
		"url":    base + path,
		"headers": map[string][]string{
			"content-type": {"application/json"},
			"cookie":       {m.Cookie},
			"user-agent":   {m.UserAgent},
			"origin":       {base},
			"referer":      {base + "/"},
			"accept":       {"*/*"},
		},
		"body": payload,
		"wire_profile": map[string]any{
			"disable_auto_compression": true,
		},
	}
	if timeout <= 0 {
		timeout = 120 * time.Second
	}
	type result struct {
		raw json.RawMessage
		err error
	}
	ch := make(chan result, 1)
	go func() {
		raw, err := doHost(hostHTTPDo, req)
		ch <- result{raw, err}
	}()
	var timer <-chan time.Time
	if timeout > 0 {
		timer = time.After(timeout)
	}
	select {
	case r := <-ch:
		if r.err != nil {
			return 0, nil, r.err
		}
		var resp struct {
			StatusCode int    `json:"status_code"`
			Body       []byte `json:"body"`
		}
		if err := json.Unmarshal(r.raw, &resp); err != nil {
			return 0, nil, err
		}
		if resp.StatusCode == 0 {
			resp.StatusCode = 200
		}
		decoded := map[string]any{}
		if len(resp.Body) > 0 {
			_ = decodeJSONUseNumber(resp.Body, &decoded)
		}
		return resp.StatusCode, decoded, nil
	case <-timer:
		return 0, nil, fmt.Errorf("prism request timed out after %s", timeout)
	}
}
