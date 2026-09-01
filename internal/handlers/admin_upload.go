package handlers

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"codex/backend/internal/media"
	"codex/backend/internal/secnet"
)

const uploadDir = "uploads/imgUser"

// maxAdminUploadBytes acota lo que se acepta por subida. Antes no había ningún
// límite: un io.Copy sin techo permite llenar el disco del VPS con una sola
// petición, y cuando el disco se llena Postgres deja de escribir y el servicio
// entero cae.
const maxAdminUploadBytes = 10 << 20 // 10 MB

// extensionPorMime fija la extensión a partir del tipo REAL detectado, no del
// nombre que mandó el cliente. Así el archivo en disco nunca acaba llamándose
// como algo que no es.
var extensionPorMime = map[string]string{
	"image/jpeg": ".jpg",
	"image/png":  ".png",
	"image/webp": ".webp",
	"image/gif":  ".gif",
}

// adminUpload recibe una imagen (campo "file") o una URL (campo "url"), la
// valida, le quita los metadatos y la guarda en uploads/imgUser.
//
// Estas imágenes se publican como banners y anuncios de la app, así que son el
// caso más delicado: van a un directorio servido públicamente y las ve todo el
// mundo. Antes se aceptaban comprobando ÚNICAMENTE la extensión del nombre del
// archivo, y se guardaban tal cual. Eso significaba dos cosas:
//
//   - Cualquier contenido con el nombre acabado en .png entraba, fuese lo que
//     fuese por dentro, y quedaba servido desde un dominio de confianza.
//   - Las fotos conservaban su EXIF, así que un banner hecho con una foto de
//     móvil publicaba las coordenadas GPS de dónde se tomó.
//
// Ahora el tipo se decide por los bytes y la imagen se re-codifica desde sus
// píxeles, con lo que sale limpia por construcción.
func (h *Handler) adminUpload(w http.ResponseWriter, r *http.Request) {
	if err := os.MkdirAll(uploadDir, 0o750); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "no se pudo preparar el directorio"})
		return
	}

	// Techo al cuerpo entero de la petición antes de leer nada de él.
	r.Body = http.MaxBytesReader(w, r.Body, maxAdminUploadBytes+(1<<20))

	// Caso 1: archivo subido.
	if file, header, err := r.FormFile("file"); err == nil {
		defer file.Close()
		raw, err := leerConTope(file, maxAdminUploadBytes)
		if err != nil {
			writeJSON(w, http.StatusRequestEntityTooLarge, map[string]string{
				"error": fmt.Sprintf("La imagen supera el máximo de %d MB.", maxAdminUploadBytes>>20)})
			return
		}
		guardarImagenSaneada(w, h, raw, header.Filename)
		return
	}

	// Caso 2: descarga desde una URL.
	if crudo := strings.TrimSpace(r.FormValue("url")); crudo != "" {
		raw, err := descargarImagen(r.Context(), crudo)
		if err != nil {
			log.Printf("[admin] descarga de imagen rechazada (%s): %v", crudo, err)
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": mensajeDescarga(err)})
			return
		}
		guardarImagenSaneada(w, h, raw, "")
		return
	}

	writeJSON(w, http.StatusBadRequest, map[string]string{"error": "No se recibió imagen ni URL."})
}

// guardarImagenSaneada valida el contenido REAL, lo limpia y lo escribe a disco.
func guardarImagenSaneada(w http.ResponseWriter, h *Handler, raw []byte, nombreOriginal string) {
	if len(raw) == 0 {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "El archivo está vacío."})
		return
	}

	// 1) ¿Es algo que directamente no debería estar aquí? Se dice qué era.
	if p := media.DetectarPeligro(raw); p != nil {
		log.Printf("[admin] subida rechazada: %q es en realidad %s (%s)", nombreOriginal, p.Motivo, p.Tipo)
		writeJSON(w, http.StatusUnsupportedMediaType, map[string]string{
			"error": "El archivo no es una imagen: parece " + p.Motivo + "."})
		return
	}

	// 2) El tipo lo deciden los bytes, no la extensión ni el Content-Type.
	mime := media.Sniff(raw)
	if _, ok := extensionPorMime[mime]; !ok {
		log.Printf("[admin] subida rechazada: contenido no reconocido como imagen (nombre %q)", nombreOriginal)
		writeJSON(w, http.StatusUnsupportedMediaType, map[string]string{
			"error": "Solo se permiten imágenes JPG, PNG, WEBP o GIF."})
		return
	}

	// 3) Re-codificar desde los píxeles: aquí desaparecen el EXIF con el GPS,
	//    el XMP, los comentarios y cualquier carga escondida en el archivo.
	limpio, mimeFinal, err := media.SanitizeImage(raw, mime)
	if err != nil {
		estado := http.StatusUnsupportedMediaType
		msg := "La imagen está dañada o no se pudo procesar."
		if errors.Is(err, media.ErrDemasiadosPixeles) {
			estado = http.StatusRequestEntityTooLarge
			msg = "La imagen tiene demasiados píxeles."
		}
		log.Printf("[admin] saneado de imagen fallido (%s): %v", mime, err)
		writeJSON(w, estado, map[string]string{"error": msg})
		return
	}

	// 4) Nombre imprevisible y con la extensión del tipo REAL. El nombre que
	//    mandó el cliente no se usa para nada en disco.
	nombre := nombreUnico() + extensionPorMime[mimeFinal]
	destino := filepath.Join(uploadDir, nombre)

	// Permisos 0640 y sin bit de ejecución: aunque alguien lograra colocar un
	// binario aquí, el sistema de archivos no lo dejaría ejecutar.
	if err := os.WriteFile(destino, limpio, 0o640); err != nil {
		log.Printf("[admin] no se pudo guardar la imagen: %v", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "Error interno del servidor."})
		return
	}

	log.Printf("[admin] imagen guardada %s (%s, %d KB, metadatos eliminados)", nombre, mimeFinal, len(limpio)>>10)
	writeJSON(w, http.StatusOK, map[string]any{"success": true, "imageUrl": h.publicURL(nombre)})
}

// descargarImagen trae una URL externa con protección contra SSRF.
//
// Este endpoint hace que el SERVIDOR abra una conexión a una dirección que
// escribe otra persona. Sin restricciones, eso convierte al servidor en un
// puente hacia todo lo que él alcanza y quien llama no: el Postgres y el Redis
// de la red interna de Docker, o el servicio de metadatos de la nube en
// 169.254.169.254, que entrega credenciales de la máquina a quien pregunte.
// ClientePublico solo deja salir a direcciones públicas de internet, y lo
// comprueba al abrir el socket (no al leer la URL), que es lo que cierra el
// truco del DNS rebinding.
func descargarImagen(ctx context.Context, crudo string) ([]byte, error) {
	u, err := url.Parse(crudo)
	if err != nil {
		return nil, fmt.Errorf("URL inválida: %w", err)
	}
	if u.Scheme != "https" && u.Scheme != "http" {
		return nil, fmt.Errorf("esquema no permitido: %q", u.Scheme)
	}
	if u.Scheme == "http" {
		// Descargar por HTTP significa aceptar lo que devuelva quien esté en la
		// ruta, no necesariamente el servidor real.
		return nil, errors.New("solo se permiten URLs https")
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "image/*")

	resp, err := secnet.ClientePublico(20 * time.Second).Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		return nil, fmt.Errorf("el servidor respondió %d", resp.StatusCode)
	}
	return leerConTope(resp.Body, maxAdminUploadBytes)
}

// mensajeDescarga traduce el error a algo que se le puede enseñar al operador
// sin revelar la topología de la red interna.
func mensajeDescarga(err error) string {
	if errors.Is(err, secnet.ErrDestinoInterno) {
		return "Esa URL apunta a una dirección interna y no se permite."
	}
	return "No se pudo descargar la imagen."
}

// leerConTope lee como mucho max bytes y falla si hay más, en vez de truncar en
// silencio: un archivo cortado a la mitad no es una imagen válida y es mejor
// decirlo que guardar algo roto.
func leerConTope(r io.Reader, max int64) ([]byte, error) {
	b, err := io.ReadAll(io.LimitReader(r, max+1))
	if err != nil {
		return nil, err
	}
	if int64(len(b)) > max {
		return nil, errors.New("el contenido supera el tamaño máximo")
	}
	return b, nil
}

func (h *Handler) publicURL(name string) string {
	return strings.TrimRight(h.Cfg.Dominio, "/") + "/uploads/imgUser/" + name
}

// nombreUnico genera el nombre del archivo con el generador CRIPTOGRÁFICO, no
// con math/rand. Con un nombre predecible, alguien puede adivinar la URL de una
// imagen antes de que se publique, o pisar la de otro.
func nombreUnico() string {
	var b [12]byte
	if _, err := rand.Read(b[:]); err != nil {
		return fmt.Sprintf("%d", time.Now().UnixNano())
	}
	return fmt.Sprintf("%d-%s", time.Now().UnixMilli(), hex.EncodeToString(b[:]))
}
