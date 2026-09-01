package store

import (
	"context"
	"errors"

	"github.com/jackc/pgx/v5"
	"golang.org/x/crypto/bcrypt"
)

// AdminUser es un usuario del panel (login email + contraseña).
type AdminUser struct {
	Email  string `json:"email"`
	Nombre string `json:"nombre"`
	Rol    string `json:"rol"`
	hash   string
}

// SeedAdmin crea el admin inicial si no existe ninguno.
func (s *Store) SeedAdmin(ctx context.Context, email, password, nombre string) error {
	var n int
	_ = s.pool.QueryRow(ctx, `SELECT count(*) FROM admin_user`).Scan(&n)
	if n > 0 {
		return nil
	}
	hash, err := bcrypt.GenerateFromPassword([]byte(password), bcrypt.DefaultCost)
	if err != nil {
		return err
	}
	_, err = s.pool.Exec(ctx, `
		INSERT INTO admin_user (email, password_hash, nombre, rol)
		VALUES (lower($1),$2,$3,'admin') ON CONFLICT (email) DO NOTHING`,
		email, string(hash), nombre)
	return err
}

// CheckLogin valida email+contraseña y devuelve el usuario (sin hash).
func (s *Store) CheckLogin(ctx context.Context, email, password string) (AdminUser, error) {
	var u AdminUser
	err := s.pool.QueryRow(ctx, `
		SELECT email, COALESCE(nombre,''), COALESCE(rol,'admin'), password_hash
		FROM admin_user WHERE email=lower($1)`, email).Scan(&u.Email, &u.Nombre, &u.Rol, &u.hash)
	if errors.Is(err, pgx.ErrNoRows) {
		return AdminUser{}, errors.New("credenciales inválidas")
	}
	if err != nil {
		return AdminUser{}, err
	}
	if bcrypt.CompareHashAndPassword([]byte(u.hash), []byte(password)) != nil {
		return AdminUser{}, errors.New("credenciales inválidas")
	}
	return u, nil
}

// ChangePassword valida la contraseña actual y guarda la nueva.
func (s *Store) ChangePassword(ctx context.Context, email, actual, nueva string) error {
	if _, err := s.CheckLogin(ctx, email, actual); err != nil {
		return errors.New("la contraseña actual no es correcta")
	}
	if len(nueva) < 6 {
		return errors.New("la nueva contraseña debe tener al menos 6 caracteres")
	}
	hash, err := bcrypt.GenerateFromPassword([]byte(nueva), bcrypt.DefaultCost)
	if err != nil {
		return err
	}
	_, err = s.pool.Exec(ctx, `UPDATE admin_user SET password_hash=$1 WHERE email=lower($2)`, string(hash), email)
	return err
}

// GetAdmin devuelve los datos del admin por email (para el perfil).
func (s *Store) GetAdmin(ctx context.Context, email string) (AdminUser, error) {
	var u AdminUser
	err := s.pool.QueryRow(ctx, `
		SELECT email, COALESCE(nombre,''), COALESCE(rol,'admin'), ''
		FROM admin_user WHERE email=lower($1)`, email).Scan(&u.Email, &u.Nombre, &u.Rol, &u.hash)
	return u, err
}
