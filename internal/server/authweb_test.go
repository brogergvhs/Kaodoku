package server

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestForwardedHeadersRequireTrustedProxy(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "http://kaodoku.test/anilist/connect", nil)
	req.RemoteAddr = "203.0.113.10:55123"
	req.Header.Set("X-Forwarded-For", "198.51.100.20")
	req.Header.Set("X-Forwarded-Proto", "https")
	req.Header.Set("X-Forwarded-Host", "public.example")

	if got := clientIP(req); got != "203.0.113.10" {
		t.Fatalf("clientIP untrusted = %q, want socket peer", got)
	}
	if secureRequest(req) {
		t.Fatal("secureRequest trusted forwarded proto from public peer")
	}
	if got := trackerRedirectURL(req, "anilist"); got != "http://kaodoku.test/anilist/callback" {
		t.Fatalf("redirect URL untrusted = %q", got)
	}

	req.RemoteAddr = "192.168.1.10:55123"
	if got := clientIP(req); got != "192.168.1.10" {
		t.Fatalf("clientIP private peer = %q, want socket peer", got)
	}

	req.RemoteAddr = "127.0.0.1:55123"
	if got := clientIP(req); got != "198.51.100.20" {
		t.Fatalf("clientIP trusted = %q, want forwarded IP", got)
	}
	if !secureRequest(req) {
		t.Fatal("secureRequest ignored trusted forwarded proto")
	}
	if got := trackerRedirectURL(req, "anilist"); got != "https://public.example/anilist/callback" {
		t.Fatalf("redirect URL trusted = %q", got)
	}

	req.Header.Set("X-Forwarded-For", "not-an-ip")
	if got := clientIP(req); got != "127.0.0.1" {
		t.Fatalf("clientIP invalid forwarded = %q, want proxy peer", got)
	}
}
