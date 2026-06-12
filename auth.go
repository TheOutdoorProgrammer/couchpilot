package main

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"golang.org/x/crypto/bcrypt"
)

const (
	sessionCookie = "couchpilot_session"
	tokenTTL      = 30 * 24 * time.Hour
)

// authFile is the on-disk shape of auth.json. It is kept separate from
// config.json (and out of the Config struct) so the password hash and signing
// secret never leak through GET /api/config.
type authFile struct {
	PasswordHash string `json:"passwordHash"`
	Secret       string `json:"secret"`
}

// AuthManager owns credential storage and cookie token signing. Whether auth is
// *enforced* is decided by Config.AuthEnabled; this type only knows how to set,
// check, and sign — never when to require.
type AuthManager struct {
	mu     sync.RWMutex
	path   string
	hash   string
	secret []byte
}

func NewAuthManager(dataDir string) (*AuthManager, error) {
	am := &AuthManager{path: filepath.Join(dataDir, "auth.json")}

	data, err := os.ReadFile(am.path)
	if err == nil {
		var f authFile
		if err := json.Unmarshal(data, &f); err != nil {
			return nil, err
		}
		am.hash = f.PasswordHash
		if f.Secret != "" {
			if s, err := base64.StdEncoding.DecodeString(f.Secret); err == nil {
				am.secret = s
			}
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}

	if len(am.secret) == 0 {
		am.secret = make([]byte, 32)
		if _, err := rand.Read(am.secret); err != nil {
			return nil, err
		}
		if err := am.saveLocked(); err != nil {
			return nil, err
		}
	}

	return am, nil
}

func (am *AuthManager) saveLocked() error {
	f := authFile{
		PasswordHash: am.hash,
		Secret:       base64.StdEncoding.EncodeToString(am.secret),
	}
	data, err := json.MarshalIndent(f, "", "  ")
	if err != nil {
		return err
	}
	os.MkdirAll(filepath.Dir(am.path), 0700)
	return os.WriteFile(am.path, data, 0600)
}

// HasPassword reports whether a password has been set. Auth enabled with no
// password means the UI must run first-time setup.
func (am *AuthManager) HasPassword() bool {
	am.mu.RLock()
	defer am.mu.RUnlock()
	return am.hash != ""
}

func (am *AuthManager) SetPassword(pw string) error {
	if len(pw) < 6 {
		return errors.New("password must be at least 6 characters")
	}
	hash, err := bcrypt.GenerateFromPassword([]byte(pw), bcrypt.DefaultCost)
	if err != nil {
		return err
	}
	am.mu.Lock()
	defer am.mu.Unlock()
	am.hash = string(hash)
	return am.saveLocked()
}

// ClearPassword removes the stored password and rotates the signing secret so
// every outstanding cookie is invalidated.
func (am *AuthManager) ClearPassword() error {
	am.mu.Lock()
	defer am.mu.Unlock()
	am.hash = ""
	if _, err := rand.Read(am.secret); err != nil {
		return err
	}
	return am.saveLocked()
}

func (am *AuthManager) CheckPassword(pw string) bool {
	am.mu.RLock()
	hash := am.hash
	am.mu.RUnlock()
	if hash == "" {
		return false
	}
	return bcrypt.CompareHashAndPassword([]byte(hash), []byte(pw)) == nil
}

func (am *AuthManager) issueToken(ttl time.Duration) string {
	payload := strconv.FormatInt(time.Now().Add(ttl).Unix(), 10)
	return payload + "." + am.sign(payload)
}

func (am *AuthManager) sign(payload string) string {
	am.mu.RLock()
	secret := am.secret
	am.mu.RUnlock()
	mac := hmac.New(sha256.New, secret)
	mac.Write([]byte(payload))
	return hex.EncodeToString(mac.Sum(nil))
}

func (am *AuthManager) validateToken(tok string) bool {
	i := strings.LastIndexByte(tok, '.')
	if i <= 0 {
		return false
	}
	payload, mac := tok[:i], tok[i+1:]
	expected := am.sign(payload)
	if subtle.ConstantTimeCompare([]byte(mac), []byte(expected)) != 1 {
		return false
	}
	exp, err := strconv.ParseInt(payload, 10, 64)
	if err != nil {
		return false
	}
	return time.Now().Unix() < exp
}

func (am *AuthManager) tokenCookie(value string, maxAge int) *http.Cookie {
	return &http.Cookie{
		Name:     sessionCookie,
		Value:    value,
		Path:     "/",
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
		MaxAge:   maxAge,
	}
}

// requestAuthed reports whether the request carries a valid session cookie.
func (am *AuthManager) requestAuthed(r *http.Request) bool {
	c, err := r.Cookie(sessionCookie)
	if err != nil || c.Value == "" {
		return false
	}
	return am.validateToken(c.Value)
}
