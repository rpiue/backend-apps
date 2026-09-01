// Command optimizeattach recomprime in-place los adjuntos de imagen existentes
// del chat (resize 1600px + JPEG q80, en Go puro). Es idempotente: si una imagen
// ya está optimizada, la deja igual. No toca videos/audios ni mensajes.
//
// Uso:  DATABASE_URL=postgres://chat_app:...@127.0.0.1:5432/chat go run ./cmd/optimizeattach
//
// Lee cada archivo (descomprime si storage_encoding=gzip), lo optimiza, lo vuelve
// a escribir (gzip si reduce) y actualiza size_bytes/mime_type/storage_encoding.
package main

import (
	"bytes"
	"compress/gzip"
	"context"
	"io"
	"log"
	"os"
	"strings"

	"github.com/jackc/pgx/v5/pgxpool"

	"codex/backend/internal/chat"
)

func main() {
	dsn := os.Getenv("DATABASE_URL")
	if dsn == "" {
		log.Fatal("falta DATABASE_URL")
	}
	ctx := context.Background()
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		log.Fatalf("db: %v", err)
	}
	defer pool.Close()

	rows, err := pool.Query(ctx,
		`select id, storage_path, mime_type, storage_encoding, size_bytes
		 from chat_attachments where mime_type like 'image/%' order by id`)
	if err != nil {
		log.Fatalf("query: %v", err)
	}
	type rec struct {
		id       int64
		path     string
		mime     string
		encoding string
		size     int64
	}
	var list []rec
	for rows.Next() {
		var r rec
		if err := rows.Scan(&r.id, &r.path, &r.mime, &r.encoding, &r.size); err != nil {
			log.Fatalf("scan: %v", err)
		}
		list = append(list, r)
	}
	rows.Close()

	var done, savedBytes int64
	for _, r := range list {
		raw, err := os.ReadFile(r.path)
		if err != nil {
			log.Printf("[skip %d] no se pudo leer %s: %v", r.id, r.path, err)
			continue
		}
		// Descomprimir si estaba en gzip.
		if r.encoding == "gzip" {
			gz, err := gzip.NewReader(bytes.NewReader(raw))
			if err != nil {
				log.Printf("[skip %d] gzip: %v", r.id, err)
				continue
			}
			dec, err := io.ReadAll(gz)
			gz.Close()
			if err != nil {
				log.Printf("[skip %d] gunzip: %v", r.id, err)
				continue
			}
			raw = dec
		}

		out, outMime, changed := chat.OptimizeImage(raw, r.mime)
		if !changed {
			continue
		}

		// Re-comprimir con gzip si reduce.
		var gzbuf bytes.Buffer
		w, _ := gzip.NewWriterLevel(&gzbuf, gzip.BestSpeed)
		_, _ = w.Write(out)
		_ = w.Close()
		encoding := "identity"
		final := out
		if gzbuf.Len()+64 < len(out) {
			encoding = "gzip"
			final = gzbuf.Bytes()
		}

		if err := os.WriteFile(r.path, final, 0o600); err != nil {
			log.Printf("[skip %d] escribir: %v", r.id, err)
			continue
		}
		if _, err := pool.Exec(ctx,
			`update chat_attachments set size_bytes=$2, mime_type=$3, storage_encoding=$4 where id=$1`,
			r.id, int64(len(final)), outMime, encoding); err != nil {
			log.Printf("[warn %d] update DB: %v", r.id, err)
			continue
		}
		if r.size > int64(len(final)) {
			savedBytes += r.size - int64(len(final))
		}
		done++
	}
	log.Printf("✅ %d/%d imágenes optimizadas (%s ahorrados)", done, len(list), humanBytes(savedBytes))
}

func humanBytes(b int64) string {
	const u = 1024
	if b < u {
		return itoa(b) + " B"
	}
	div, exp := int64(u), 0
	for n := b / u; n >= u; n /= u {
		div *= u
		exp++
	}
	return itoa(b/div) + string("KMGT"[exp]) + "B"
}

func itoa(n int64) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var b strings.Builder
	var digs []byte
	for n > 0 {
		digs = append(digs, byte('0'+n%10))
		n /= 10
	}
	if neg {
		b.WriteByte('-')
	}
	for i := len(digs) - 1; i >= 0; i-- {
		b.WriteByte(digs[i])
	}
	return b.String()
}
