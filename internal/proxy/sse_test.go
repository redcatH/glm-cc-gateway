package proxy

import (
	"testing"
)

func TestSSEEventScannerReassembly(t *testing.T) {
	s := &sseEventScanner{}
	// 事件被 TCP chunk 边界劈开:重组后 ev2 收到事件 a,ev3 收到事件 b。
	ev1 := s.Feed([]byte("data: {\"a\""))
	ev2 := s.Feed([]byte(":1}\n\ndata: {\"b\":2}"))
	ev3 := s.Feed([]byte("\n\n"))
	if len(ev1) != 0 || len(ev2) != 1 || len(ev3) != 1 {
		t.Fatalf("event reassembly wrong: %d %d %d", len(ev1), len(ev2), len(ev3))
	}
	if string(ev2[0]) != "data: {\"a\":1}\n\n" {
		t.Fatalf("event a wrong: %q", ev2[0])
	}
	if string(ev3[0]) != "data: {\"b\":2}\n\n" {
		t.Fatalf("event b wrong: %q", ev3[0])
	}
	if tail := s.Flush(); len(tail) != 0 {
		t.Fatalf("tail should be empty, got %q", tail)
	}
}

func TestExtractUsageFromEvents(t *testing.T) {
	start := []byte("event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"usage\":{\"input_tokens\":100,\"cache_read_input_tokens\":500,\"cache_creation_input_tokens\":50}}}\n\n")
	delta := []byte("data: {\"type\":\"message_delta\",\"delta\":{},\"usage\":{\"output_tokens\":42}}\n\n")

	in, _, cr, cc, ok := extractUsage(start)
	if !ok || in != 100 || cr != 500 || cc != 50 {
		t.Fatalf("message_start usage wrong: in=%d cr=%d cc=%d ok=%v", in, cr, cc, ok)
	}
	in2, out2, _, _, ok2 := extractUsage(delta)
	if !ok2 || out2 != 42 || in2 != 0 {
		t.Fatalf("message_delta usage wrong: %d %d %v", in2, out2, ok2)
	}
	// 无 usage 事件。
	_, _, _, _, ok3 := extractUsage([]byte("data: {\"type\":\"content_block_start\"}\n\n"))
	if ok3 {
		t.Fatal("non-usage event should return ok=false")
	}
}

func TestExtractNonStreamUsage(t *testing.T) {
	body := []byte(`{"id":"x","usage":{"input_tokens":10,"output_tokens":5,"cache_read_input_tokens":2,"cache_creation_input_tokens":3}}`)
	in, out, cr, cc, ok := extractNonStreamUsage(body)
	if !ok || in != 10 || out != 5 || cr != 2 || cc != 3 {
		t.Fatalf("usage wrong: %d %d %d %d %v", in, out, cr, cc, ok)
	}
}
