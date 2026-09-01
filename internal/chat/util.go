package chat

import (
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"strings"
)

// newUUID genera un UUID v4 (para jti y client_nonce), sin dependencias extra.
func newUUID() string {
	var b [16]byte
	_, _ = rand.Read(b[:])
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}

// randToken genera un token aleatorio en base64url (para ws-ticket).
func randToken(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return base64.RawURLEncoding.EncodeToString(b)
}

func lowerTrim(s string) string {
	return strings.ToLower(strings.TrimSpace(s))
}

// sprintfLike construye el filtro de búsqueda de listConversationsForAdmin usando
// el placeholder $idx (email/nombre/apellidos/telefono/app).
func sprintfLike(idx int) string {
	p := fmt.Sprintf("$%d", idx)
	return " and (lower(u.email) like " + p +
		" or lower(coalesce(u.nombre, '')) like " + p +
		" or lower(coalesce(u.apellidos, '')) like " + p +
		" or lower(coalesce(u.telefono, '')) like " + p +
		" or lower(c.app_name) like " + p + ")"
}

// parseReactions decodifica el json_agg de reacciones a []Reaction.
func parseReactions(raw []byte) []Reaction {
	if len(raw) == 0 {
		return []Reaction{}
	}
	var items []struct {
		Emoji   string  `json:"emoji"`
		Count   int     `json:"count"`
		UserIDs []int64 `json:"userIds"`
	}
	if err := json.Unmarshal(raw, &items); err != nil {
		return []Reaction{}
	}
	out := make([]Reaction, 0, len(items))
	for _, it := range items {
		ids := it.UserIDs
		if ids == nil {
			ids = []int64{}
		}
		out = append(out, Reaction{Emoji: it.Emoji, Count: it.Count, UserIDs: ids})
	}
	return out
}
