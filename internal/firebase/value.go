package firebase

import (
	"encoding/json"
	"strconv"
	"time"
)

// La API REST de Firestore representa cada valor con un "typed value"
// (stringValue, integerValue, timestampValue, mapValue, ...). Este codec
// traduce entre ese formato y `any` de Go, conservando la MISMA forma JSON
// que producía el SDK web (firebase/firestore/lite) para no romper la app.
//
// En particular, un Timestamp se decodifica a { seconds, nanoseconds }, tal
// como hacía Timestamp.toJSON() del SDK web (verificado en node_modules).

// Timestamp es el marcador para ESCRIBIR un timestamp en Firestore.
// Al serializarse a JSON adopta la forma { seconds, nanoseconds }, igual que
// Timestamp.toJSON() del SDK web, para mantener compatibilidad con la app.
type Timestamp struct{ Time time.Time }

func (t Timestamp) MarshalJSON() ([]byte, error) {
	return json.Marshal(tsJSON{Seconds: t.Time.Unix(), Nanoseconds: int64(t.Time.Nanosecond())})
}

// tsJSON es la forma de LECTURA de un timestamp (igual a Timestamp.toJSON del SDK web).
type tsJSON struct {
	Seconds     int64 `json:"seconds"`
	Nanoseconds int64 `json:"nanoseconds"`
}

// time reconstruye el time.Time de un timestamp leído, para poder REESCRIBIRLO.
func (t tsJSON) time() time.Time { return time.Unix(t.Seconds, t.Nanoseconds) }

// decodeValue convierte un typed value REST a un valor Go nativo.
func decodeValue(v map[string]any) any {
	for k, raw := range v {
		switch k {
		case "nullValue":
			return nil
		case "stringValue":
			return raw
		case "booleanValue":
			return raw
		case "integerValue":
			// REST lo entrega como string; lo pasamos a número.
			if s, ok := raw.(string); ok {
				if n, err := strconv.ParseInt(s, 10, 64); err == nil {
					return n
				}
			}
			return raw
		case "doubleValue":
			return raw
		case "timestampValue":
			if s, ok := raw.(string); ok {
				if t, err := time.Parse(time.RFC3339Nano, s); err == nil {
					return tsJSON{Seconds: t.Unix(), Nanoseconds: int64(t.Nanosecond())}
				}
			}
			return raw
		case "referenceValue":
			return raw
		case "geoPointValue":
			return raw
		case "bytesValue":
			return raw
		case "arrayValue":
			out := []any{}
			if m, ok := raw.(map[string]any); ok {
				if vals, ok := m["values"].([]any); ok {
					for _, item := range vals {
						if im, ok := item.(map[string]any); ok {
							out = append(out, decodeValue(im))
						}
					}
				}
			}
			return out
		case "mapValue":
			if m, ok := raw.(map[string]any); ok {
				if f, ok := m["fields"].(map[string]any); ok {
					return decodeFields(f)
				}
			}
			return map[string]any{}
		}
	}
	return nil
}

// decodeFields convierte el bloque "fields" de un documento a un map nativo.
func decodeFields(fields map[string]any) map[string]any {
	out := make(map[string]any, len(fields))
	for name, raw := range fields {
		if m, ok := raw.(map[string]any); ok {
			out[name] = decodeValue(m)
		}
	}
	return out
}

// encodeValue convierte un valor Go a un typed value REST.
func encodeValue(v any) map[string]any {
	switch val := v.(type) {
	case nil:
		return map[string]any{"nullValue": nil}
	case bool:
		return map[string]any{"booleanValue": val}
	case string:
		return map[string]any{"stringValue": val}
	case int:
		return map[string]any{"integerValue": strconv.FormatInt(int64(val), 10)}
	case int64:
		return map[string]any{"integerValue": strconv.FormatInt(val, 10)}
	case float64:
		// Si es entero exacto, lo guardamos como integer (como hace el cliente JS).
		if val == float64(int64(val)) {
			return map[string]any{"integerValue": strconv.FormatInt(int64(val), 10)}
		}
		return map[string]any{"doubleValue": val}
	case Timestamp:
		return map[string]any{"timestampValue": val.Time.UTC().Format(time.RFC3339Nano)}
	case time.Time:
		return map[string]any{"timestampValue": val.UTC().Format(time.RFC3339Nano)}
	case tsJSON:
		// Timestamp LEÍDO de Firestore que se vuelve a escribir (p.ej. el espejo de
		// `createdAt` en codigosIndex al renovar códigos grupales). Sin este caso
		// caía en `default` y se guardaba como NULL, borrando la fecha original.
		return map[string]any{"timestampValue": val.time().UTC().Format(time.RFC3339Nano)}
	case *tsJSON:
		if val == nil {
			return map[string]any{"nullValue": nil}
		}
		return map[string]any{"timestampValue": val.time().UTC().Format(time.RFC3339Nano)}
	case []any:
		vals := make([]any, 0, len(val))
		for _, item := range val {
			vals = append(vals, encodeValue(item))
		}
		return map[string]any{"arrayValue": map[string]any{"values": vals}}
	case map[string]any:
		return map[string]any{"mapValue": map[string]any{"fields": encodeFields(val)}}
	default:
		return map[string]any{"nullValue": nil}
	}
}

func encodeFields(m map[string]any) map[string]any {
	out := make(map[string]any, len(m))
	for k, v := range m {
		out[k] = encodeValue(v)
	}
	return out
}
