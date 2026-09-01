package middleware

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
	"time"

	"codex/backend/internal/cache"
)

// redisURL de un Redis de pruebas (docker en 6399). Si no está, se salta el test.
func testCache(t *testing.T) *cache.Cache {
	t.Helper()
	url := os.Getenv("TEST_REDIS_URL")
	if url == "" {
		url = "redis://localhost:6399/0"
	}
	c, err := cache.New(url)
	if err != nil {
		t.Skipf("sin Redis de pruebas (%s): %v", url, err)
	}
	return c
}

func TestAllowWindow(t *testing.T) {
	c := testCache(t)
	defer c.Close()
	l := NewLimiter(c, nil)
	ctx := context.Background()
	key := "unit-" + time.Now().Format("150405.000")

	// 3 permitidas, la 4ª bloqueada dentro de la misma ventana.
	for i := 1; i <= 3; i++ {
		if ok, _ := l.Allow(ctx, "t", key, 3, time.Minute); !ok {
			t.Fatalf("petición %d debería permitirse", i)
		}
	}
	ok, retry := l.Allow(ctx, "t", key, 3, time.Minute)
	if ok {
		t.Fatal("la 4ª petición debería bloquearse")
	}
	if retry <= 0 {
		t.Fatalf("Retry-After debería ser > 0, fue %d", retry)
	}
}

func TestLimitMiddleware429(t *testing.T) {
	c := testCache(t)
	defer c.Close()
	l := NewLimiter(c, nil)
	h := l.Limit("mw-"+time.Now().Format("150405.000"), 2, time.Minute)(
		http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(200) }))

	codes := []int{}
	for i := 0; i < 3; i++ {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest("POST", "/x", nil)
		req.RemoteAddr = "203.0.113.7:1234"
		h.ServeHTTP(rec, req)
		codes = append(codes, rec.Code)
	}
	if codes[0] != 200 || codes[1] != 200 || codes[2] != http.StatusTooManyRequests {
		t.Fatalf("esperado [200 200 429], obtenido %v", codes)
	}
}

func TestClientIPTrustedProxy(t *testing.T) {
	// Sin proxies confiables: se ignora X-Forwarded-For (anti-spoofing).
	l := NewLimiter(nil, nil)
	req := httptest.NewRequest("GET", "/", nil)
	req.RemoteAddr = "198.51.100.9:5555"
	req.Header.Set("X-Forwarded-For", "1.2.3.4")
	if got := l.ClientIP(req); got != "198.51.100.9" {
		t.Fatalf("sin proxy confiable debe usar el socket, got %s", got)
	}

	// Con el peer en la lista confiable: se honra X-Forwarded-For.
	l2 := NewLimiter(nil, []string{"198.51.100.0/24"})
	if got := l2.ClientIP(req); got != "1.2.3.4" {
		t.Fatalf("con proxy confiable debe usar XFF, got %s", got)
	}
}
