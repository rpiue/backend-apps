package chat

import (
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"log"
	"strings"

	"codex/backend/internal/media"
)

// Límites (mismos defaults que index.js; configurables por env en config).
const (
	maxImageBytes  = 5 * 1024 * 1024
	maxAudioBytes  = 15 * 1024 * 1024
	maxVideoBytes  = 60 * 1024 * 1024
	maxFileBytes   = 60 * 1024 * 1024
	maxVideoDurMs  = 5 * 60 * 1000
	userQuotaBytes = 250 * 1024 * 1024
)

var allowedMimes = map[string]bool{
	"image/jpeg": true, "image/png": true, "image/webp": true, "image/gif": true,
	"audio/mpeg": true, "audio/mp4": true, "audio/ogg": true, "audio/webm": true,
	"video/mp4": true, "video/webm": true, "video/quicktime": true,
}

// detectMimeFromBuffer detecta el tipo real por los bytes de cabecera.
// Delega en media.Sniff para que el chat, el panel de admin y las
// automatizaciones compartan EXACTAMENTE el mismo criterio: tener dos
// detectores es tener dos comportamientos que se separan con el tiempo, y basta
// con que uno acepte algo que el otro rechaza para abrir un hueco.
func detectMimeFromBuffer(b []byte) string { return media.Sniff(b) }

func mediaKindForMime(mime string) string {
	switch {
	case strings.HasPrefix(mime, "image/"):
		return "image"
	case strings.HasPrefix(mime, "audio/"):
		return "audio"
	case strings.HasPrefix(mime, "video/"):
		return "video"
	}
	return ""
}

func isCompatibleMime(declared, detected string) bool {
	if !allowedMimes[declared] || !allowedMimes[detected] {
		return false
	}
	if declared == detected {
		return true
	}
	dk := mediaKindForMime(declared)
	ek := mediaKindForMime(detected)
	if dk == "" || dk != ek {
		// excepción: ftyp detectado como video/mp4 puede declararse audio/mp4 o video/quicktime
		if detected == "video/mp4" && (declared == "audio/mp4" || declared == "video/quicktime") {
			return true
		}
		return false
	}
	if dk == "video" && detected == "video/mp4" {
		return true
	}
	if dk == "audio" && detected == "video/mp4" {
		return true
	}
	return false
}

func maxBytesForMime(mime string) int64 {
	switch mediaKindForMime(mime) {
	case "image":
		return maxImageBytes
	case "audio":
		return maxAudioBytes
	case "video":
		return maxVideoBytes
	}
	return maxFileBytes
}

// parseMp4DurationMs replica parseMp4DurationMs (lee mvhd).
func parseMp4DurationMs(buf []byte) *int {
	offset := 0
	for offset+8 <= len(buf) {
		size := int(binary.BigEndian.Uint32(buf[offset : offset+4]))
		typ := string(buf[offset+4 : offset+8])
		if size < 8 {
			break
		}
		if typ == "mvhd" && offset+28 <= len(buf) {
			version := buf[offset+8]
			if version == 1 && offset+32 <= len(buf) {
				timescale := binary.BigEndian.Uint32(buf[offset+28 : offset+32])
				if offset+40 > len(buf) {
					return nil
				}
				duration := binary.BigEndian.Uint64(buf[offset+32 : offset+40])
				if timescale == 0 {
					return nil
				}
				v := int(float64(duration) / float64(timescale) * 1000)
				return &v
			}
			if offset+28 > len(buf) {
				return nil
			}
			timescale := binary.BigEndian.Uint32(buf[offset+20 : offset+24])
			duration := binary.BigEndian.Uint32(buf[offset+24 : offset+28])
			if timescale == 0 {
				return nil
			}
			v := int(float64(duration) / float64(timescale) * 1000)
			return &v
		}
		end := offset + size
		if typ == "moov" || typ == "trak" || typ == "mdia" {
			offset += 8
			continue
		}
		offset = end
	}
	return nil
}

// preparedUpload es el resultado de validar/comprimir un archivo (validateAndPrepareUpload).
type preparedUpload struct {
	Buffer          []byte
	StorageEncoding string
	MimeType        string
	Kind            string
	DurationMs      *int
	OriginalSize    int64
	StoredSize      int64
}

// chatError es un error con statusCode HTTP asociado.
type chatErr struct {
	msg    string
	status int
}

func (e chatErr) Error() string { return e.msg }
func newChatErr(msg string, status int) chatErr {
	return chatErr{msg: msg, status: status}
}

// validateAndPrepareUpload replica validateAndPrepareUpload (sin sharp: no re-codifica a webp).
func validateAndPrepareUpload(buf []byte, declaredMime string, clientDurationMs *int) (*preparedUpload, error) {
	if len(buf) == 0 {
		return nil, newChatErr("Archivo no permitido", 400)
	}
	// Primero se mira si el contenido es algo que NO debería estar aquí. Un
	// ejecutable renombrado a .png pasa cualquier filtro por extensión, así que
	// se reconoce por sus bytes y se rechaza diciendo qué era en realidad —
	// tanto para que el usuario entienda el error como para poder registrarlo.
	if p := media.DetectarPeligro(buf); p != nil {
		log.Printf("[chat] subida rechazada: el archivo declarado como %q es en realidad %s (%s)",
			declaredMime, p.Motivo, p.Tipo)
		return nil, newChatErr("El archivo no es una imagen, audio ni video: parece "+p.Motivo, 415)
	}

	detected := detectMimeFromBuffer(buf)
	if detected == "" || !isCompatibleMime(declaredMime, detected) {
		return nil, newChatErr("Tipo de archivo no permitido o contenido invalido", 415)
	}
	mime := detected
	if detected == "video/mp4" && (declaredMime == "audio/mp4" || declaredMime == "video/quicktime") {
		mime = declaredMime
	}
	size := int64(len(buf))
	if size > maxBytesForMime(mime) {
		return nil, newChatErr("Archivo demasiado grande para este tipo de medio", 413)
	}
	kind := mediaKindForMime(mime)
	var durationMs *int
	if kind == "video" {
		durationMs = parseMp4DurationMs(buf)
		if durationMs == nil {
			durationMs = clientDurationMs
		}
	} else {
		durationMs = clientDurationMs
	}
	if kind == "video" && durationMs != nil && *durationMs > maxVideoDurMs {
		return nil, newChatErr("El video no puede durar mas de 5 minutos", 413)
	}

	// Saneamiento de imágenes: SIEMPRE re-encodeamos (excepto GIF) para eliminar
	// metadatos, payloads embebidos y estructuras polyglot; además se acota el
	// número de píxeles para frenar bombas de descompresión. El archivo se
	// almacena ya "limpio". (Los GIF se dejan igual; ClamAV los cubre en fase 2.)
	sourceBuffer := buf
	storedMime := mime
	if kind == "image" {
		if !imageDimensionsOK(buf) {
			return nil, newChatErr("La imagen excede el tamaño máximo en píxeles", 413)
		}
		if mime != "image/gif" {
			san := sanitizeImageBuffer(buf, mime)
			if san == nil {
				// Pasó el magic-byte pero no se pudo decodificar/re-encodear:
				// contenido sospechoso o corrupto → se rechaza.
				return nil, newChatErr("Imagen inválida o dañada", 415)
			}
			sourceBuffer = san.buffer
			storedMime = san.mimeType
		}
	}

	// Metadatos de los formatos que NO se re-codifican: GIF (comentarios y
	// bloques de aplicación, el escondite habitual de los polyglot), MP4/MOV
	// (la caja udta, donde el móvil deja las COORDENADAS GPS de la grabación) y
	// MP3 (etiquetas ID3). Sin esto, reenviar un vídeo publica dónde se grabó.
	if limpio, cambiado := media.StripMetadata(sourceBuffer, storedMime); cambiado {
		log.Printf("[chat] metadatos eliminados de un adjunto %s (%d bytes)", storedMime, len(sourceBuffer))
		sourceBuffer = limpio
	}

	// Compresión gzip si reduce el tamaño (storage_encoding gzip|identity).
	var gz bytes.Buffer
	w, _ := gzip.NewWriterLevel(&gz, gzip.BestSpeed)
	_, _ = w.Write(sourceBuffer)
	_ = w.Close()
	compressed := gz.Bytes()
	storeCompressed := int64(len(compressed))+64 < int64(len(sourceBuffer))

	out := &preparedUpload{
		MimeType: storedMime, Kind: kind, DurationMs: durationMs, OriginalSize: size,
	}
	if storeCompressed {
		out.Buffer = compressed
		out.StorageEncoding = "gzip"
		out.StoredSize = int64(len(compressed))
	} else {
		out.Buffer = sourceBuffer
		out.StorageEncoding = "identity"
		out.StoredSize = int64(len(sourceBuffer))
	}
	return out, nil
}

func sha256OfBytes(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

func safeDownloadName(name string) string {
	var b strings.Builder
	for _, r := range name {
		switch {
		case r == '\r' || r == '\n' || r == '"':
			// drop
		case (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') ||
			r == '.' || r == '_' || r == ' ' || r == '-':
			b.WriteRune(r)
		default:
			b.WriteRune('_')
		}
	}
	out := b.String()
	if len(out) > 120 {
		out = out[:120]
	}
	if out == "" {
		return "archivo"
	}
	return out
}

// tiposDeSalida son los únicos Content-Type con los que se entrega un adjunto.
var tiposDeSalida = map[string]bool{
	"image/jpeg": true, "image/png": true, "image/webp": true, "image/gif": true,
	"audio/mpeg": true, "audio/mp4": true, "audio/ogg": true, "audio/webm": true,
	"video/mp4": true, "video/webm": true, "video/quicktime": true,
}

// tipoSeguro decide con qué Content-Type sale un adjunto.
//
// Existe por los archivos que YA están en el servidor. La validación estricta de
// contenido es reciente; todo lo subido antes se guardó con el tipo que declaró
// el cliente, sin comprobarlo. Un adjunto antiguo puede tener registrado
// cualquier cosa en `mime_type`, y devolverlo tal cual sería dejar que un
// archivo de hace meses decida cómo lo interpreta el navegador de quien lo abra.
//
// Cualquier tipo que no esté en la lista sale como application/octet-stream: un
// flujo de bytes que el navegador no intenta interpretar ni ejecutar, solo
// guardar. El archivo se sigue entregando —no se pierde nada— pero deja de ser
// algo que el navegador ejecute.
func tipoSeguro(mime string) string {
	m := strings.ToLower(strings.TrimSpace(mime))
	if i := strings.Index(m, ";"); i >= 0 {
		m = strings.TrimSpace(m[:i]) // descarta parámetros tipo "; charset=..."
	}
	if tiposDeSalida[m] {
		return m
	}
	return "application/octet-stream"
}
