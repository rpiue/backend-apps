package media

import (
	"bytes"
	"strings"
	"testing"
)

// El caso concreto que motivó todo esto: un ejecutable al que le cambian el
// nombre a "foto.png". El nombre y el Content-Type los elige quien sube el
// archivo, así que la única comprobación que vale es mirar los bytes.
func TestDetectarPeligro_EjecutableDisfrazadoDeImagen(t *testing.T) {
	casos := []struct {
		nombre    string
		contenido []byte
		tipo      string
	}{
		{"ELF de Linux", append([]byte{0x7f, 'E', 'L', 'F', 2, 1, 1}, make([]byte, 64)...), "elf"},
		{"EXE de Windows", append([]byte("MZ\x90\x00\x03"), make([]byte, 64)...), "pe"},
		{"Mach-O de macOS", append([]byte{0xcf, 0xfa, 0xed, 0xfe}, make([]byte, 64)...), "macho"},
		{"script con shebang", []byte("#!/usr/bin/env python3\nimport os\nos.system('rm -rf /')\n"), "script"},
		{"código PHP", []byte("<?php system($_GET['cmd']); ?>"), "php"},
		{"HTML con JavaScript", []byte("<script>fetch('/api/adm/usuarios')</script>"), "html"},
		{"SVG (ejecuta JS)", []byte(`<svg xmlns="http://www.w3.org/2000/svg"><script>alert(1)</script></svg>`), "svg"},
		{"ZIP/JAR/APK", append([]byte{'P', 'K', 0x03, 0x04}, make([]byte, 64)...), "zip"},
		{"clase de Java", append([]byte{0xca, 0xfe, 0xba, 0xbe}, make([]byte, 64)...), "class"},
		{"WebAssembly", append([]byte{0x00, 0x61, 0x73, 0x6d}, make([]byte, 64)...), "wasm"},
		{"PDF", append([]byte("%PDF-1.7"), make([]byte, 64)...), "pdf"},
		{"script de Python", []byte("import socket\ns = socket.socket()\n"), "script"},
	}
	for _, c := range casos {
		t.Run(c.nombre, func(t *testing.T) {
			p := DetectarPeligro(c.contenido)
			if p == nil {
				t.Fatalf("%s no se detectó como peligroso", c.nombre)
			}
			if p.Tipo != c.tipo {
				t.Errorf("tipo detectado %q, se esperaba %q", p.Tipo, c.tipo)
			}
			if p.Motivo == "" {
				t.Error("el motivo no debería estar vacío: se le enseña al usuario")
			}
			// Y lo esencial: Sniff tampoco debe confundirlo con media permitida.
			if m := Sniff(c.contenido); Permitido(m) {
				t.Errorf("Sniff lo aceptó como %q", m)
			}
		})
	}
}

// Contrapunto: los archivos legítimos no deben marcarse como peligrosos, o el
// sistema quedaría inutilizable.
func TestDetectarPeligro_MediaLegitimaPasa(t *testing.T) {
	casos := map[string][]byte{
		"image/jpeg": append([]byte{0xff, 0xd8, 0xff, 0xe0, 0, 0x10, 'J', 'F', 'I', 'F', 0, 1}, make([]byte, 32)...),
		"image/png":  append([]byte{0x89, 'P', 'N', 'G', 0x0d, 0x0a, 0x1a, 0x0a, 0, 0, 0, 13}, make([]byte, 32)...),
		"image/gif":  append([]byte("GIF89a"), make([]byte, 32)...),
		"video/mp4":  append([]byte{0, 0, 0, 0x20, 'f', 't', 'y', 'p', 'i', 's', 'o', 'm'}, make([]byte, 32)...),
		"audio/ogg":  append([]byte("OggS"), make([]byte, 32)...),
	}
	for esperado, contenido := range casos {
		if p := DetectarPeligro(contenido); p != nil {
			t.Errorf("%s se marcó como peligroso: %s", esperado, p.Motivo)
		}
		if got := Sniff(contenido); got != esperado {
			t.Errorf("Sniff devolvió %q, se esperaba %q", got, esperado)
		}
	}
}

// Un polyglot: cabecera de GIF válida seguida de código PHP. La cabecera manda,
// así que Sniff lo ve como GIF — por eso las imágenes se re-codifican además de
// comprobarse, que es lo que destruye la parte de código.
func TestSniff_PolyglotSeDetectaComoImagen(t *testing.T) {
	polyglot := append([]byte("GIF89a"), []byte("\x00\x00\x00\x00\x00\x00<?php system($_GET['c']); ?>")...)
	if got := Sniff(polyglot); got != "image/gif" {
		t.Fatalf("Sniff devolvió %q; la cabecera es de GIF", got)
	}
	// Documenta la limitación: la firma inicial gana, y por eso la defensa real
	// es re-codificar (imágenes) o limpiar los bloques (GIF), no solo olfatear.
	if p := DetectarPeligro(polyglot); p != nil {
		t.Logf("además se detectó como %s", p.Motivo)
	}
}

func TestSniff_ContenidoCortoOVacio(t *testing.T) {
	for _, b := range [][]byte{nil, {}, []byte("abc"), bytes.Repeat([]byte{0}, 11)} {
		if m := Sniff(b); m != "" {
			t.Errorf("con %d bytes Sniff devolvió %q, se esperaba vacío", len(b), m)
		}
	}
}

func TestDetectarPeligro_ToleraEspaciosYBOM(t *testing.T) {
	// Un atacante añade un BOM y saltos de línea para esquivar una comparación
	// ingenua de prefijo.
	conBOM := append([]byte("\xef\xbb\xbf\n\t  "), []byte("<?php echo 1;")...)
	p := DetectarPeligro(conBOM)
	if p == nil || p.Tipo != "php" {
		t.Fatalf("no se detectó el PHP precedido de BOM y espacios: %v", p)
	}
}

func TestClase(t *testing.T) {
	casos := map[string]string{
		"image/png": "image", "audio/mpeg": "audio", "video/mp4": "video",
		"application/pdf": "", "": "",
	}
	for mime, want := range casos {
		if got := Clase(mime); got != want {
			t.Errorf("Clase(%q)=%q, quería %q", mime, got, want)
		}
	}
}

func TestPermitido(t *testing.T) {
	if Permitido("application/x-msdownload") || Permitido("image/svg+xml") {
		t.Error("la lista blanca no debe incluir ejecutables ni SVG")
	}
	if !Permitido("image/jpeg") || !Permitido("video/mp4") {
		t.Error("faltan tipos legítimos en la lista blanca")
	}
	// El SVG merece mención aparte: es una imagen para el usuario, pero para el
	// navegador es un documento que puede ejecutar JavaScript.
	if strings.Contains(strings.Join(clavesDe(permitidos), ","), "svg") {
		t.Error("el SVG no debe estar permitido: ejecuta scripts al abrirse")
	}
}

func clavesDe(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
