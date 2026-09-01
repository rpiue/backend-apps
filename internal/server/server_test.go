package server

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
)

// buildDir crea un build falso del panel (index.html + un asset con hash).
func buildDir(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "assets"), 0o755); err != nil {
		t.Fatal(err)
	}
	write := func(name, body string) {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("index.html", "<html>panel</html>")
	write(filepath.Join("assets", "index-abc123.js"), "console.log(1)")
	return dir
}

// El panel vive bajo /api/chat/admin/web y el resto de /api/chat sigue siendo
// la API del chat.
func TestChatOrAdminWeb(t *testing.T) {
	dir := buildDir(t)
	chatAPI := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("chat-api"))
	})
	h := chatOrAdminWeb(chatAPI, spaHandler(dir, adminWebPrefix))

	cases := []struct {
		name, path string
		status     int
		body       string
	}{
		{"index", adminWebPrefix + "/", http.StatusOK, "<html>panel</html>"},
		{"asset", adminWebPrefix + "/assets/index-abc123.js", http.StatusOK, "console.log(1)"},
		{"ruta del SPA cae en index", adminWebPrefix + "/resumen", http.StatusOK, "<html>panel</html>"},
		{"sin barra final redirige", adminWebPrefix, http.StatusMovedPermanently, ""},
		{"la API del chat sigue viva", "/api/chat/admin/conversations", http.StatusOK, "chat-api"},
		// Clean() saca la ruta fuera del dir y ServeFile además rechaza los "..".
		{"no hay traversal", adminWebPrefix + "/../../../etc/passwd", http.StatusBadRequest, ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, c.path, nil))
			if rec.Code != c.status {
				t.Fatalf("status = %d, quería %d", rec.Code, c.status)
			}
			if c.body != "" && rec.Body.String() != c.body {
				t.Fatalf("body = %q, quería %q", rec.Body.String(), c.body)
			}
		})
	}
}
