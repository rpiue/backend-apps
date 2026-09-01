package chat

import (
	"context"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// validAutomationMediaPath verifica que la ruta esté DENTRO del UploadDir. Evita
// que un admin persista (vía saveAutomation) una ruta arbitraria del disco.
func (s *Service) validAutomationMediaPath(p string) bool {
	if strings.TrimSpace(p) == "" {
		return false
	}
	base, err := filepath.Abs(s.cfg.UploadDir)
	if err != nil {
		return false
	}
	abs, err := filepath.Abs(p)
	if err != nil {
		return false
	}
	return abs == base || strings.HasPrefix(abs, base+string(os.PathSeparator))
}

// postAutomationMedia sube una imagen/video para una respuesta automática: valida,
// sanea y guarda en disco, y devuelve los metadatos. El panel los reenvía luego en
// saveAutomation como "attachment". NO crea ningún mensaje.
func (s *Service) postAutomationMedia(w http.ResponseWriter, r *http.Request) {
	buf, filename, mime, _, err := readUpload(r)
	if err != nil || len(buf) == 0 {
		writeErr(w, http.StatusBadRequest, "Archivo no permitido")
		return
	}
	prepared, err := validateAndPrepareUpload(buf, mime, nil)
	if err != nil {
		writeChatErr(w, err)
		return
	}
	if prepared.Kind != "image" && prepared.Kind != "video" {
		writeErr(w, http.StatusBadRequest, "Solo se permiten imágenes o videos")
		return
	}
	ext := strings.ToLower(filepath.Ext(filename))
	safeName := "auto-" + newUUID() + ext
	diskPath := filepath.Join(s.cfg.UploadDir, safeName)
	if err := os.WriteFile(diskPath, prepared.Buffer, 0o600); err != nil {
		writeErr(w, http.StatusInternalServerError, "No se pudo guardar el archivo")
		return
	}
	name := filename
	if name == "" {
		name = safeName
	}
	att := &AutomationAttachment{
		Path: diskPath, Mime: prepared.MimeType, Kind: prepared.Kind, Encoding: prepared.StorageEncoding,
		Name: name, Size: prepared.StoredSize, SHA256: sha256OfBytes(buf), DurationMs: prepared.DurationMs,
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "attachment": att})
}

// getAutomationMedia sirve el media guardado de una automatización (vista previa
// en el panel al editar). Descomprime gzip si aplica.
func (s *Service) getAutomationMedia(w http.ResponseWriter, r *http.Request) {
	id, _ := strconv.ParseInt(chiParam(r, "id"), 10, 64)
	att, _ := s.db.getAutomationAttachment(r.Context(), id)
	if att == nil {
		writeErr(w, http.StatusNotFound, "Sin media")
		return
	}
	storagePath, _ := filepath.Abs(att.Path)
	if !s.validAutomationMediaPath(storagePath) {
		writeErr(w, http.StatusForbidden, "Ruta inválida")
		return
	}
	f, err := os.Open(storagePath)
	if err != nil {
		writeErr(w, http.StatusNotFound, "Archivo no encontrado")
		return
	}
	defer f.Close()
	w.Header().Set("Content-Type", tipoSeguro(att.Mime))
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Content-Security-Policy", "default-src 'none'; sandbox; frame-ancestors 'none'")
	w.Header().Set("Cache-Control", "private, no-store")
	if att.Encoding == "gzip" {
		gz, err := gzipReader(f)
		if err != nil {
			writeErr(w, http.StatusInternalServerError, "Error al leer archivo")
			return
		}
		defer gz.Close()
		_, _ = io.Copy(w, io.LimitReader(gz, maxBytesForMime(att.Mime)+64))
		return
	}
	_, _ = io.Copy(w, f)
}

// createAutomationAttachmentMessage crea el mensaje (image/video) + su adjunto a
// partir del media ya guardado de la automatización. Reusa el archivo en disco
// (no re-valida). Devuelve el mensaje listo para broadcast.
func (s *Service) createAutomationAttachmentMessage(ctx context.Context, conversationID, senderID int64, att *AutomationAttachment) *Message {
	if att == nil {
		return nil
	}
	kind := att.Kind
	if kind != "image" && kind != "video" {
		kind = "image"
	}
	nonce := "auto-media-" + strconv.FormatInt(conversationID, 10) + "-" + newUUID()
	created, err := s.db.createMessage(ctx, conversationID, senderID, kind,
		Encrypted{Ciphertext: "", IV: "automation", Tag: ""}, nonce, nil, nil, nil)
	if err != nil {
		return nil
	}
	if err := s.db.createAttachment(ctx, created.ID, senderID, fileMeta{
		Path: att.Path, OriginalName: att.Name, MimeType: att.Mime, Size: att.Size,
		SHA256: att.SHA256, DurationMs: att.DurationMs, StorageEncoding: att.Encoding,
	}); err != nil {
		return nil
	}
	before := created.ID - 1
	msgs, _ := s.db.listMessages(ctx, conversationID, &before, nil, 1)
	if len(msgs) == 0 {
		return nil
	}
	return &msgs[0]
}
