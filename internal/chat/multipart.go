package chat

import (
	"compress/gzip"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strconv"

	"github.com/go-chi/chi/v5"
)

func chiParam(r *http.Request, key string) string {
	return chi.URLParam(r, key)
}

// readBodyOptional intenta decodificar el body JSON sin fallar si está vacío.
func readBodyOptional(r *http.Request, v any) error {
	if r.Body == nil {
		return nil
	}
	dec := json.NewDecoder(io.LimitReader(r.Body, 1<<20))
	if err := dec.Decode(v); err != nil && err != io.EOF {
		return err
	}
	return nil
}

// encFromFields arma un *Encrypted desde los campos del multipart (si vienen).
func encFromFields(fields map[string]string) *Encrypted {
	ct := fields["ciphertext"]
	iv := fields["iv"]
	tag := fields["tag"]
	if ct == "" && iv == "" {
		// también puede venir un objeto JSON "encrypted"
		if raw := fields["encrypted"]; raw != "" {
			var e Encrypted
			if json.Unmarshal([]byte(raw), &e) == nil {
				return &e
			}
		}
		return &Encrypted{}
	}
	return &Encrypted{Ciphertext: ct, IV: iv, Tag: tag}
}

func replyFromFields(fields map[string]string) *int64 {
	for _, k := range []string{"replyToMessageId", "reply_to_message_id", "replyTo"} {
		if v := fields[k]; v != "" {
			if n, err := strconv.ParseInt(v, 10, 64); err == nil && n > 0 {
				return &n
			}
		}
	}
	return nil
}

func uploadText(fields map[string]string) string {
	for _, k := range []string{"text", "message", "mensaje"} {
		if v := fields[k]; v != "" {
			return v
		}
	}
	return ""
}

func durationFromFields(fields map[string]string) *int {
	for _, k := range []string{"durationMs", "duration_ms"} {
		if v := fields[k]; v != "" {
			if n, err := strconv.Atoi(v); err == nil {
				return &n
			}
		}
	}
	return nil
}

func gzipReader(r io.Reader) (io.ReadCloser, error) {
	return gzip.NewReader(r)
}

// writeChatErrOr usa el statusCode del chatErr si lo es; si no, un fallback.
func writeChatErrOr(w http.ResponseWriter, err error, fallbackStatus int, fallbackMsg string) {
	var ce chatErr
	if errors.As(err, &ce) {
		writeJSON(w, ce.status, map[string]any{"ok": false, "error": ce.msg})
		return
	}
	writeJSON(w, fallbackStatus, map[string]any{"ok": false, "error": fallbackMsg})
}
