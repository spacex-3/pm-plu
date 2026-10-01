package main

import (
	"encoding/json"
	"fmt"
)

const (
	hostHTTPDo          = "host.http.do"
	hostHTTPDoStream    = "host.http.do_stream"
	hostHTTPStreamRead  = "host.http.stream_read"
	hostHTTPStreamClose = "host.http.stream_close"
	hostStreamEmit      = "host.stream.emit"
	hostStreamClose     = "host.stream.close"
	hostLog             = "host.log"
)

// doHost is replaced in tests.
var doHost = callHost

func unwrapHost(raw []byte) (json.RawMessage, error) {
	if len(raw) == 0 {
		return json.RawMessage(`{}`), nil
	}
	var env struct {
		OK     *bool           `json:"ok"`
		Result json.RawMessage `json:"result"`
		Error  *struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(raw, &env); err == nil && env.OK != nil {
		if !*env.OK {
			if env.Error != nil && env.Error.Message != "" {
				return nil, fmt.Errorf("%s", env.Error.Message)
			}
			return nil, fmt.Errorf("host call failed")
		}
		if len(env.Result) == 0 {
			return json.RawMessage(`{}`), nil
		}
		return env.Result, nil
	}
	return json.RawMessage(raw), nil
}
