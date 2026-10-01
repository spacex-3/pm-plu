package main

import "testing"

func mustBytes(t *testing.T, value any) []byte {
	t.Helper()
	raw, err := marshalCompact(value)
	if err != nil {
		t.Fatalf("marshalCompact: %v", err)
	}
	return raw
}
