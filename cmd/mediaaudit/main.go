// Command mediaaudit revisa los archivos multimedia que YA están en el servidor.
//
// Está pensado para lo que ya existe en el VPS: miles de adjuntos subidos por el
// sistema anterior, cuando el tipo se aceptaba tal como lo declaraba el cliente
// y nadie tocaba los metadatos. Las defensas nuevas solo actúan sobre lo que
// llega de ahora en adelante; esto es para lo de antes.
//
// REGLA DE ORO: esta herramienta NUNCA BORRA NADA.
//
//   - Por defecto solo MIRA e informa. No escribe un solo byte.
//   - Con -perms quita el permiso de ejecución (no altera el contenido).
//   - Con -limpiar quita los metadatos, y antes deja una copia .bak del original.
//   - Con -aislar MUEVE los archivos sospechosos a otra carpeta. Mover no es
//     borrar: siguen ahí y se devuelven con un `mv` si hiciera falta.
//
// Uso:
//
//	go run ./cmd/mediaaudit /root/api/chat/uploads              # solo informe
//	go run ./cmd/mediaaudit -perms /root/api/chat/uploads       # + quita el bit de ejecución
//	go run ./cmd/mediaaudit -limpiar /root/api/chat/uploads     # + quita metadatos (con copia)
//	go run ./cmd/mediaaudit -aislar /root/cuarentena /root/api/chat/uploads
package main

import (
	"bytes"
	"compress/gzip"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"codex/backend/internal/media"
)

// leerCabecera lee como mucho n bytes: para identificar el tipo no hace falta
// cargar en memoria un vídeo de 60 MB.
const bytesDeCabecera = 8192

type resumen struct {
	total       int
	porTipo     map[string]int
	sospechosos []hallazgo
	conMeta     []string
	ejecutables []string
	limpiados   int
	permisos    int
	aislados    int
	errores     int
}

type hallazgo struct {
	ruta   string
	motivo string
	tipo   string
}

func main() {
	var (
		perms   = flag.Bool("perms", false, "quita el permiso de ejecución de los archivos (no altera el contenido)")
		limpiar = flag.Bool("limpiar", false, "elimina los metadatos, dejando antes una copia .bak del original")
		aislar  = flag.String("aislar", "", "MUEVE los archivos sospechosos a esta carpeta (no los borra)")
		sinBak  = flag.Bool("sin-copia", false, "con -limpiar, no deja la copia .bak (más rápido, sin vuelta atrás)")
		verbose = flag.Bool("v", false, "lista también los archivos correctos")
	)
	flag.Parse()

	dirs := flag.Args()
	if len(dirs) == 0 {
		fmt.Fprintln(os.Stderr, "Uso: mediaaudit [opciones] <carpeta> [carpeta...]")
		flag.PrintDefaults()
		os.Exit(2)
	}

	soloMirar := !*perms && !*limpiar && *aislar == ""
	fmt.Println("═══════════════════════════════════════════════════════════")
	fmt.Println(" Revisión de multimedia existente")
	if soloMirar {
		fmt.Println(" MODO INFORME: no se modifica ni se mueve ningún archivo.")
	} else {
		fmt.Println(" MODO CORRECCIÓN. Recuerda: esta herramienta nunca borra.")
		if *limpiar && !*sinBak {
			fmt.Println("   · Antes de limpiar cada archivo se deja una copia .bak")
		}
		if *aislar != "" {
			fmt.Printf("   · Los sospechosos se MUEVEN a %s\n", *aislar)
		}
	}
	fmt.Println("═══════════════════════════════════════════════════════════")

	r := &resumen{porTipo: map[string]int{}}
	for _, d := range dirs {
		recorrer(d, r, *perms, *limpiar, *sinBak, *aislar, *verbose)
	}
	imprimirResumen(r, soloMirar, *aislar)

	// Código de salida 1 si hay algo sospechoso, para poder encadenarlo.
	if len(r.sospechosos) > 0 {
		os.Exit(1)
	}
}

func recorrer(raiz string, r *resumen, perms, limpiar, sinBak bool, aislar string, verbose bool) {
	raizAbs, _ := filepath.Abs(raiz)
	err := filepath.WalkDir(raiz, func(ruta string, d os.DirEntry, err error) error {
		if err != nil {
			fmt.Printf("  ⚠  no se pudo leer %s: %v\n", ruta, err)
			r.errores++
			return nil
		}
		if d.IsDir() {
			// No se entra en la carpeta de cuarentena: ya se revisó lo que hay.
			if aislar != "" {
				if abs1, e1 := filepath.Abs(ruta); e1 == nil {
					if abs2, e2 := filepath.Abs(aislar); e2 == nil && abs1 == abs2 {
						return filepath.SkipDir
					}
				}
			}
			return nil
		}
		// Las copias de seguridad de una pasada anterior no se revisan.
		if strings.HasSuffix(ruta, ".bak") {
			return nil
		}
		revisarArchivo(ruta, raizAbs, d, r, perms, limpiar, sinBak, aislar, verbose)
		return nil
	})
	if err != nil {
		fmt.Printf("  ⚠  error recorriendo %s: %v\n", raiz, err)
		r.errores++
	}
}

func revisarArchivo(ruta, raiz string, d os.DirEntry, r *resumen, perms, limpiar, sinBak bool, aislar string, verbose bool) {
	r.total++

	info, err := d.Info()
	if err != nil {
		r.errores++
		return
	}

	// 1) Permiso de ejecución. Un archivo de datos nunca debería tenerlo; si lo
	//    tiene es por un umask mal puesto o por una copia desde otro sistema.
	//    Quitarlo no altera el contenido y es reversible.
	if info.Mode().Perm()&0o111 != 0 {
		r.ejecutables = append(r.ejecutables, ruta)
		if perms {
			if err := os.Chmod(ruta, info.Mode().Perm()&^0o111); err == nil {
				r.permisos++
			} else {
				fmt.Printf("  ⚠  no se pudo quitar el bit de ejecución de %s: %v\n", ruta, err)
				r.errores++
			}
		}
	}

	cabecera, err := leerPrefijo(ruta, bytesDeCabecera)
	if err != nil {
		r.errores++
		return
	}
	if len(cabecera) == 0 {
		return // archivo vacío: no hay nada que interpretar
	}

	// 2) El chat guarda muchos adjuntos comprimidos con gzip. Para saber qué son
	//    de verdad hay que mirar DENTRO, no quedarse en la capa de compresión.
	contenido, eraGzip := descomprimirSiHaceFalta(cabecera)

	// 3) ¿Es algo que no debería estar aquí?
	if p := media.DetectarPeligro(contenido); p != nil {
		r.sospechosos = append(r.sospechosos, hallazgo{ruta: ruta, motivo: p.Motivo, tipo: p.Tipo})
		fmt.Printf("  ✗ SOSPECHOSO  %s\n      es %s\n", ruta, p.Motivo)
		if aislar != "" {
			moverACuarentena(ruta, raiz, aislar, r)
		}
		return
	}

	mime := media.Sniff(contenido)
	if mime == "" {
		r.porTipo["desconocido"]++
		if verbose {
			fmt.Printf("  ?  %s — no se reconoce el formato (se sirve como descarga opaca)\n", ruta)
		}
		return
	}
	r.porTipo[mime]++

	// 4) ¿Le quedan metadatos dentro?
	//    Los archivos comprimidos se dejan para el final: limpiarlos requiere
	//    recomprimir y actualizar el tamaño en la base de datos, así que aquí
	//    solo se informa.
	if eraGzip {
		if verbose {
			fmt.Printf("  ·  %s (%s, comprimido) — no se limpia desde aquí\n", ruta, mime)
		}
		return
	}
	// Con solo la cabecera no se puede limpiar de verdad: hay que leerlo entero.
	completo, err := os.ReadFile(ruta)
	if err != nil {
		r.errores++
		return
	}
	limpio, cambiado := media.StripMetadata(completo, mime)
	if !cambiado {
		if verbose {
			fmt.Printf("  ✓  %s (%s) — sin metadatos\n", ruta, mime)
		}
		return
	}

	r.conMeta = append(r.conMeta, ruta)
	if !limpiar {
		fmt.Printf("  ●  CON METADATOS  %s (%s)\n", ruta, mime)
		return
	}

	// Copia de seguridad antes de tocar nada.
	if !sinBak {
		if err := os.WriteFile(ruta+".bak", completo, info.Mode().Perm()); err != nil {
			fmt.Printf("  ⚠  no se pudo respaldar %s (no se toca): %v\n", ruta, err)
			r.errores++
			return
		}
	}
	if err := escrituraAtomica(ruta, limpio, info.Mode().Perm()); err != nil {
		fmt.Printf("  ⚠  no se pudo limpiar %s: %v\n", ruta, err)
		r.errores++
		return
	}
	r.limpiados++
	fmt.Printf("  ✓  LIMPIADO  %s (%s, %d → %d bytes)\n", ruta, mime, len(completo), len(limpio))
}

// escrituraAtomica escribe en un temporal del mismo directorio y luego renombra.
// Si el proceso se corta a medias, el archivo original sigue intacto: nunca
// queda un adjunto a medio escribir.
func escrituraAtomica(ruta string, datos []byte, modo os.FileMode) error {
	tmp := ruta + ".tmp-mediaaudit"
	if err := os.WriteFile(tmp, datos, modo); err != nil {
		return err
	}
	if err := os.Rename(tmp, ruta); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	return nil
}

// moverACuarentena MUEVE el archivo. No lo borra: queda en la otra carpeta, con
// la estructura de directorios replicada, y se puede devolver con un `mv`.
func moverACuarentena(ruta, raiz, destino string, r *resumen) {
	// Se conserva la estructura de carpetas relativa a lo escaneado, para que
	// devolver un archivo a su sitio sea copiar la misma ruta de vuelta.
	rel := ruta
	if abs, err := filepath.Abs(ruta); err == nil && raiz != "" {
		if s, err := filepath.Rel(raiz, abs); err == nil && !strings.HasPrefix(s, "..") {
			rel = s
		}
	}
	final := filepath.Join(destino, rel)

	if err := os.MkdirAll(filepath.Dir(final), 0o700); err != nil {
		fmt.Printf("      ⚠  no se pudo crear la carpeta de cuarentena: %v\n", err)
		r.errores++
		return
	}
	if err := os.Rename(ruta, final); err != nil {
		// Puede fallar si origen y destino están en discos distintos: se copia.
		datos, e := os.ReadFile(ruta)
		if e != nil {
			fmt.Printf("      ⚠  no se pudo aislar: %v\n", err)
			r.errores++
			return
		}
		if e := os.WriteFile(final, datos, 0o600); e != nil {
			fmt.Printf("      ⚠  no se pudo aislar: %v\n", e)
			r.errores++
			return
		}
		if e := os.Remove(ruta); e != nil {
			fmt.Printf("      ⚠  copiado a cuarentena pero el original sigue en su sitio: %v\n", e)
			r.errores++
			return
		}
	}
	r.aislados++
	fmt.Printf("      → movido a %s\n", final)
}

// descomprimirSiHaceFalta mira dentro de un gzip para identificar el contenido
// real. Devuelve el original si no es gzip o si no se puede descomprimir.
func descomprimirSiHaceFalta(b []byte) ([]byte, bool) {
	if len(b) < 2 || b[0] != 0x1f || b[1] != 0x8b {
		return b, false
	}
	zr, err := gzip.NewReader(bytes.NewReader(b))
	if err != nil {
		return b, false
	}
	defer zr.Close()
	// Tope a la expansión: un gzip diminuto puede descomprimirse a gigabytes
	// (bomba de descompresión) y dejar sin memoria a la herramienta.
	dec, err := io.ReadAll(io.LimitReader(zr, bytesDeCabecera))
	if err != nil && len(dec) == 0 {
		return b, false
	}
	return dec, true
}

func leerPrefijo(ruta string, n int) ([]byte, error) {
	f, err := os.Open(ruta)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	buf := make([]byte, n)
	leidos, err := io.ReadFull(f, buf)
	if err != nil && err != io.EOF && err != io.ErrUnexpectedEOF {
		return nil, err
	}
	return buf[:leidos], nil
}

func imprimirResumen(r *resumen, soloMirar bool, aislar string) {
	fmt.Println("\n═══════════════════════════════════════════════════════════")
	fmt.Printf(" RESUMEN — %d archivos revisados\n", r.total)
	fmt.Println("═══════════════════════════════════════════════════════════")

	if len(r.porTipo) > 0 {
		fmt.Println("\n Por tipo real (según su contenido, no su extensión):")
		tipos := make([]string, 0, len(r.porTipo))
		for t := range r.porTipo {
			tipos = append(tipos, t)
		}
		sort.Strings(tipos)
		for _, t := range tipos {
			fmt.Printf("   %-24s %d\n", t, r.porTipo[t])
		}
	}

	if len(r.ejecutables) > 0 {
		fmt.Printf("\n ● %d archivo(s) con permiso de EJECUCIÓN\n", len(r.ejecutables))
		if r.permisos > 0 {
			fmt.Printf("   Corregidos: %d\n", r.permisos)
		} else {
			fmt.Println("   Quítaselo con:  mediaaudit -perms <carpeta>")
		}
	}

	if len(r.conMeta) > 0 {
		fmt.Printf("\n ● %d archivo(s) CON METADATOS dentro (fecha, dispositivo, y en fotos/vídeos de móvil, GPS)\n", len(r.conMeta))
		if r.limpiados > 0 {
			fmt.Printf("   Limpiados: %d (el original quedó en el .bak de al lado)\n", r.limpiados)
		} else {
			fmt.Println("   Límpialos con:  mediaaudit -limpiar <carpeta>")
		}
	}

	if len(r.sospechosos) > 0 {
		fmt.Printf("\n ✗ %d archivo(s) SOSPECHOSOS — su contenido no es multimedia:\n", len(r.sospechosos))
		porTipo := map[string][]string{}
		for _, h := range r.sospechosos {
			porTipo[h.motivo] = append(porTipo[h.motivo], h.ruta)
		}
		for motivo, rutas := range porTipo {
			fmt.Printf("   · %s: %d\n", motivo, len(rutas))
			for i, ru := range rutas {
				if i >= 5 {
					fmt.Printf("       … y %d más\n", len(rutas)-5)
					break
				}
				fmt.Printf("       %s\n", ru)
			}
		}
		if r.aislados > 0 {
			fmt.Printf("\n   Movidos a cuarentena: %d (siguen existiendo, no se borró nada)\n", r.aislados)
		} else if aislar == "" {
			fmt.Println("\n   Míralos antes de decidir. Para apartarlos SIN borrarlos:")
			fmt.Println("     mediaaudit -aislar /root/cuarentena <carpeta>")
		}
		fmt.Println("\n   Contexto para no alarmarse de más: el servidor Go NO ejecuta")
		fmt.Println("   nada de esta carpeta (no hay intérprete detrás), y al servirse")
		fmt.Println("   van con nosniff y CSP sandbox. El riesgo real es que tu dominio")
		fmt.Println("   esté alojando archivos que no son lo que dicen ser.")
	} else {
		fmt.Println("\n ✓ Ningún archivo sospechoso.")
	}

	if r.errores > 0 {
		fmt.Printf("\n ⚠  %d error(es) de lectura o escritura (ver arriba)\n", r.errores)
	}

	if soloMirar && (len(r.conMeta) > 0 || len(r.ejecutables) > 0) {
		fmt.Println("\n No se modificó nada: esto fue solo un informe.")
	}
	fmt.Println()
}
