package chat

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

const (
	chatTextMaxLength       = 4000
	chatCiphertextMaxLength = 20000
	maxPendingUserMessages  = 10
)

// parseEncrypted replica parseEncrypted: acepta {ciphertext,iv,tag} o texto plano.
func parseEncrypted(enc *Encrypted, text string) (*Encrypted, error) {
	ciphertext, iv, tag := "", "", ""
	if enc != nil {
		ciphertext, iv, tag = enc.Ciphertext, enc.IV, enc.Tag
	}
	if ciphertext == "" || iv == "" {
		t := strings.TrimSpace(text)
		if t == "" {
			return nil, nil
		}
		if len(t) > chatTextMaxLength {
			return nil, newChatErr(fmt.Sprintf("Mensaje demasiado largo. Maximo %d caracteres.", chatTextMaxLength), 400)
		}
		return &Encrypted{
			Ciphertext: base64.StdEncoding.EncodeToString([]byte(t)),
			IV:         "plain-dev",
			Tag:        "",
		}, nil
	}
	if len(ciphertext) > chatCiphertextMaxLength {
		return nil, newChatErr(fmt.Sprintf("Mensaje cifrado demasiado largo. Maximo %d caracteres.", chatCiphertextMaxLength), 400)
	}
	return &Encrypted{Ciphertext: ciphertext, IV: iv, Tag: tag}, nil
}

// enforceUserSendGuard replica enforceUserSendGuard (anti-spam). Devuelve los
// spamFingerprint/spamSignature finales o un error con statusCode.
func (s *Service) enforceUserSendGuard(ctx context.Context, role string, conversationID, userID int64, text string, fpIn, sigIn *string) (*string, *string, error) {
	if role == "admin" {
		return fpIn, sigIn, nil
	}
	fp, sig := spamSignal(text)
	finalFp := fp
	if fpIn != nil {
		finalFp = *fpIn
	}
	finalSig := sig
	if sigIn != nil {
		finalSig = *sigIn
	}
	var fpPtr *string
	if finalFp != "" {
		fpPtr = &finalFp
	}
	state, err := s.db.getConversationUserSendState(ctx, conversationID, userID, fpPtr)
	if err != nil {
		return nil, nil, err
	}
	if fpIn == nil && isLowInformationText(text) && state.PendingUserMessages > 0 {
		return nil, nil, newChatErr("Envia un mensaje mas claro para que soporte pueda ayudarte.", 429)
	}
	if state.PendingUserMessages >= maxPendingUserMessages {
		return nil, nil, newChatErr("Espera la respuesta del admin antes de enviar mas mensajes.", 429)
	}
	if state.DuplicateRecent {
		return nil, nil, newChatErr("Ya enviaste ese mensaje recientemente. Intenta otra vez en 5 minutos.", 429)
	}
	if finalSig != "" {
		for _, row := range state.Recent {
			if row.CreatedAt.After(time.Now().Add(-5*time.Minute)) && row.SpamSignature != nil &&
				signatureSimilarity(finalSig, *row.SpamSignature) >= 0.82 {
				return nil, nil, newChatErr("Detectamos mensajes repetitivos. Espera 5 minutos antes de repetir la misma consulta.", 429)
			}
		}
	}
	return ptrOrNil(finalFp), ptrOrNil(finalSig), nil
}

func ptrOrNil(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

// authedUser es el resultado de autenticar una sesión de usuario por app/email/pin.
type authedUser struct {
	user         *PublicUser
	conversation *Conversation
	nameApp      string
}

func (s *Service) authenticateUserSession(ctx context.Context, app, email, pin string) (*authedUser, error) {
	cleanEmail := normalizeEmail(email)
	cleanPin := strings.TrimSpace(pin)
	cleanApp := normalizeApp(app)
	if cleanEmail == "" || cleanPin == "" || cleanApp == "" {
		return nil, newChatErr("Faltan email, pin y app", 400)
	}
	users, nameApp, err := s.dir.Lookup(ctx, cleanApp, cleanEmail, &cleanPin)
	if err != nil {
		return nil, err
	}
	var fb *FirebaseUser
	if len(users) > 0 {
		fb = &users[0]
	}
	var fbID *string
	if fb != nil && fb.ID != "" {
		fbID = &fb.ID
	}
	var nombre, apellidos, telefono *string
	if fb != nil {
		nombre, apellidos, telefono = fb.Nombre, fb.Apellidos, fb.Telefono
	}
	u, err := s.db.upsertChatUser(ctx, nameApp, cleanEmail, fbID, nombre, apellidos, telefono, "user")
	if err != nil {
		return nil, err
	}
	conv, err := s.db.ensureConversation(ctx, nameApp, u.ID)
	if err != nil {
		return nil, err
	}
	return &authedUser{user: u, conversation: conv, nameApp: nameApp}, nil
}

func adminLookupApps(app string) []string {
	if strings.TrimSpace(app) != "" {
		return []string{normalizeApp(app)}
	}
	return []string{"yape", "bcp", "interbank"}
}

func (s *Service) findFirebaseUsersByEmail(ctx context.Context, app, email string) []FirebaseUserRef {
	cleanEmail := normalizeEmail(email)
	if cleanEmail == "" {
		return nil
	}
	out := []FirebaseUserRef{}
	for _, appName := range adminLookupApps(app) {
		users, nameApp, err := s.dir.Lookup(ctx, appName, cleanEmail, nil)
		if err != nil {
			continue
		}
		for _, item := range users {
			if item.Email == "" {
				continue
			}
			out = append(out, FirebaseUserRef{
				App: nameApp, ID: item.ID, Email: normalizeEmail(item.Email),
				Nombre: item.Nombre, Apellidos: item.Apellidos, Telefono: item.Telefono,
			})
		}
	}
	return out
}

// signSessionFor crea la sesión persistida + JWT (replica signSession).
func (s *Service) signSessionFor(fp fingerprint, ip string, userID int64, role, app string, conversationID *int64, email string, deviceID *string) (string, error) {
	var appPtr *string
	if app != "" {
		appPtr = &app
	}
	var emailPtr *string
	if email != "" {
		emailPtr = &email
	}
	var ipPtr *string
	if ip != "" {
		ipPtr = &ip
	}
	jti, err := s.db.createChatSession(context.Background(), userID, role, appPtr, conversationID, emailPtr, fp.userAgentHash, fp.ipHash, ipPtr, deviceID, 12)
	if err != nil {
		return "", err
	}
	return s.signToken(userID, role, app, conversationID, email, jti)
}

// preparedFile contiene el adjunto ya validado y escrito a disco.
type attachmentResult struct {
	message *Message
	kind    string
}

// createAttachmentMessage replica createAttachmentMessage: valida, guarda en
// disco y crea el mensaje + adjunto. fileBuf es el contenido subido.
func (s *Service) createAttachmentMessage(ctx context.Context, conversationID, senderID int64, senderRole string, fileBuf []byte, declaredMime, originalName string, enc *Encrypted, clientNonce string, replyTo *int64, text string, durationMs *int) (*attachmentResult, error) {
	if len(fileBuf) == 0 {
		return nil, newChatErr("Archivo no permitido", 400)
	}
	prepared, err := validateAndPrepareUpload(fileBuf, declaredMime, durationMs)
	if err != nil {
		return nil, err
	}
	// Escaneo antimalware (ClamAV) sobre el buffer ORIGINAL subido — así el
	// verdicto refleja lo que el usuario envió (para la analítica) aunque la
	// versión almacenada de las imágenes ya venga re-encodeada/saneada. NUNCA
	// bloquea: el archivo se guarda igual, solo se marca.
	scanStatus, scanSig := s.scanAttachment(ctx, fileBuf)
	hash := sha256OfBytes(fileBuf)
	fpStr := prepared.Kind + ":" + hash
	sigStr := prepared.Kind + " " + hash
	finalFp, finalSig, err := s.enforceUserSendGuard(ctx, senderRole, conversationID, senderID, text, &fpStr, &sigStr)
	if err != nil {
		return nil, err
	}

	used, err := s.db.getStorageUsage(ctx, senderID)
	if err != nil {
		return nil, err
	}
	if used+prepared.StoredSize > userQuotaBytes {
		return nil, newChatErr("Limite de almacenamiento alcanzado", 413)
	}

	ext := filepath.Ext(originalName)
	ext = strings.ToLower(ext)
	safeName := fmt.Sprintf("%d-%s%s", time.Now().UnixMilli(), newUUID(), ext)
	diskPath := filepath.Join(s.cfg.UploadDir, safeName)
	if err := os.WriteFile(diskPath, prepared.Buffer, 0o600); err != nil {
		return nil, err
	}

	if enc == nil {
		enc = &Encrypted{}
	}
	created, err := s.db.createMessage(ctx, conversationID, senderID, prepared.Kind, *enc, clientNonce, finalFp, finalSig, replyTo)
	if err != nil {
		_ = os.Remove(diskPath)
		return nil, err
	}
	name := originalName
	if name == "" {
		name = safeName
	}
	if err := s.db.createAttachment(ctx, created.ID, senderID, fileMeta{
		Path: diskPath, OriginalName: name, MimeType: prepared.MimeType, Size: prepared.StoredSize,
		SHA256: hash, DurationMs: prepared.DurationMs, StorageEncoding: prepared.StorageEncoding,
		ScanStatus: scanStatus, ScanSignature: scanSig,
	}); err != nil {
		return nil, err
	}
	// Registra el evento de malware para analítica (best-effort, no bloquea).
	if scanStatus == scanInfected {
		s.recordMalwareEvent(ctx, conversationID, senderID, senderRole, prepared.Kind, prepared.MimeType, scanSig, hash)
	}
	before := created.ID - 1
	msgs, err := s.db.listMessages(ctx, conversationID, &before, nil, 1)
	if err != nil || len(msgs) == 0 {
		return nil, err
	}
	return &attachmentResult{message: &msgs[0], kind: prepared.Kind}, nil
}

// themeForApp replica themeForApp.
func themeForApp(app string) map[string]any {
	switch normalizeApp(app) {
	case "bcp":
		return map[string]any{"app": "bcp", "primary": "#0646ad", "accent": "#ff7a00", "name": "BCP"}
	case "interbank":
		return map[string]any{"app": "interbank", "primary": "#00a94f", "accent": "#0070ba", "name": "Interbank"}
	}
	return map[string]any{"app": "yape", "primary": "#6f22b8", "accent": "#19c3b3", "name": "Yape"}
}

func randUploadName() string {
	b := make([]byte, 8)
	_, _ = rand.Read(b)
	return base64.RawURLEncoding.EncodeToString(b)
}
