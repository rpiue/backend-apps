package media

import (
	"bytes"
	"errors"
	"image"
	"image/jpeg"
	"image/png"

	_ "image/gif"  // registra el decodificador GIF
	_ "image/jpeg" // registra el decodificador JPEG
	_ "image/png"  // registra el decodificador PNG

	xdraw "golang.org/x/image/draw"
	_ "golang.org/x/image/webp" // decodifica WEBP (se re-codifica a PNG/JPEG)
)

const (
	// MaxDimension es el lado máximo tras el reescalado.
	MaxDimension = 1600
	// CalidadJPEG del re-encodeado.
	CalidadJPEG = 85
	// MaxPixeles frena las "bombas de descompresión": archivos diminutos en
	// disco que al decodificarse ocupan cientos de megapíxeles en memoria.
	// Se comprueba leyendo SOLO la cabecera, antes de decodificar nada.
	MaxPixeles = 50 * 1000 * 1000
)

// ErrImagenInvalida indica que el contenido no se pudo decodificar como imagen.
var ErrImagenInvalida = errors.New("media: la imagen no se pudo decodificar")

// ErrDemasiadosPixeles indica una posible bomba de descompresión.
var ErrDemasiadosPixeles = errors.New("media: la imagen tiene demasiados píxeles")

// DimensionesOK comprueba el número de píxeles leyendo solo la cabecera.
func DimensionesOK(b []byte) bool {
	cfg, _, err := image.DecodeConfig(bytes.NewReader(b))
	if err != nil {
		return true // no es una imagen conocida; lo filtra Sniff
	}
	if cfg.Width <= 0 || cfg.Height <= 0 {
		return false
	}
	return int64(cfg.Width)*int64(cfg.Height) <= MaxPixeles
}

// SanitizeImage re-codifica la imagen desde sus píxeles.
//
// Es la forma más completa de limpiar una imagen, y por eso se prefiere a
// intentar recortar campos concretos: al decodificar a píxeles y volver a
// codificar desde cero, lo único que sobrevive es la imagen en sí. Desaparecen
// de golpe el EXIF (con las coordenadas GPS, el modelo del móvil y la fecha),
// el XMP, los perfiles de color, los comentarios, cualquier dato escondido
// después del marcador de fin y las estructuras "polyglot" que hacen que un
// mismo archivo sea a la vez una imagen válida y otra cosa ejecutable.
//
// Los GIF no se re-codifican (se perdería la animación): para ellos se usa
// StripMetadata, que quita los bloques de comentario y de aplicación.
func SanitizeImage(b []byte, mime string) (out []byte, outMime string, err error) {
	if mime == "image/gif" {
		limpio, _ := StripMetadata(b, mime)
		return limpio, mime, nil
	}
	if !DimensionesOK(b) {
		return nil, "", ErrDemasiadosPixeles
	}
	src, _, err := image.Decode(bytes.NewReader(b))
	if err != nil {
		return nil, "", ErrImagenInvalida
	}
	bounds := src.Bounds()
	w, h := bounds.Dx(), bounds.Dy()
	if w == 0 || h == 0 {
		return nil, "", ErrImagenInvalida
	}

	nw, nh := AjustarDentro(w, h, MaxDimension)
	dst := src
	if nw != w || nh != h {
		rgba := image.NewRGBA(image.Rect(0, 0, nw, nh))
		xdraw.CatmullRom.Scale(rgba, rgba.Bounds(), src, bounds, xdraw.Over, nil)
		dst = rgba
	}

	var buf bytes.Buffer
	// PNG y WEBP pueden llevar transparencia: se re-codifican a PNG para no
	// perderla. Las JPEG vuelven a JPEG.
	if mime == "image/jpeg" {
		if err := jpeg.Encode(&buf, dst, &jpeg.Options{Quality: CalidadJPEG}); err != nil {
			return nil, "", err
		}
		return buf.Bytes(), "image/jpeg", nil
	}
	enc := png.Encoder{CompressionLevel: png.BestCompression}
	if err := enc.Encode(&buf, dst); err != nil {
		return nil, "", err
	}
	return buf.Bytes(), "image/png", nil
}

// AjustarDentro calcula el tamaño que cabe en max×max conservando la proporción,
// sin agrandar nunca la imagen original.
func AjustarDentro(w, h, max int) (int, int) {
	if w <= max && h <= max {
		return w, h
	}
	if w >= h {
		nh := h * max / w
		if nh < 1 {
			nh = 1
		}
		return max, nh
	}
	nw := w * max / h
	if nw < 1 {
		nw = 1
	}
	return nw, max
}
