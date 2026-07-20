package main

import (
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// probeRunning finds a real hub's /health and recognizes the "service":"messenger" body.
func TestProbeRunning_DetectsRealHub(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/health" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"service":"messenger"}`))
	}))
	defer srv.Close()

	addr := strings.TrimPrefix(srv.URL, "http://")
	if !probeRunning(addr) {
		t.Fatalf("probeRunning(%q) = false, want true", addr)
	}
}

// probeRunning against nobody listening returns false promptly — well under the 2s
// client timeout, so a future regression that drops the timeout gets caught.
func TestProbeRunning_NothingListening_FastFalse(t *testing.T) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := l.Addr().String()
	l.Close() // now nothing is listening on addr

	start := time.Now()
	got := probeRunning(addr)
	elapsed := time.Since(start)

	if got {
		t.Fatalf("probeRunning(%q) = true, want false (nothing listening)", addr)
	}
	if elapsed > time.Second {
		t.Fatalf("probeRunning took %v, want well under the 2s timeout", elapsed)
	}
}

// probeRunning against a server that answers /health but isn't messenger returns false.
func TestProbeRunning_WrongService(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"service":"something-else"}`))
	}))
	defer srv.Close()

	addr := strings.TrimPrefix(srv.URL, "http://")
	if probeRunning(addr) {
		t.Fatalf("probeRunning(%q) = true, want false (not messenger)", addr)
	}
}

// isAddrInUse recognizes the real EADDRINUSE error from a deliberate double-bind, and
// rejects an unrelated error (a DNS/lookup failure has a different underlying errno).
func TestIsAddrInUse(t *testing.T) {
	l1, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l1.Close()

	_, err = net.Listen("tcp", l1.Addr().String())
	if err == nil {
		t.Fatal("expected the second Listen on the same address to fail")
	}
	if !isAddrInUse(err) {
		t.Fatalf("isAddrInUse(%v) = false, want true", err)
	}

	_, dnsErr := net.Dial("tcp", "no-such-host.invalid:80")
	if dnsErr == nil {
		t.Fatal("expected dialing a bogus host to fail")
	}
	if isAddrInUse(dnsErr) {
		t.Fatalf("isAddrInUse(%v) = true, want false (unrelated error)", dnsErr)
	}
}
