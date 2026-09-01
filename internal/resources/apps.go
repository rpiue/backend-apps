package resources

// AppsData replica getAppsData() de yape.js: la lista de "más apps" (maisAPPs)
// que /datosApp devuelve. Es un objeto keyed por "1","2","3".
func AppsData() map[string]any {
	return map[string]any{
		"1": map[string]any{
			"name":              "Bcp",
			"img":               "https://codexpe.com/img/bcp.webp",
			"packageName":       "codex.bcp.fake",
			"latestVersionName": "7.0.0",
			"status":            true,
		},
		"2": map[string]any{
			"name":              "Yape",
			"img":               "https://codexpe.com/img/yape-icon.webp",
			"packageName":       "com.codex.yape",
			"latestVersionName": "9.0.0",
			"status":            true,
		},
		"3": map[string]any{
			"name":              "Interbank",
			"img":               "https://codexpe.com/img/interbank.webp",
			"packageName":       "com.codex.interbank",
			"latestVersionName": "1.0.0",
			"status":            false,
		},
	}
}
