package firebase

import (
	"os"
	"strings"
)

// Project identifica un proyecto Firestore por su ID, su apiKey web y el
// service-account JSON que debe usarse cuando el backend necesita permisos de
// servidor (transacciones/commit/beginTransaction). Cada proyecto del backend
// puede tener su propio archivo de credenciales.
type Project struct {
	ID              string
	APIKey          string
	CredentialsFile string
}

// Las claves web NO son secretas (viajan en las apps cliente), pero se permiten
// override por env. Los valores por defecto son los que ya estaban en db/firebase.js.
func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

// Registry resuelve el proyecto Firestore correcto según el caso de uso,
// replicando getDbByAppName() y la elección de DB en /datosApp del index.js.
type Registry struct {
	appPagos    Project // apppagos-1ec3f  -> usuarios app "yape"
	controller  Project // controller-b0871 -> datosApp de "yape"
	pagos2      Project // pagos2-4ddd3    -> "interbank" (usuarios y datosApp)
	bcp         Project // pago2-858c1     -> "bcp" (usuarios y datosApp)
	telefonos   Project // datatelefonos   -> /telefono
	controlPago Project // controlpagos-1262b -> códigos
}

func NewRegistry() *Registry {
	return &Registry{
		appPagos:    Project{ID: "apppagos-1ec3f", APIKey: envOr("FB_KEY_APPPAGOS", "AIzaSyAImehLPFTGMupcVxuzNNyWkrkkB6utx34"), CredentialsFile: envOr("FB_CREDS_APPPAGOS", "")},
		controller:  Project{ID: "controller-b0871", APIKey: envOr("FB_KEY_CONTROLLER", "AIzaSyDCpa3Pg4hcwxrnWl3-Fb4IhqqsDPO1wbg"), CredentialsFile: envOr("FB_CREDS_CONTROLLER", "")},
		pagos2:      Project{ID: "pagos2-4ddd3", APIKey: envOr("FB_KEY_PAGOS2", "AIzaSyCXdMUyFddmpE7WKhLgvki_dTPPCPOdQJ8"), CredentialsFile: envOr("FB_CREDS_PAGOS2", "")},
		bcp:         Project{ID: "pago2-858c1", APIKey: envOr("FB_KEY_BCP", "AIzaSyB0VDRSSLRMMrk34tBxWD_RRElhYiwh7lk"), CredentialsFile: envOr("FB_CREDS_BCP", "")},
		telefonos:   Project{ID: "datatelefonos", APIKey: envOr("FB_KEY_TELEFONOS", "AIzaSyBnuglEkjVNEUPGo7zVrcQ71MXZXTqEb1k"), CredentialsFile: envOr("FB_CREDS_TELEFONOS", "")},
		controlPago: Project{ID: "controlpagos-1262b", APIKey: envOr("FB_KEY_CONTROLPAGOS", "AIzaSyAdcbf95GpxBm8Rk7xKimQC2s7AGYVe2zM"), CredentialsFile: defaultCredentialsFile("FB_CREDS_CONTROLPAGOS", "control-pagos.json")},
	}
}

func defaultCredentialsFile(envName, fileName string) string {
	if v := strings.TrimSpace(os.Getenv(envName)); v != "" {
		return v
	}
	if v := strings.TrimSpace(os.Getenv("GOOGLE_APPLICATION_CREDENTIALS")); v != "" {
		return v
	}
	if dir := strings.TrimSpace(os.Getenv("FIREBASE_CREDS_DIR")); dir != "" {
		return dir + "/" + fileName
	}
	if _, err := os.Stat("./backend/secrets/" + fileName); err == nil {
		return "./backend/secrets/" + fileName
	}
	if _, err := os.Stat("./secrets/" + fileName); err == nil {
		return "./secrets/" + fileName
	}
	return ""
}

// UserDB replica getDbByAppName(app): yape/default -> apppagos, interbank -> pagos2, bcp -> bcp.
func (r *Registry) UserDB(app string) (Project, string) {
	switch normApp(app) {
	case "interbank":
		return r.pagos2, "interbank"
	case "bcp":
		return r.bcp, "bcp"
	default:
		return r.appPagos, "yape"
	}
}

// AppDataDB replica getDbForApp de app-data-cache.js:
// yape -> controller, interbank -> pagos2, bcp -> bcp.
func (r *Registry) AppDataDB(app string) (Project, string, bool) {
	switch normApp(app) {
	case "yape":
		return r.controller, "yape", true
	case "interbank":
		return r.pagos2, "interbank", true
	case "bcp":
		return r.bcp, "bcp", true
	default:
		return Project{}, "", false
	}
}

func (r *Registry) Telefonos() Project    { return r.telefonos }
func (r *Registry) ControlPagos() Project { return r.controlPago }

func normApp(app string) string {
	switch strings.ToLower(strings.TrimSpace(app)) {
	case "interbank":
		return "interbank"
	case "bcp":
		return "bcp"
	default:
		return "yape"
	}
}
