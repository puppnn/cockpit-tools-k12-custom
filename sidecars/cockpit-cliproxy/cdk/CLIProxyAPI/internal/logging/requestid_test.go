package logging

import (
	"encoding/hex"
	"testing"
)

func TestGenerateRequestIDUses128Bits(t *testing.T) {
	seen := make(map[string]struct{}, 1_000)
	for i := 0; i < 1_000; i++ {
		id := GenerateRequestID()
		if len(id) != 32 {
			t.Fatalf("request ID length = %d, want 32: %q", len(id), id)
		}
		if _, err := hex.DecodeString(id); err != nil {
			t.Fatalf("request ID is not hexadecimal: %q: %v", id, err)
		}
		if _, exists := seen[id]; exists {
			t.Fatalf("duplicate request ID generated: %q", id)
		}
		seen[id] = struct{}{}
	}
}
