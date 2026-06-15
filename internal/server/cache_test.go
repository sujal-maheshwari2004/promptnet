package server

import (
	"testing"
	"time"

	pb "promptnet/gen/promptnet/v1"
)

func TestTTLCache(t *testing.T) {
	c := newTTLCache(50 * time.Millisecond)
	r := &pb.GetPromptResponse{Uri: "u"}

	if _, ok := c.get("u"); ok {
		t.Fatal("empty cache should miss")
	}
	c.put("u", r)
	if got, ok := c.get("u"); !ok || got != r {
		t.Fatal("want hit immediately after put")
	}
	time.Sleep(60 * time.Millisecond)
	if _, ok := c.get("u"); ok {
		t.Fatal("entry should expire after ttl")
	}

	// A nil cache (disabled) is a no-op, never a panic.
	var nilc *ttlCache
	nilc.put("u", r)
	if _, ok := nilc.get("u"); ok {
		t.Fatal("nil cache should always miss")
	}
}
