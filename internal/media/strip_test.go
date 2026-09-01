package media

import (
	"bytes"
	"encoding/binary"
	"image"
	"image/color"
	"image/gif"
	"testing"
)

// --- GIF --------------------------------------------------------------------

// gifConComentario construye un GIF animado real y le inserta un bloque de
// comentario con un secreto dentro, que es lo que debe desaparecer.
func gifConComentario(t *testing.T, secreto string) []byte {
	t.Helper()
	m := image.NewPaletted(image.Rect(0, 0, 4, 4), color.Palette{color.Black, color.White})
	m.SetColorIndex(1, 1, 1)
	var base bytes.Buffer
	if err := gif.EncodeAll(&base, &gif.GIF{Image: []*image.Paletted{m, m}, Delay: []int{10, 10}}); err != nil {
		t.Fatalf("no se pudo generar el GIF de prueba: %v", err)
	}
	b := base.Bytes()

	// Inserta la extensión de comentario justo después de la cabecera + paleta.
	corte := 13
	flags := b[10]
	if flags&0x80 != 0 {
		corte += 3 * (1 << ((flags & 0x07) + 1))
	}
	var com bytes.Buffer
	com.Write([]byte{0x21, 0xFE})
	com.WriteByte(byte(len(secreto)))
	com.WriteString(secreto)
	com.WriteByte(0x00)

	out := append([]byte{}, b[:corte]...)
	out = append(out, com.Bytes()...)
	return append(out, b[corte:]...)
}

func TestStripGIF_QuitaComentarioYSigueSiendoValido(t *testing.T) {
	const secreto = "COORDENADAS-SECRETAS-123"
	original := gifConComentario(t, secreto)

	if !bytes.Contains(original, []byte(secreto)) {
		t.Fatal("el GIF de prueba debería contener el secreto")
	}

	limpio, cambiado := StripMetadata(original, "image/gif")
	if !cambiado {
		t.Fatal("debía detectar y quitar el comentario")
	}
	if bytes.Contains(limpio, []byte(secreto)) {
		t.Fatal("el comentario sigue dentro del GIF saneado")
	}

	// Lo esencial: sigue siendo un GIF que se puede decodificar, con sus dos
	// fotogramas. Un saneador que corrompe el archivo no sirve de nada.
	g, err := gif.DecodeAll(bytes.NewReader(limpio))
	if err != nil {
		t.Fatalf("el GIF saneado ya no decodifica: %v", err)
	}
	if len(g.Image) != 2 {
		t.Fatalf("se perdieron fotogramas: quedan %d de 2", len(g.Image))
	}
}

func TestStripGIF_SinComentarioNoToca(t *testing.T) {
	m := image.NewPaletted(image.Rect(0, 0, 2, 2), color.Palette{color.Black, color.White})
	var buf bytes.Buffer
	if err := gif.Encode(&buf, m, nil); err != nil {
		t.Fatal(err)
	}
	if _, cambiado := StripMetadata(buf.Bytes(), "image/gif"); cambiado {
		t.Fatal("un GIF sin metadatos no debería marcarse como cambiado")
	}
}

// --- MP4 --------------------------------------------------------------------

func caja(tipo string, payload []byte) []byte {
	out := make([]byte, 8+len(payload))
	binary.BigEndian.PutUint32(out[0:4], uint32(8+len(payload)))
	copy(out[4:8], tipo)
	copy(out[8:], payload)
	return out
}

// mp4ConGPS arma un MP4 mínimo con la caja udta que llevaría un móvil: el campo
// ©xyz con las coordenadas de donde se grabó.
func mp4ConGPS() ([]byte, string) {
	const gps = "+40.4168-003.7038/"
	udta := caja("udta", caja("\xa9xyz", []byte(gps)))
	moov := caja("moov", append(caja("mvhd", make([]byte, 100)), udta...))
	ftyp := caja("ftyp", []byte("isom\x00\x00\x02\x00isomiso2"))
	mdat := caja("mdat", bytes.Repeat([]byte{0xAB}, 64))
	return append(append(ftyp, mdat...), moov...), gps
}

func TestStripMP4_QuitaGPSSinMoverBytes(t *testing.T) {
	original, gps := mp4ConGPS()
	if !bytes.Contains(original, []byte(gps)) {
		t.Fatal("el MP4 de prueba debería llevar las coordenadas")
	}

	limpio, cambiado := StripMetadata(original, "video/mp4")
	if !cambiado {
		t.Fatal("debía detectar y neutralizar la caja udta")
	}
	if bytes.Contains(limpio, []byte(gps)) {
		t.Fatal("las coordenadas GPS siguen en el vídeo saneado")
	}

	// Esto es lo crítico del enfoque: el tamaño NO cambia. Si cambiara, las
	// tablas de posición (stco/co64) apuntarían a sitios equivocados y el vídeo
	// no se reproduciría.
	if len(limpio) != len(original) {
		t.Fatalf("el tamaño cambió (%d → %d): eso rompería la reproducción", len(original), len(limpio))
	}
	// El contenido del vídeo (mdat) debe quedar intacto.
	if !bytes.Contains(limpio, bytes.Repeat([]byte{0xAB}, 64)) {
		t.Fatal("se dañaron los datos del vídeo (mdat)")
	}
	// La caja debe haberse renombrado a 'free', que los reproductores ignoran.
	if !bytes.Contains(limpio, []byte("free")) {
		t.Fatal("la caja udta no se renombró a free")
	}
	if bytes.Contains(limpio, []byte("udta")) {
		t.Fatal("la caja udta sigue presente")
	}
}

func TestStripMP4_NoRompeArchivoTruncado(t *testing.T) {
	original, _ := mp4ConGPS()
	// Un archivo cortado a la mitad no debe provocar pánico ni corrupción.
	for _, n := range []int{1, 7, 9, 20, len(original) / 2} {
		trozo := original[:n]
		out, _ := StripMetadata(trozo, "video/mp4")
		if len(out) != len(trozo) {
			t.Fatalf("con %d bytes cambió el tamaño", n)
		}
	}
}

// --- MP3 --------------------------------------------------------------------

func mp3ConID3(secreto string) []byte {
	payload := append([]byte("TIT2"), []byte(secreto)...)
	tam := len(payload)
	hdr := []byte{'I', 'D', '3', 3, 0, 0,
		byte(tam >> 21 & 0x7f), byte(tam >> 14 & 0x7f), byte(tam >> 7 & 0x7f), byte(tam & 0x7f)}
	// Fotograma MP3 mínimo (sincronía 0xFF 0xFB) + etiqueta ID3v1 al final.
	audio := append([]byte{0xFF, 0xFB, 0x90, 0x00}, bytes.Repeat([]byte{0x00}, 100)...)
	v1 := append([]byte("TAG"), bytes.Repeat([]byte{'x'}, 125)...)
	return append(append(append(hdr, payload...), audio...), v1...)
}

func TestStripID3_QuitaEtiquetasYConservaElAudio(t *testing.T) {
	const secreto = "NOMBRE-REAL-DEL-AUTOR"
	original := mp3ConID3(secreto)

	limpio, cambiado := StripMetadata(original, "audio/mpeg")
	if !cambiado {
		t.Fatal("debía quitar las etiquetas ID3")
	}
	if bytes.Contains(limpio, []byte(secreto)) {
		t.Fatal("la etiqueta ID3v2 sigue dentro")
	}
	if bytes.HasPrefix(limpio, []byte("ID3")) {
		t.Fatal("queda una cabecera ID3v2")
	}
	if bytes.Contains(limpio, []byte("TAG")) {
		t.Fatal("queda la etiqueta ID3v1 del final")
	}
	// El audio debe empezar exactamente en la sincronía del primer fotograma.
	if len(limpio) < 2 || limpio[0] != 0xFF || (limpio[1]&0xE0) != 0xE0 {
		t.Fatalf("el audio quedó desalineado: %x", limpio[:min(4, len(limpio))])
	}
}

func TestStripMetadata_FormatoDesconocidoSeDevuelveIntacto(t *testing.T) {
	original := []byte("contenido cualquiera que no sabemos tratar")
	out, cambiado := StripMetadata(original, "application/octet-stream")
	if cambiado {
		t.Fatal("no debería decir que cambió algo que no sabe tratar")
	}
	if !bytes.Equal(out, original) {
		t.Fatal("un formato no soportado debe devolverse intacto, nunca corrompido")
	}
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
