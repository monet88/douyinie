package main

import (
	"net"
	"os"
	"testing"
)

func TestFindAvailablePort(t *testing.T) {
	port := findAvailablePort(8080)
	if port <= 0 || port > 65535 {
		t.Fatalf("invalid allocated port: %d", port)
	}

	// Occupy port and ensure fallback allocates a new ephemeral port
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen error: %v", err)
	}
	defer l.Close()
	occupiedPort := l.Addr().(*net.TCPAddr).Port

	fallbackPort := findAvailablePort(occupiedPort)
	if fallbackPort == occupiedPort {
		t.Fatalf("expected different fallback port when %d occupied, got %d", occupiedPort, fallbackPort)
	}
	if fallbackPort <= 0 || fallbackPort > 65535 {
		t.Fatalf("invalid fallback port: %d", fallbackPort)
	}
}

func TestResolveDefaultDataDir(t *testing.T) {
	t.Setenv("DOUYINIE_DATA_DIR", t.TempDir())
	dir := resolveDefaultDataDir()
	if dir == "" {
		t.Fatal("expected non-empty data dir")
	}

	t.Setenv("DOUYINIE_DATA_DIR", "")
	dir2 := resolveDefaultDataDir()
	if dir2 == "" {
		t.Fatal("expected non-empty default data dir")
	}
	if _, err := os.Stat(dir2); os.IsNotExist(err) {
		t.Fatalf("expected data dir to exist: %s", dir2)
	}
}
