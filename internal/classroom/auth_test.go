package classroom

import (
	"errors"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"golang.org/x/oauth2"
	"google.golang.org/api/googleapi"
)

func TestLoadTokenErrors(t *testing.T) {
	dir := t.TempDir()

	_, err := loadToken(filepath.Join(dir, "missing.json"))
	if err == nil || !strings.Contains(err.Error(), "google-login") {
		t.Fatalf("missing token: %v", err)
	}

	bad := filepath.Join(dir, "bad.json")
	if err := os.WriteFile(bad, []byte("not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := loadToken(bad); err == nil || !strings.Contains(err.Error(), "google-login") {
		t.Fatalf("invalid token: %v", err)
	}

	if _, err := loadToken(""); err == nil {
		t.Fatal("empty path should error")
	}

	expired := filepath.Join(dir, "expired.json")
	if err := os.WriteFile(expired, []byte(`{"access_token":"a","expiry":"2020-01-01T00:00:00Z"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := loadToken(expired); err == nil || !strings.Contains(err.Error(), "google-login") {
		t.Fatalf("expired token without refresh token: %v", err)
	}
}

func TestSaveAndLoadToken(t *testing.T) {
	path := filepath.Join(t.TempDir(), "nested", "dir", "token.json")
	tok := &oauth2.Token{
		AccessToken:  "at",
		RefreshToken: "rt",
		TokenType:    "Bearer",
		Expiry:       time.Date(2030, 1, 1, 0, 0, 0, 0, time.UTC),
	}
	if err := saveToken(path, tok); err != nil {
		t.Fatal(err)
	}
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if perm := fi.Mode().Perm(); perm != 0o600 {
		t.Errorf("token file mode = %v, want 0600", perm)
	}
	got, err := loadToken(path)
	if err != nil {
		t.Fatal(err)
	}
	if got.AccessToken != "at" || got.RefreshToken != "rt" || !got.Expiry.Equal(tok.Expiry) {
		t.Errorf("round trip lost data: %+v", got)
	}
}

// stubSource hands out a fresh access token on every call.
type stubSource struct {
	n   int
	err error
}

func (s *stubSource) Token() (*oauth2.Token, error) {
	if s.err != nil {
		return nil, s.err
	}
	s.n++
	return &oauth2.Token{
		AccessToken:  "access-" + string(rune('a'+s.n-1)),
		RefreshToken: "rt",
		Expiry:       time.Now().Add(time.Hour),
	}, nil
}

func TestSavingTokenSourcePersistsRefreshedTokens(t *testing.T) {
	path := filepath.Join(t.TempDir(), "token.json")
	src := &stubSource{}
	ts := &savingTokenSource{
		src:  src,
		path: path,
		last: "access-a", // pretend this is what is already on disk
		log:  slog.New(slog.NewTextHandler(io.Discard, nil)),
	}

	// First call returns the token already on disk: nothing is written.
	if _, err := ts.Token(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("unchanged token should not have been written: %v", err)
	}

	// Second call rotates the access token: it must be persisted.
	tok, err := ts.Token()
	if err != nil {
		t.Fatal(err)
	}
	if tok.AccessToken != "access-b" {
		t.Fatalf("got %q", tok.AccessToken)
	}
	saved, err := loadToken(path)
	if err != nil {
		t.Fatal(err)
	}
	if saved.AccessToken != "access-b" || saved.RefreshToken != "rt" {
		t.Errorf("saved token = %+v", saved)
	}
}

func TestSavingTokenSourceTranslatesAuthErrors(t *testing.T) {
	ts := &savingTokenSource{
		src:  &stubSource{err: &oauth2.RetrieveError{ErrorCode: "invalid_grant"}},
		path: filepath.Join(t.TempDir(), "token.json"),
		log:  slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
	_, err := ts.Token()
	if !errors.Is(err, errTokenExpired) {
		t.Fatalf("got %v, want errTokenExpired", err)
	}
}

func TestLoadOAuthConfig(t *testing.T) {
	dir := t.TempDir()
	if _, err := loadOAuthConfig(""); err == nil {
		t.Fatal("empty path should error")
	}
	if _, err := loadOAuthConfig(filepath.Join(dir, "nope.json")); err == nil {
		t.Fatal("missing file should error")
	}

	path := filepath.Join(dir, "creds.json")
	creds := `{"installed":{"client_id":"cid.apps.googleusercontent.com","client_secret":"secret",` +
		`"auth_uri":"https://accounts.google.com/o/oauth2/auth","token_uri":"https://oauth2.googleapis.com/token",` +
		`"redirect_uris":["http://localhost"]}}`
	if err := os.WriteFile(path, []byte(creds), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := loadOAuthConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.ClientID != "cid.apps.googleusercontent.com" || cfg.ClientSecret != "secret" {
		t.Errorf("config = %+v", cfg)
	}
	if len(cfg.Scopes) != len(oauthScopes) {
		t.Errorf("scopes = %v", cfg.Scopes)
	}
	for _, s := range cfg.Scopes {
		if !strings.HasPrefix(s, "https://www.googleapis.com/auth/") {
			t.Errorf("scope %q is not a full URL", s)
		}
	}
}

func TestAuthError(t *testing.T) {
	if authError(nil) != nil {
		t.Error("nil is not an auth error")
	}
	if authError(errors.New("boom")) != nil {
		t.Error("a generic error is not an auth error")
	}
	if !errors.Is(authError(&googleapi.Error{Code: http.StatusUnauthorized}), errTokenExpired) {
		t.Error("401 should map to errTokenExpired")
	}
	if authError(&googleapi.Error{Code: http.StatusForbidden}) != nil {
		t.Error("403 is not an auth error")
	}
	if !errors.Is(authError(errors.New(`oauth2: "invalid_grant" token expired`)), errTokenExpired) {
		t.Error("invalid_grant should map to errTokenExpired")
	}
}

func TestRetryable(t *testing.T) {
	for _, tc := range []struct {
		code int
		want bool
	}{{429, true}, {500, true}, {503, true}, {400, false}, {403, false}, {404, false}} {
		if got := retryable(&googleapi.Error{Code: tc.code}); got != tc.want {
			t.Errorf("retryable(%d) = %v, want %v", tc.code, got, tc.want)
		}
	}
	if retryable(errors.New("boom")) {
		t.Error("a plain error is not retryable")
	}
}

func TestIsForbidden(t *testing.T) {
	if !isForbidden(&googleapi.Error{Code: 403}) || !isForbidden(&googleapi.Error{Code: 404}) {
		t.Error("403/404 should be forbidden")
	}
	if isForbidden(&googleapi.Error{Code: 500}) || isForbidden(errors.New("boom")) {
		t.Error("only 403/404 are forbidden")
	}
}
