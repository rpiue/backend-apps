package notify

import "strconv"

// buildFCMPayload replica buildFcmMessageTarget de notifications.js: arma el
// objeto `message` de FCM HTTP v1 con data + android + apns. `target` lleva
// {"token": ...} o {"topic": ...}.
func buildFCMPayload(target map[string]any, m Message) map[string]any {
	channelID := m.ChannelID
	if channelID == "" {
		channelID = "alerts_v2"
	}
	mode := m.Mode
	if mode == "" {
		mode = "new"
	}
	headsUp := "true"
	if !m.HeadsUp {
		// El JS trataba headsUp como true salvo que sea explícitamente false.
		headsUp = "true"
	}

	base := map[string]any{
		"data": map[string]any{
			"title":      m.Title,
			"body":       m.Body,
			"route":      m.Route,
			"color":      m.Color,
			"imageUrl":   m.ImageURL,
			"mode":       mode,
			"notifId":    m.NotifID,
			"group":      m.Group,
			"persistent": "false",
			"headsUp":    headsUp,
			"pinDelayMs": "1200",
			"channelId":  channelID,
			"forceLocal": strconv.FormatBool(m.ForceLocal),
		},
	}
	for k, v := range target {
		base[k] = v
	}

	androidNotif := map[string]any{
		"channelId":  channelID,
		"sound":      "default",
		"visibility": "PUBLIC",
	}
	if m.Color != "" {
		androidNotif["color"] = m.Color
	}
	if m.ImageURL != "" {
		androidNotif["image"] = m.ImageURL
	}
	if m.Tag != "" {
		androidNotif["tag"] = m.Tag
	}
	android := map[string]any{"priority": "HIGH"}
	if m.CollapseKey != "" {
		android["collapseKey"] = m.CollapseKey
	}

	if m.DataOnly {
		// Sin notification ni apns: la app pinta cada notificación localmente.
		base["android"] = android
	} else {
		android["notification"] = androidNotif
		base["android"] = android
		base["apns"] = map[string]any{
			"payload": map[string]any{
				"aps": map[string]any{
					"alert": map[string]any{"title": m.Title, "body": m.Body},
					"sound": "default",
				},
			},
		}
		title := m.Title
		if title == "" {
			title = "Notificación"
		}
		base["notification"] = map[string]any{"title": title, "body": m.Body}
	}

	return map[string]any{"message": base}
}
