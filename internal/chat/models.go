package chat

import "time"

// PublicUser replica publicUser(row) de db.js (forma JSON que ve el frontend).
type PublicUser struct {
	ID             int64   `json:"id"`
	FirebaseUserID *string `json:"firebaseUserId"`
	App            string  `json:"app"`
	Email          string  `json:"email"`
	Nombre         *string `json:"nombre"`
	Apellidos      *string `json:"apellidos"`
	Telefono       *string `json:"telefono"`
	GuestNumber    *int64  `json:"guestNumber"`
	IsGuest        bool    `json:"isGuest"`
	Role           string  `json:"role"`
}

// Conversation es la forma reducida usada al crear/autenticar sesión.
type Conversation struct {
	ID      int64  `json:"id"`
	App     string `json:"app"`
	UserID  int64  `json:"userId,omitempty"`
	AdminID int64  `json:"adminId,omitempty"`
}

// Encrypted son los tres campos cifrados de un mensaje.
type Encrypted struct {
	Ciphertext string `json:"ciphertext"`
	IV         string `json:"iv"`
	Tag        string `json:"tag"`
}

// Attachment es el adjunto de un mensaje (forma JSON).
type Attachment struct {
	ID         int64   `json:"id"`
	Name       *string `json:"name"`
	MimeType   string  `json:"mimeType"`
	Size       int64   `json:"size"`
	Width      *int    `json:"width,omitempty"`
	Height     *int    `json:"height,omitempty"`
	DurationMs *int    `json:"durationMs,omitempty"`
	// Verdicto antimalware para que el cliente muestre la etiqueta sin ejecutar
	// el contenido: "clean" | "infected" | "error" | "unscanned".
	ScanStatus    string `json:"scanStatus"`
	ScanSignature string `json:"scanSignature,omitempty"`
}

// Reaction es una reacción agregada por emoji.
type Reaction struct {
	Emoji   string  `json:"emoji"`
	Count   int     `json:"count"`
	UserIDs []int64 `json:"userIds"`
}

// CompactMessage es la vista reducida del mensaje citado (replyTo).
type CompactMessage struct {
	ID         int64       `json:"id"`
	SenderID   int64       `json:"senderId"`
	SenderRole string      `json:"senderRole"`
	Kind       string      `json:"kind"`
	Ciphertext string      `json:"ciphertext"`
	IV         string      `json:"iv"`
	Tag        string      `json:"tag"`
	CreatedAt  time.Time   `json:"createdAt"`
	Attachment *Attachment `json:"attachment"`
}

// Message es la forma JSON de un mensaje del chat (listMessages).
type Message struct {
	ID               int64           `json:"id"`
	ConversationID   int64           `json:"conversationId"`
	SenderID         int64           `json:"senderId"`
	SenderRole       string          `json:"senderRole"`
	ReplyToMessageID *int64          `json:"replyToMessageId"`
	ReplyTo          *CompactMessage `json:"replyTo"`
	Kind             string          `json:"kind"`
	Ciphertext       string          `json:"ciphertext"`
	IV               string          `json:"iv"`
	Tag              string          `json:"tag"`
	ClientNonce      string          `json:"clientNonce"`
	CreatedAt        time.Time       `json:"createdAt"`
	Attachment       *Attachment     `json:"attachment"`
	Reactions        []Reaction      `json:"reactions"`
}

// rawMessage es lo que insertamos/devolvemos al crear (campos crudos de la fila).
type rawMessage struct {
	ID             int64
	ConversationID int64
	SenderID       int64
	Kind           string
}

// ConversationDetails es getConversationDetails (join con users).
type ConversationDetails struct {
	ID                     int64
	AppName                string
	UserID                 int64
	AdminID                int64
	Status                 string
	CreatedAt              time.Time
	LastUserPresenceAt     *time.Time
	LastUserSeenMessageID  *int64
	LastAdminSeenMessageID *int64
	UserEmail              string
	UserNombre             *string
	UserApellidos          *string
	UserGuestNumber        *int64
	AdminEmail             string
}

// UserDeviceStats resume los dispositivos/sesiones de un usuario del chat.
type UserDeviceStats struct {
	DeviceCount    int
	SessionCount   int
	FirstSeenAt    *time.Time
	LastSeenAt     *time.Time
	ActiveSessions int
}

// PlanOption es un plan que el admin puede otorgar (según lo disponible por app).
type PlanOption struct {
	Value string `json:"value"` // nombre exacto que espera GrantPlan
	Label string `json:"label"`
	Group string `json:"group"` // "personal" | "grupal"
}

// Presence es getConversationPresence.
type Presence struct {
	ID                     int64      `json:"id"`
	LastUserPresenceAt     *time.Time `json:"last_user_presence_at"`
	LastAdminPresenceAt    *time.Time `json:"last_admin_presence_at"`
	LastUserSeenMessageID  *int64     `json:"last_user_seen_message_id"`
	LastAdminSeenMessageID *int64     `json:"last_admin_seen_message_id"`
	UserOnline             bool       `json:"user_online"`
	AdminOnline            bool       `json:"admin_online"`
	Online                 *Online    `json:"online,omitempty"`
}

// Online refleja getConversationOnline (presencia por WebSocket en memoria).
type Online struct {
	UserOnline  bool `json:"userOnline"`
	AdminOnline bool `json:"adminOnline"`
}

// AdminConversation es un ítem de listConversationsForAdmin.
type AdminConversation struct {
	ID                 int64         `json:"id"`
	App                string        `json:"app"`
	Status             string        `json:"status"`
	UpdatedAt          time.Time     `json:"updatedAt"`
	LastUserPresenceAt *time.Time    `json:"lastUserPresenceAt"`
	LastMessage        *string       `json:"lastMessage"`
	UnreadCount        int           `json:"unreadCount"`
	User               AdminConvUser `json:"user"`
	Online             *Online       `json:"online,omitempty"`
	Labels             []Label       `json:"labels"`
}

type AdminConvUser struct {
	ID          int64   `json:"id"`
	Email       string  `json:"email"`
	Nombre      *string `json:"nombre"`
	Apellidos   *string `json:"apellidos"`
	Telefono    *string `json:"telefono"`
	GuestNumber *int64  `json:"guestNumber"`
	IsGuest     bool    `json:"isGuest"`
}

// FirebaseUserRef es el resultado de findFirebaseUsersByEmail.
type FirebaseUserRef struct {
	App       string  `json:"app"`
	ID        string  `json:"id"`
	Email     string  `json:"email"`
	Nombre    *string `json:"nombre"`
	Apellidos *string `json:"apellidos"`
	Telefono  *string `json:"telefono"`
}

// Automation es una automatización por patrón.
type Automation struct {
	ID            int64                 `json:"id"`
	Title         string                `json:"title"`
	Patterns      []string              `json:"patterns"`
	Response      string                `json:"response"`
	AppName       *string               `json:"app_name"`
	Enabled       bool                  `json:"enabled"`
	SendPaymentQR bool                  `json:"send_payment_qr"`
	MinScore      float64               `json:"min_score"`
	Attachment    *AutomationAttachment `json:"attachment"` // media opcional (img/video) que se envía
	CreatedAt     time.Time             `json:"created_at"`
	UpdatedAt     time.Time             `json:"updated_at"`
}

// AutomationAttachment son los metadatos del archivo (ya preparado en disco) que
// una respuesta automática envía junto al texto. Se guarda como jsonb en
// chat_automations.attachment y se reusa para crear el mensaje+adjunto al disparar.
type AutomationAttachment struct {
	Path       string `json:"path"`     // ruta en disco (dentro de UploadDir)
	Mime       string `json:"mime"`     // image/*, video/*
	Kind       string `json:"kind"`     // image | video
	Encoding   string `json:"encoding"` // identity | gzip
	Name       string `json:"name"`     // nombre original
	Size       int64  `json:"size"`     // bytes almacenados
	SHA256     string `json:"sha256"`
	DurationMs *int   `json:"durationMs,omitempty"`
}

// SessionRow es la sesión persistida (chat_sessions).
type SessionRow struct {
	JTI            string
	UserID         int64
	Role           string
	App            *string
	ConversationID *int64
	Email          *string
}

// UserSessionInfo es getLatestUserSessionInfo.
type UserSessionInfo struct {
	IPAddress  *string
	DeviceID   *string
	LastSeenAt *time.Time
	CreatedAt  time.Time
	ExpiresAt  time.Time
	RevokedAt  *time.Time
}

// AttachmentRow es la fila completa de un adjunto (para descarga).
type AttachmentRow struct {
	ID              int64
	MessageID       int64
	ConversationID  int64
	StoragePath     string
	OriginalName    *string
	MimeType        string
	SizeBytes       int64
	StorageEncoding string
}

// SendState es getConversationUserSendState.
type SendState struct {
	PendingUserMessages int
	Recent              []recentMsg
	DuplicateRecent     bool
}

type recentMsg struct {
	ID            int64
	SpamFinger    *string
	SpamSignature *string
	CreatedAt     time.Time
}
