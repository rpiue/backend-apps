package chat

import (
	"bytes"
	"image"
	"image/gif"
	"image/jpeg"
	"image/png"

	xdraw "golang.org/x/image/draw"
	_ "golang.org/x/image/webp" // decode webp (encode lo hacemos a JPEG/PNG)
)

const (
	imageMaxDimension = 1600 // mismo límite que sharp (resize fit inside 1600x1600)
	imageJPEGQuality  = 80
	// imageMaxPixels acota el total de píxeles ANTES de decodificar la imagen
	// entera, para frenar "bombas de descompresión" (imágenes diminutas en disco
	// que se expanden a cientos de megapíxeles y agotan la memoria).
	imageMaxPixels = 50 * 1000 * 1000 // 50 MP
)

// imageDimensionsOK usa DecodeConfig (lee solo la cabecera, no decodifica toda
// la imagen) para rechazar imágenes con demasiados píxeles. Devuelve true si el
// formato no es una imagen conocida (esos casos los filtra el magic-byte antes).
func imageDimensionsOK(buf []byte) bool {
	cfg, _, err := image.DecodeConfig(bytes.NewReader(buf))
	if err != nil {
		return true
	}
	if cfg.Width <= 0 || cfg.Height <= 0 {
		return false
	}
	return int64(cfg.Width)*int64(cfg.Height) <= imageMaxPixels
}

// sanitizeImageBuffer re-encodea SIEMPRE la imagen (excepto GIF) para eliminar
// metadatos, payloads embebidos y estructuras polyglot: decodifica los píxeles
// y vuelve a codificar desde cero. Preserva transparencia usando PNG cuando la
// fuente puede tener alfa (png/webp); las JPEG se recodifican a JPEG. Reescala a
// 1600px máx. Devuelve nil para GIF (se maneja aparte) o si no se puede decodificar.
func sanitizeImageBuffer(buf []byte, mimeType string) *optimizedImage {
	if mimeType == "image/gif" {
		return nil
	}
	src, _, err := image.Decode(bytes.NewReader(buf))
	if err != nil {
		return nil
	}
	b := src.Bounds()
	w, h := b.Dx(), b.Dy()
	if w == 0 || h == 0 {
		return nil
	}
	nw, nh := fitInside(w, h, imageMaxDimension)
	dst := src
	if nw != w || nh != h {
		rgba := image.NewRGBA(image.Rect(0, 0, nw, nh))
		xdraw.CatmullRom.Scale(rgba, rgba.Bounds(), src, b, xdraw.Over, nil)
		dst = rgba
	}
	// image/png y image/webp pueden llevar alfa: se re-encodean a PNG (lossless)
	// para no perder transparencia; las JPEG a JPEG.
	if mimeType == "image/jpeg" {
		var out bytes.Buffer
		if err := jpeg.Encode(&out, dst, &jpeg.Options{Quality: imageJPEGQuality}); err != nil {
			return nil
		}
		return &optimizedImage{buffer: out.Bytes(), mimeType: "image/jpeg"}
	}
	var out bytes.Buffer
	enc := png.Encoder{CompressionLevel: png.BestCompression}
	if err := enc.Encode(&out, dst); err != nil {
		return nil
	}
	return &optimizedImage{buffer: out.Bytes(), mimeType: "image/png"}
}

// optimizedImage es el resultado de optimizar una imagen.
type optimizedImage struct {
	buffer   []byte
	mimeType string
}

// OptimizeImage es el wrapper exportado para herramientas externas (cmd
// optimizeattach): reescala+recomprime una imagen. changed=false si no conviene.
func OptimizeImage(buf []byte, mime string) (out []byte, outMime string, changed bool) {
	if o := optimizeImageBuffer(buf, mime); o != nil {
		return o.buffer, o.mimeType, true
	}
	return buf, mime, false
}

// optimizeImageBuffer replica el objetivo de optimizeImageBuffer (sharp): rota
// según EXIF no se hace (raro en uploads de chat), reescala a máx 1600px y
// recodifica a JPEG de calidad 80. Devuelve nil si no conviene (gif animado o
// no se redujo el tamaño). Implementación en Go puro (sin cgo).
func optimizeImageBuffer(buf []byte, mimeType string) *optimizedImage {
	if mimeType == "image/gif" {
		return nil // los GIF (posibles animados) se dejan igual, como sharp
	}
	src, _, err := image.Decode(bytes.NewReader(buf))
	if err != nil {
		return nil
	}
	b := src.Bounds()
	w, h := b.Dx(), b.Dy()
	if w == 0 || h == 0 {
		return nil
	}
	nw, nh := fitInside(w, h, imageMaxDimension)

	var dst image.Image
	if nw == w && nh == h {
		dst = src
	} else {
		rgba := image.NewRGBA(image.Rect(0, 0, nw, nh))
		// CatmullRom: alta calidad (equivalente al resize "inside" de sharp).
		xdraw.CatmullRom.Scale(rgba, rgba.Bounds(), src, b, xdraw.Over, nil)
		dst = rgba
	}

	var out bytes.Buffer
	if err := jpeg.Encode(&out, dst, &jpeg.Options{Quality: imageJPEGQuality}); err != nil {
		return nil
	}
	// Solo vale la pena si redujo (margen de 1KB como sharp).
	if out.Len()+1024 >= len(buf) {
		return nil
	}
	return &optimizedImage{buffer: out.Bytes(), mimeType: "image/jpeg"}
}

// fitInside calcula el tamaño que cabe dentro de max×max conservando proporción
// (sin agrandar), igual que fit:"inside" + withoutEnlargement de sharp.
func fitInside(w, h, max int) (int, int) {
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

var _ = gif.Decode // mantener image/gif importado para registrar el decoder
