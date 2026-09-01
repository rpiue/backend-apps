// Package media identifica el tipo REAL de un archivo por su contenido y le
// quita los metadatos antes de guardarlo o servirlo.
//
// Los dos problemas que resuelve:
//
//  1. La extensión y el Content-Type los elige quien sube el archivo, así que no
//     son una comprobación de nada: un ejecutable renombrado a "foto.png" pasa
//     cualquier filtro basado en el nombre. Aquí el tipo se decide leyendo los
//     bytes de cabecera, y los formatos peligrosos (ejecutables, scripts,
//     archivos comprimidos) se reconocen EXPLÍCITAMENTE para poder rechazarlos
//     diciendo qué eran en realidad, en vez de un "no permitido" ciego.
//
//  2. Las fotos y los vídeos que salen de un móvil llevan dentro dónde y cuándo
//     se hicieron. Una imagen reenviada por el panel puede publicar las
//     coordenadas GPS de la casa de alguien sin que nadie lo pretenda.
package media

import (
	"bytes"
	"strings"
)

// Tipos permitidos en la plataforma (mismo conjunto que ya usaba el chat).
var permitidos = map[string]bool{
	"image/jpeg": true, "image/png": true, "image/webp": true, "image/gif": true,
	"audio/mpeg": true, "audio/mp4": true, "audio/ogg": true, "audio/webm": true,
	"video/mp4": true, "video/webm": true, "video/quicktime": true,
}

// Permitido indica si un MIME está en la lista blanca.
func Permitido(mime string) bool { return permitidos[mime] }

// Peligro describe un formato reconocido como ejecutable, script o contenedor.
// No es una lista de "malware": es una lista de cosas que NO son media y que,
// disfrazadas de imagen, solo tienen sentido si alguien pretende que el archivo
// acabe ejecutándose en algún sitio.
type Peligro struct {
	Tipo   string // identificador corto: "elf", "pe", "script"…
	Motivo string // explicación en castellano para el mensaje de error y el log
}

// firmasPeligrosas mapea prefijos de bytes a su descripción.
var firmasPeligrosas = []struct {
	magic  []byte
	tipo   string
	motivo string
}{
	{[]byte{0x7f, 'E', 'L', 'F'}, "elf", "ejecutable de Linux (ELF)"},
	{[]byte{'M', 'Z'}, "pe", "ejecutable de Windows (PE/DOS)"},
	{[]byte{0xfe, 0xed, 0xfa, 0xce}, "macho", "ejecutable de macOS (Mach-O)"},
	{[]byte{0xfe, 0xed, 0xfa, 0xcf}, "macho", "ejecutable de macOS (Mach-O 64)"},
	{[]byte{0xcf, 0xfa, 0xed, 0xfe}, "macho", "ejecutable de macOS (Mach-O)"},
	{[]byte{0xca, 0xfe, 0xba, 0xbe}, "class", "clase de Java o binario universal de macOS"},
	{[]byte{0x00, 0x61, 0x73, 0x6d}, "wasm", "módulo WebAssembly"},
	{[]byte{'#', '!'}, "script", "script con intérprete (#!)"},
	{[]byte{'P', 'K', 0x03, 0x04}, "zip", "archivo comprimido ZIP (o JAR/APK/DOCX)"},
	{[]byte{'P', 'K', 0x05, 0x06}, "zip", "archivo comprimido ZIP vacío"},
	{[]byte{'R', 'a', 'r', '!'}, "rar", "archivo comprimido RAR"},
	{[]byte{0x37, 0x7a, 0xbc, 0xaf}, "7z", "archivo comprimido 7-Zip"},
	{[]byte{0x1f, 0x8b}, "gzip", "archivo comprimido gzip"},
	{[]byte{'B', 'Z', 'h'}, "bzip2", "archivo comprimido bzip2"},
	{[]byte{0xfd, '7', 'z', 'X', 'Z'}, "xz", "archivo comprimido xz"},
	{[]byte{'%', 'P', 'D', 'F'}, "pdf", "documento PDF (puede llevar JavaScript embebido)"},
	{[]byte{0xd0, 0xcf, 0x11, 0xe0}, "ole", "documento de Office antiguo (puede llevar macros)"},
	{[]byte{'{', '\\', 'r', 't'}, "rtf", "documento RTF (vector conocido de exploits)"},
}

// prefijosDeTexto son inicios de archivo que delatan código fuente o marcado.
// Se comprueban sobre el principio del archivo ya en minúsculas y sin espacios.
var prefijosDeTexto = []struct {
	prefijo string
	tipo    string
	motivo  string
}{
	{"<?php", "php", "código PHP"},
	{"<?=", "php", "código PHP (etiqueta corta)"},
	{"<script", "html", "HTML con JavaScript"},
	{"<html", "html", "documento HTML"},
	{"<!doctype", "html", "documento HTML"},
	{"<svg", "svg", "SVG (puede ejecutar JavaScript al abrirse)"},
	{"<?xml", "xml", "XML (puede ser un SVG o un XXE)"},
	{"import ", "script", "código fuente (Python/JS)"},
	{"from ", "script", "código fuente (Python)"},
	{"package ", "script", "código fuente (Go/Java)"},
	{"function ", "script", "código fuente (JavaScript)"},
	{"#include", "script", "código fuente (C/C++)"},
	{"@echo", "bat", "script por lotes de Windows"},
	{"mz", "pe", "ejecutable de Windows"},
}

// DetectarPeligro devuelve una descripción si el contenido es un formato que no
// debería estar aquí. Devuelve nil si no reconoce nada peligroso — lo cual NO
// significa que sea seguro: la validación real es la lista blanca de Sniff.
func DetectarPeligro(b []byte) *Peligro {
	if len(b) == 0 {
		return nil
	}
	for _, f := range firmasPeligrosas {
		if bytes.HasPrefix(b, f.magic) {
			// gzip es un caso especial: el propio sistema comprime los adjuntos
			// con gzip para almacenarlos, así que un gzip aquí es plausible.
			// Aun así se marca, porque lo que SUBE el cliente no debería serlo.
			return &Peligro{Tipo: f.tipo, Motivo: f.motivo}
		}
	}
	// Contenido de texto: se mira el arranque sin espacios ni BOM.
	cabecera := b
	if len(cabecera) > 512 {
		cabecera = cabecera[:512]
	}
	cabecera = bytes.TrimLeft(cabecera, "\xef\xbb\xbf \t\r\n")
	minus := strings.ToLower(string(cabecera))
	for _, p := range prefijosDeTexto {
		if strings.HasPrefix(minus, p.prefijo) {
			return &Peligro{Tipo: p.tipo, Motivo: p.motivo}
		}
	}
	return nil
}

// Sniff determina el MIME real por los bytes de cabecera. Devuelve "" si el
// contenido no es ninguno de los formatos de media permitidos.
//
// Es la MISMA lógica que ya usaba el chat, movida aquí para que todas las vías
// de subida (chat, panel de admin, automatizaciones) compartan un solo criterio
// en vez de tener cada una el suyo.
func Sniff(b []byte) string {
	if len(b) < 12 {
		return ""
	}
	switch {
	case b[0] == 0xff && b[1] == 0xd8 && b[2] == 0xff:
		return "image/jpeg"
	case bytes.HasPrefix(b, []byte{0x89, 'P', 'N', 'G', 0x0d, 0x0a, 0x1a, 0x0a}):
		return "image/png"
	case string(b[0:6]) == "GIF87a" || string(b[0:6]) == "GIF89a":
		return "image/gif"
	case string(b[0:4]) == "RIFF" && string(b[8:12]) == "WEBP":
		return "image/webp"
	case bytes.HasPrefix(b, []byte{0x1a, 0x45, 0xdf, 0xa3}):
		return "video/webm"
	case string(b[0:4]) == "OggS":
		return "audio/ogg"
	case string(b[0:3]) == "ID3" || (b[0] == 0xff && (b[1]&0xe0) == 0xe0):
		return "audio/mpeg"
	case string(b[4:8]) == "ftyp":
		return "video/mp4" // mp4/m4a/mov comparten el contenedor ISO-BMFF
	}
	return ""
}

// Clase agrupa el MIME en image/audio/video (cadena vacía si no aplica).
func Clase(mime string) string {
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
