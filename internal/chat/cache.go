package chat

import (
	"context"
	"strconv"
	"time"
)

// Estrategia de cache (Redis) para escalar a miles de chats:
//
//   - Cada conversación tiene una "versión" entera en Redis (chat:cv:{id}). Toda
//     escritura (mensaje, reacción, borrado) la incrementa con bumpConv. Las
//     páginas de mensajes se cachean con la versión embebida en la clave, así un
//     bump invalida TODAS las páginas viejas sin tener que borrarlas una a una.
//   - La lista de conversaciones del admin se cachea con TTL corto (staleness
//     aceptable; el estado "online" se añade fresco desde memoria, no se cachea).
//
// Si d.cache es nil, todo cae a la consulta directa (sin romper nada).

const (
	msgPageTTL   = 2 * time.Minute
	adminListTTL = 4 * time.Second
)

func (d *DB) convVer(ctx context.Context, conversationID int64) int64 {
	if d.cache == nil {
		return 0
	}
	v, err := d.cache.Client().Get(ctx, "chat:cv:"+strconv.FormatInt(conversationID, 10)).Int64()
	if err != nil {
		return 0
	}
	return v
}

// bumpConv invalida las páginas de mensajes cacheadas de una conversación.
func (d *DB) bumpConv(ctx context.Context, conversationID int64) {
	if d.cache == nil {
		return
	}
	_ = d.cache.Client().Incr(ctx, "chat:cv:"+strconv.FormatInt(conversationID, 10)).Err()
}

func ptrKey(p *int64) string {
	if p == nil {
		return "0"
	}
	return strconv.FormatInt(*p, 10)
}
