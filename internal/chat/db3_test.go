package chat

import (
	"regexp"
	"strconv"
	"testing"
)

var placeholderRe = regexp.MustCompile(`\$(\d+)`)

// maxPlaceholder devuelve el $N más alto referenciado en el SQL.
func maxPlaceholder(sql string) int {
	max := 0
	for _, m := range placeholderRe.FindAllStringSubmatch(sql, -1) {
		if n, _ := strconv.Atoi(m[1]); n > max {
			max = n
		}
	}
	return max
}

// El WHERE se arma concatenando filtros opcionales, así que un $N mal numerado
// solo explotaría en runtime contra Postgres. Cada combinación debe referenciar
// exactamente tantos placeholders como parámetros se pasan a pgx.
func TestBuildConvWhere_PlaceholdersCuadranConParams(t *testing.T) {
	casos := map[string]ConvFilters{
		"sin filtros":     {},
		"solo app":        {App: "yape"},
		"solo busqueda":   {Search: "jairo"},
		"solo no leidos":  {UnreadOnly: true},
		"solo etiqueta":   {LabelID: 7},
		"app+busqueda":    {App: "bcp", Search: "ana"},
		"busqueda+label":  {Search: "ana", LabelID: 3},
		"no leidos+label": {UnreadOnly: true, LabelID: 3},
		"todos":           {App: "yape", Search: "ana", UnreadOnly: true, LabelID: 9},
	}
	for nombre, f := range casos {
		t.Run(nombre, func(t *testing.T) {
			// limit/offset ocupan siempre $1 y $2.
			where, params := buildConvWhere(f, []any{30, 0})
			if got := maxPlaceholder(where); got > len(params) {
				t.Fatalf("el WHERE usa $%d pero solo hay %d params: %s", got, len(params), where)
			}
			// Ningún filtro puede dejar un $1/$2 sin usar por delante: los params
			// añadidos deben empezar justo en $3.
			if len(params) > 2 && maxPlaceholder(where) != len(params) {
				t.Fatalf("params=%d pero el mayor placeholder es $%d: %s", len(params), maxPlaceholder(where), where)
			}
		})
	}
}

// El filtro "No leídos" debe resolverse en SQL (era el que obligaba a pulsar
// "Cargar más" sin parar cuando se filtraba en el cliente).
func TestBuildConvWhere_NoLeidosVaEnSQL(t *testing.T) {
	where, params := buildConvWhere(ConvFilters{UnreadOnly: true}, []any{30, 0})
	if where == "" {
		t.Fatal("unreadOnly no generó WHERE")
	}
	if len(params) != 2 {
		t.Fatalf("unreadOnly no debe añadir params, hay %d", len(params))
	}
	if !regexp.MustCompile(`(?s)exists.*chat_messages`).MatchString(where) {
		t.Fatalf("se esperaba un EXISTS sobre chat_messages: %s", where)
	}
}

// La etiqueta filtra por chat_conversation_labels con su propio parámetro.
func TestBuildConvWhere_EtiquetaParametrizada(t *testing.T) {
	where, params := buildConvWhere(ConvFilters{LabelID: 42}, []any{30, 0})
	if len(params) != 3 || params[2] != int64(42) {
		t.Fatalf("se esperaba el labelID como tercer param, got %v", params)
	}
	if maxPlaceholder(where) != 3 {
		t.Fatalf("el labelID debía ser $3: %s", where)
	}
}
