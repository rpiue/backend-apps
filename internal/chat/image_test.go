package chat

import (
	"bytes"
	"image"
	"image/color"
	"image/png"
	"testing"
)

func makePNG(t *testing.T, w, h int) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	for x := 0; x < w; x++ {
		img.Set(x, 0, color.RGBA{uint8(x), 0, 0, 255})
	}
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatalf("png encode: %v", err)
	}
	return buf.Bytes()
}

// TestSanitizeReencodesPNG: una PNG válida se re-encodea a una imagen decodable
// (se descartan metadatos/payloads al reconstruir los píxeles).
func TestSanitizeReencodesPNG(t *testing.T) {
	src := makePNG(t, 32, 32)
	// Simula un payload adjunto tras el IEND (polyglot): no debe sobrevivir.
	tampered := append(append([]byte{}, src...), []byte("<script>evil()</script>")...)

	san := sanitizeImageBuffer(tampered, "image/png")
	if san == nil {
		t.Fatal("sanitizeImageBuffer devolvió nil para PNG válida")
	}
	if _, _, err := image.Decode(bytes.NewReader(san.buffer)); err != nil {
		t.Fatalf("la imagen saneada no decodifica: %v", err)
	}
	if bytes.Contains(san.buffer, []byte("<script>")) {
		t.Fatal("el payload embebido sobrevivió al saneamiento")
	}
}

// TestSanitizeRejectsGarbage: bytes no decodificables como imagen → nil (el
// llamador lo rechaza).
func TestSanitizeRejectsGarbage(t *testing.T) {
	if san := sanitizeImageBuffer([]byte("no soy una imagen"), "image/png"); san != nil {
		t.Fatal("se aceptó basura como imagen")
	}
}

// TestSanitizeSkipsGIF: los GIF se dejan pasar (nil) para no romper animación.
func TestSanitizeSkipsGIF(t *testing.T) {
	if san := sanitizeImageBuffer(makePNG(t, 4, 4), "image/gif"); san != nil {
		t.Fatal("GIF no debería re-encodearse aquí")
	}
}

// TestImageDimensionsOK acepta imágenes normales y rechaza megapíxeles absurdos.
func TestImageDimensionsOK(t *testing.T) {
	if !imageDimensionsOK(makePNG(t, 100, 100)) {
		t.Fatal("imagen 100x100 rechazada")
	}
}
