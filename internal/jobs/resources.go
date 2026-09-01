package jobs

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log"
	"net/http"
	"os"
	"time"

	"codex/backend/internal/secnet"
)

// RefreshResources replica actualizarRecursos() de yape.js: se autentica contra
// el servicio externo y refresca banners/anuncios en memoria (resources.Store).
// Las credenciales salen del código y van a env (con valores por defecto).
func (j *Jobs) RefreshResources(ctx context.Context) {
	pin := envOr("RESOURCES_PIN", "123456")
	email := envOr("RESOURCES_EMAIL", "camilaelite0@gmail.com")
	httpc := secnet.Client(20 * time.Second)

	hdr := map[string]string{
		"User-Agent":   "Mozilla/5.0 (Linux; Android 8.0.0; SM-G955U) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/138.0.0.0 Mobile Safari/537.36",
		"Content-Type": "application/json",
		"Accept":       "application/json, text/plain, */*",
	}

	// 1) login
	loginBody, _ := json.Marshal(map[string]string{"pin": pin, "email": email})
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, "https://server-yape.vercel.app/api/auth/access", bytes.NewReader(loginBody))
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	resp, err := httpc.Do(req)
	if err != nil {
		log.Printf("[cron recursos] login error: %v", err)
		return
	}
	var auth struct {
		Status bool   `json:"status"`
		Token  string `json:"token"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&auth)
	resp.Body.Close()
	if !auth.Status || auth.Token == "" {
		log.Println("[cron recursos] status no es true")
		return
	}

	// 2) recursos
	req2, _ := http.NewRequestWithContext(ctx, http.MethodGet, "https://server-yape.vercel.app/api/resources/get_resources", nil)
	req2.Header.Set("Accept", "application/json")
	req2.Header.Set("Authorization", "Bearer "+auth.Token)
	resp2, err := httpc.Do(req2)
	if err != nil {
		log.Printf("[cron recursos] get_resources error: %v", err)
		return
	}
	data, _ := io.ReadAll(resp2.Body)
	resp2.Body.Close()

	var payload struct {
		Banners    []map[string]any `json:"banners"`
		Publicitys []map[string]any `json:"publicitys"`
	}
	if err := json.Unmarshal(data, &payload); err != nil {
		log.Printf("[cron recursos] json error: %v", err)
		return
	}

	banners := mapImg(payload.Banners, false)
	anuncios := mapImg(payload.Publicitys, true)
	j.Resources.Set(banners, anuncios)
	log.Printf("[cron recursos] actualizados: banners=%d anuncios=%d", len(banners), len(anuncios))
}

// mapImg replica .map(({img, ...rest}) => ({...rest, imgUrl: img[, activo:true])).
func mapImg(items []map[string]any, addActivo bool) []map[string]any {
	out := make([]map[string]any, 0, len(items))
	for _, it := range items {
		m := map[string]any{}
		for k, v := range it {
			if k == "img" {
				continue
			}
			m[k] = v
		}
		if img, ok := it["img"]; ok {
			m["imgUrl"] = img
		}
		if addActivo {
			m["activo"] = true
		}
		out = append(out, m)
	}
	return out
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}
