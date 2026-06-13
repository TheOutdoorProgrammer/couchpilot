package main

import (
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

func newTestAuth(t *testing.T) *AuthManager {
	t.Helper()
	am, err := NewAuthManager(t.TempDir())
	if err != nil {
		t.Fatalf("NewAuthManager: %v", err)
	}
	return am
}

func TestAuthManagerGeneratesAndPersistsSecret(t *testing.T) {
	dir := t.TempDir()
	am, err := NewAuthManager(dir)
	if err != nil {
		t.Fatalf("NewAuthManager: %v", err)
	}
	if len(am.secret) != 32 {
		t.Fatalf("want 32-byte secret, got %d", len(am.secret))
	}

	// auth.json must be created with restrictive permissions — it holds the
	// signing secret and password hash.
	info, err := os.Stat(filepath.Join(dir, "auth.json"))
	if err != nil {
		t.Fatalf("stat auth.json: %v", err)
	}
	if perm := info.Mode().Perm(); perm != 0600 {
		t.Errorf("auth.json perms = %o, want 0600", perm)
	}

	// Reopening the same dir must recover the identical secret, or every
	// outstanding cookie would be invalidated on restart.
	am2, err := NewAuthManager(dir)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	if string(am2.secret) != string(am.secret) {
		t.Error("secret not stable across reload")
	}
}

func TestSetPasswordRejectsShort(t *testing.T) {
	am := newTestAuth(t)
	if err := am.SetPassword("12345"); err == nil {
		t.Error("want error for <6 char password")
	}
	if am.HasPassword() {
		t.Error("password should not be set after rejected SetPassword")
	}
}

func TestPasswordRoundTrip(t *testing.T) {
	am := newTestAuth(t)
	if am.HasPassword() {
		t.Fatal("fresh manager should have no password")
	}
	if am.CheckPassword("anything") {
		t.Error("CheckPassword must be false with no password set")
	}
	if err := am.SetPassword("hunter2!"); err != nil {
		t.Fatalf("SetPassword: %v", err)
	}
	if !am.HasPassword() {
		t.Error("HasPassword should be true after set")
	}
	if !am.CheckPassword("hunter2!") {
		t.Error("correct password rejected")
	}
	if am.CheckPassword("wrong") {
		t.Error("wrong password accepted")
	}
}

func TestPasswordPersistsAcrossReload(t *testing.T) {
	dir := t.TempDir()
	am, _ := NewAuthManager(dir)
	if err := am.SetPassword("correct horse"); err != nil {
		t.Fatal(err)
	}
	am2, err := NewAuthManager(dir)
	if err != nil {
		t.Fatal(err)
	}
	if !am2.CheckPassword("correct horse") {
		t.Error("password did not survive reload")
	}
}

func TestClearPasswordRotatesSecret(t *testing.T) {
	am := newTestAuth(t)
	if err := am.SetPassword("password1"); err != nil {
		t.Fatal(err)
	}
	tok := am.issueToken(time.Hour)
	if !am.validateToken(tok) {
		t.Fatal("freshly issued token should validate")
	}

	if err := am.ClearPassword(); err != nil {
		t.Fatalf("ClearPassword: %v", err)
	}
	if am.HasPassword() {
		t.Error("password should be cleared")
	}
	// Secret rotation must invalidate every previously issued cookie.
	if am.validateToken(tok) {
		t.Error("token still valid after ClearPassword rotated the secret")
	}
}

func TestTokenRoundTrip(t *testing.T) {
	am := newTestAuth(t)
	tok := am.issueToken(time.Hour)
	if !am.validateToken(tok) {
		t.Error("valid token rejected")
	}
}

func TestValidateTokenRejectsTampered(t *testing.T) {
	am := newTestAuth(t)
	tok := am.issueToken(time.Hour)

	cases := map[string]string{
		"empty":           "",
		"no dot":          "abcdef",
		"trailing dot":    "123.",
		"leading dot":     ".abc",
		"flipped mac":     flipLastByte(tok),
		"non-numeric exp": "notanumber." + am.sign("notanumber") + "x",
	}
	for name, bad := range cases {
		if am.validateToken(bad) {
			t.Errorf("%s: tampered token validated", name)
		}
	}
}

func TestValidateTokenRejectsExpired(t *testing.T) {
	am := newTestAuth(t)
	// Issue a token that expired an hour ago; the MAC is valid but the
	// expiry is in the past.
	expired := am.issueToken(-time.Hour)
	if am.validateToken(expired) {
		t.Error("expired token validated")
	}
}

func TestValidateTokenRejectsForeignSecret(t *testing.T) {
	am1 := newTestAuth(t)
	am2 := newTestAuth(t)
	tok := am1.issueToken(time.Hour)
	if am2.validateToken(tok) {
		t.Error("token signed by another manager's secret validated")
	}
}

func TestRequestAuthed(t *testing.T) {
	am := newTestAuth(t)
	good := am.issueToken(time.Hour)

	tests := []struct {
		name   string
		cookie *http.Cookie
		want   bool
	}{
		{"no cookie", nil, false},
		{"empty value", &http.Cookie{Name: sessionCookie, Value: ""}, false},
		{"valid", &http.Cookie{Name: sessionCookie, Value: good}, true},
		{"garbage", &http.Cookie{Name: sessionCookie, Value: "x.y"}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r, _ := http.NewRequest("GET", "/", nil)
			if tt.cookie != nil {
				r.AddCookie(tt.cookie)
			}
			if got := am.requestAuthed(r); got != tt.want {
				t.Errorf("requestAuthed = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestTokenCookieAttributes(t *testing.T) {
	am := newTestAuth(t)
	c := am.tokenCookie("val", 3600)
	if c.Name != sessionCookie || c.Value != "val" {
		t.Errorf("name/value wrong: %s=%s", c.Name, c.Value)
	}
	if !c.HttpOnly {
		t.Error("cookie must be HttpOnly")
	}
	if c.SameSite != http.SameSiteLaxMode {
		t.Error("cookie must be SameSite=Lax")
	}
	if c.Path != "/" {
		t.Errorf("path = %q, want /", c.Path)
	}
	if c.MaxAge != 3600 {
		t.Errorf("maxAge = %d, want 3600", c.MaxAge)
	}
}

func TestIssueTokenEncodesExpiry(t *testing.T) {
	am := newTestAuth(t)
	tok := am.issueToken(2 * time.Hour)
	payload := tok[:strings.LastIndexByte(tok, '.')]
	exp, err := strconv.ParseInt(payload, 10, 64)
	if err != nil {
		t.Fatalf("payload not a unix ts: %v", err)
	}
	delta := exp - time.Now().Unix()
	if delta < 7100 || delta > 7300 {
		t.Errorf("expiry delta = %ds, want ~7200s", delta)
	}
}

func flipLastByte(s string) string {
	if s == "" {
		return s
	}
	b := []byte(s)
	if b[len(b)-1] == 'a' {
		b[len(b)-1] = 'b'
	} else {
		b[len(b)-1] = 'a'
	}
	return string(b)
}
