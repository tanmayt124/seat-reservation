package httpapi

import (
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"
)

func TestAdmissionShedsOnlyAfterWait(t *testing.T) {
	release := make(chan struct{})
	entered := make(chan struct{})
	slow := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		entered <- struct{}{}
		<-release
		w.WriteHeader(http.StatusCreated)
	})
	h := withRequestID(admission(1, 50*time.Millisecond, nil)(slow))

	var wg sync.WaitGroup
	first := httptest.NewRecorder()
	wg.Add(1)
	go func() { defer wg.Done(); h.ServeHTTP(first, httptest.NewRequest("POST", "/", nil)) }()
	<-entered

	// The only slot is taken: the second request waits 50ms, then gets 429.
	second := httptest.NewRecorder()
	start := time.Now()
	h.ServeHTTP(second, httptest.NewRequest("POST", "/", nil))
	if second.Code != http.StatusTooManyRequests || second.Header().Get("Retry-After") != "1" {
		t.Fatalf("second: %d retry-after=%q", second.Code, second.Header().Get("Retry-After"))
	}
	if waited := time.Since(start); waited < 50*time.Millisecond {
		t.Fatalf("shed after %s, before the wait elapsed", waited)
	}

	close(release)
	wg.Wait()
	if first.Code != http.StatusCreated {
		t.Fatalf("first: %d", first.Code)
	}
}

func TestAdmissionDisabledByDefault(t *testing.T) {
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusTeapot) })
	rec := httptest.NewRecorder()
	admission(0, time.Second, nil)(next).ServeHTTP(rec, httptest.NewRequest("POST", "/", nil))
	if rec.Code != http.StatusTeapot {
		t.Fatalf("disabled limiter changed the response: %d", rec.Code)
	}
}
