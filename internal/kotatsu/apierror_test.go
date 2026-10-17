package kotatsu

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestClientDecodesWorkerError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = w.Write([]byte(`{"error":"cloudflare_challenge","message":"cloudflare challenge at https://x/y","source":"ONISAGA_EN","url":"https://x/y","upstream_status":null}`))
	}))
	defer srv.Close()

	_, err := New(srv.URL, "k").Sources(context.Background())
	var ke *APIError
	if !errors.As(err, &ke) {
		t.Fatalf("want *APIError, got %T: %v", err, err)
	}
	if !ke.IsCloudflare() || ke.Source != "ONISAGA_EN" {
		t.Fatalf("decoded = %+v", ke)
	}
	if ke.Error() == "" {
		t.Fatal("empty error text")
	}
}

func TestClientFallsBackToTextError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "unknown source", http.StatusNotFound)
	}))
	defer srv.Close()

	_, err := New(srv.URL, "k").Sources(context.Background())
	var ke *APIError
	if errors.As(err, &ke) {
		t.Fatalf("plain text should not decode as APIError: %+v", ke)
	}
	if err == nil {
		t.Fatal("want error")
	}
}
