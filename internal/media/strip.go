package media

import (
	"bytes"
	"encoding/binary"
)

// StripMetadata elimina los metadatos de un archivo de media según su tipo.
//
// Devuelve el buffer saneado y si hubo cambios. Si el formato no se sabe tratar,
// devuelve el original con cambiado=false: nunca corrompe un archivo por
// intentar limpiarlo.
//
// Las imágenes NO se tratan aquí: para ellas la limpieza correcta es
// re-codificar los píxeles desde cero (ver internal/chat/image.go), que además
// destruye cualquier payload embebido. Aquí están los formatos que no se pueden
// re-codificar sin perder calidad o sin una librería pesada.
func StripMetadata(b []byte, mime string) (out []byte, cambiado bool) {
	switch mime {
	case "image/gif":
		return stripGIF(b)
	case "video/mp4", "video/quicktime", "audio/mp4":
		return stripISOBMFF(b)
	case "audio/mpeg":
		return stripID3(b)
	}
	return b, false
}

// --- GIF -------------------------------------------------------------------

// stripGIF elimina los bloques de extensión de Comentario (0x21 0xFE) y de
// Aplicación (0x21 0xFF), que es donde viven los comentarios, las marcas de la
// herramienta que lo generó y —lo que importa— los datos arbitrarios que se
// esconden ahí para hacer archivos "polyglot": un GIF válido que a la vez
// contiene otro formato dentro.
//
// El GIF es un formato de bloques secuenciales sin tabla de posiciones, así que
// quitar bloques del medio es seguro: nada dentro del archivo apunta a un
// desplazamiento absoluto. Se conserva la extensión de Control Gráfico (0xF9),
// que lleva el tiempo entre fotogramas y la transparencia — sin ella las
// animaciones se romperían.
func stripGIF(b []byte) ([]byte, bool) {
	if len(b) < 13 || (string(b[0:6]) != "GIF87a" && string(b[0:6]) != "GIF89a") {
		return b, false
	}
	out := make([]byte, 0, len(b))
	p := 0

	// Cabecera (13 bytes) + tabla global de color si la hay.
	out = append(out, b[:13]...)
	flags := b[10]
	p = 13
	if flags&0x80 != 0 {
		n := 3 * (1 << ((flags & 0x07) + 1))
		if p+n > len(b) {
			return b, false
		}
		out = append(out, b[p:p+n]...)
		p += n
	}

	cambiado := false
	for p < len(b) {
		switch b[p] {
		case 0x3B: // terminador del archivo
			out = append(out, 0x3B)
			return out, cambiado

		case 0x21: // bloque de extensión
			if p+2 > len(b) {
				return b, false
			}
			etiqueta := b[p+1]
			fin, ok := finDeSubBloques(b, p+2)
			if !ok {
				return b, false
			}
			// 0xFE = Comentario, 0xFF = Aplicación: fuera. El resto se conserva.
			if etiqueta == 0xFE || etiqueta == 0xFF {
				cambiado = true
			} else {
				out = append(out, b[p:fin]...)
			}
			p = fin

		case 0x2C: // descriptor de imagen (un fotograma)
			if p+10 > len(b) {
				return b, false
			}
			out = append(out, b[p:p+10]...)
			lf := b[p+9]
			p += 10
			if lf&0x80 != 0 { // tabla local de color
				n := 3 * (1 << ((lf & 0x07) + 1))
				if p+n > len(b) {
					return b, false
				}
				out = append(out, b[p:p+n]...)
				p += n
			}
			if p >= len(b) {
				return b, false
			}
			out = append(out, b[p]) // tamaño mínimo de código LZW
			p++
			fin, ok := finDeSubBloques(b, p)
			if !ok {
				return b, false
			}
			out = append(out, b[p:fin]...)
			p = fin

		default:
			// Byte inesperado: el archivo no sigue la estructura que esperamos.
			// Se devuelve intacto en vez de arriesgarse a corromperlo.
			return b, false
		}
	}
	return out, cambiado
}

// finDeSubBloques recorre una cadena de sub-bloques GIF ([longitud][datos]…)
// hasta el bloque de longitud 0 y devuelve la posición siguiente.
func finDeSubBloques(b []byte, p int) (int, bool) {
	for p < len(b) {
		n := int(b[p])
		if n == 0 {
			return p + 1, true
		}
		p += 1 + n
	}
	return 0, false
}

// --- MP4 / MOV / M4A (ISO-BMFF) --------------------------------------------

// contenedores son las cajas ISO-BMFF cuyo contenido son más cajas.
var contenedores = map[string]bool{
	"moov": true, "trak": true, "mdia": true, "minf": true,
	"stbl": true, "edts": true, "moof": true, "traf": true,
}

// stripISOBMFF neutraliza las cajas de metadatos de un MP4/MOV/M4A.
//
// Aquí está el dato que de verdad importa: los vídeos grabados con un móvil
// llevan las COORDENADAS GPS del sitio donde se grabaron, dentro de la caja
// `udta` (en el campo ©xyz), junto al modelo del teléfono y el software.
// Publicar ese vídeo tal cual publica la ubicación de quien lo grabó.
//
// La técnica: en lugar de BORRAR la caja —que desplazaría todos los bytes
// posteriores y rompería las tablas de posición de fragmentos (`stco`/`co64`),
// dejando el vídeo sin reproducir— se la RENOMBRA a `free` y se rellena su
// contenido con ceros. `free` es la caja de relleno del propio estándar: los
// reproductores la saltan. El archivo mantiene exactamente el mismo tamaño y
// todas las posiciones internas siguen siendo válidas.
func stripISOBMFF(b []byte) ([]byte, bool) {
	if len(b) < 8 {
		return b, false
	}
	out := append([]byte(nil), b...)
	cambiado := neutralizarCajas(out, 0, len(out), 0)
	if !cambiado {
		return b, false
	}
	return out, true
}

// cajasDeMetadatos son las que se neutralizan.
//
//	udta: "user data" — GPS, modelo del dispositivo, software, título…
//	meta: metadatos estilo iTunes (artista, álbum, comentarios)
//	uuid: cajas de extensión, el sitio habitual del XMP de Adobe
var cajasDeMetadatos = map[string]bool{"udta": true, "meta": true, "uuid": true}

// neutralizarCajas recorre el árbol de cajas entre [ini,fin) y sobrescribe las
// de metadatos. profundidad acota la recursión ante un archivo manipulado.
func neutralizarCajas(b []byte, ini, fin, profundidad int) bool {
	if profundidad > 8 {
		return false
	}
	cambiado := false
	p := ini
	for p+8 <= fin {
		tam := int(binary.BigEndian.Uint32(b[p : p+4]))
		tipo := string(b[p+4 : p+8])
		cabecera := 8

		switch {
		case tam == 0:
			// tamaño 0 = "hasta el final del archivo"
			tam = fin - p
		case tam == 1:
			// tamaño 1 = tamaño real de 64 bits en los 8 bytes siguientes
			if p+16 > fin {
				return cambiado
			}
			t64 := binary.BigEndian.Uint64(b[p+8 : p+16])
			if t64 > uint64(fin-p) {
				return cambiado
			}
			tam = int(t64)
			cabecera = 16
		}
		if tam < cabecera || p+tam > fin {
			return cambiado // estructura inconsistente: no se toca más
		}

		if cajasDeMetadatos[tipo] {
			copy(b[p+4:p+8], []byte("free")) // los reproductores la ignoran
			for i := p + cabecera; i < p+tam; i++ {
				b[i] = 0
			}
			cambiado = true
		} else if contenedores[tipo] {
			if neutralizarCajas(b, p+cabecera, p+tam, profundidad+1) {
				cambiado = true
			}
		}
		p += tam
	}
	return cambiado
}

// --- MP3 (ID3) --------------------------------------------------------------

// stripID3 quita las etiquetas ID3v2 (al principio) e ID3v1 (los últimos 128
// bytes) de un MP3. Ahí van el artista, el álbum, la carátula y los comentarios
// —donde a veces se cuelan datos personales o cargas embebidas.
//
// A diferencia del MP4, en un MP3 sí se pueden ELIMINAR de verdad: los
// fotogramas MP3 se auto-sincronizan y no hay ninguna tabla interna que apunte
// a posiciones absolutas, así que desplazar el contenido no rompe nada.
func stripID3(b []byte) ([]byte, bool) {
	ini, fin := 0, len(b)
	cambiado := false

	// ID3v2: "ID3" + versión(2) + banderas(1) + tamaño(4, syncsafe de 7 bits).
	for fin-ini >= 10 && string(b[ini:ini+3]) == "ID3" {
		tam := int(b[ini+6]&0x7f)<<21 | int(b[ini+7]&0x7f)<<14 |
			int(b[ini+8]&0x7f)<<7 | int(b[ini+9]&0x7f)
		salto := 10 + tam
		if b[ini+5]&0x10 != 0 {
			salto += 10 // pie de página presente
		}
		if salto <= 0 || ini+salto > fin {
			break
		}
		ini += salto
		cambiado = true
	}

	// ID3v1: los últimos 128 bytes empiezan por "TAG".
	if fin-ini >= 128 && string(b[fin-128:fin-125]) == "TAG" {
		fin -= 128
		cambiado = true
	}

	if !cambiado {
		return b, false
	}
	if ini >= fin {
		return b, false // solo eran etiquetas: se deja como estaba
	}
	return append([]byte(nil), b[ini:fin]...), true
}

// TieneMetadatos indica si el archivo lleva metadatos que StripMetadata quitaría.
// Sirve para registrar en el log cuántos archivos venían con datos dentro.
func TieneMetadatos(b []byte, mime string) bool {
	_, cambiado := StripMetadata(b, mime)
	return cambiado
}

var _ = bytes.Equal // usado por los tests
