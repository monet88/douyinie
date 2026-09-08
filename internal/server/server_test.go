package server

import (
	"testing"
	"time"
)

// TestProductionWriteTimeoutCoversLongOperations pins the production HTTP
// write timeout: localhost synchronous ML endpoints legitimately run for
// multiple minutes before the first response byte, so the timeout must equal
// the named long-operation budget and stay finite/non-zero. No sleeping.
func TestProductionWriteTimeoutCoversLongOperations(t *testing.T) {
	s := New(Config{Addr: "127.0.0.1:0"})
	if s.server == nil {
		t.Fatal("expected production http.Server to be configured")
	}
	if got := s.server.WriteTimeout; got != runtimeHostWriteTimeout {
		t.Fatalf("WriteTimeout = %v, want named runtimeHostWriteTimeout %v", got, runtimeHostWriteTimeout)
	}
	if s.server.WriteTimeout <= 0 {
		t.Fatal("WriteTimeout must remain finite/non-zero")
	}
	if want := 30 * time.Minute; s.server.WriteTimeout != want {
		t.Fatalf("WriteTimeout = %v, want %v", s.server.WriteTimeout, want)
	}
	if got := s.server.ReadTimeout; got != 30*time.Second {
		t.Fatalf("ReadTimeout = %v, want 30s (must stay unchanged)", got)
	}
}
