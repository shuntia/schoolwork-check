package classroom

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sync"
	"time"

	"golang.org/x/oauth2"
	"golang.org/x/oauth2/google"
	gclassroom "google.golang.org/api/classroom/v1"
	"google.golang.org/api/drive/v3"
	"google.golang.org/api/option"
)

// oauthScopes are the read-only, student-side scopes this adapter needs.
// Drive read access is required to export attached Google Docs and to
// download uploaded files (PDF/DOCX) for text extraction.
var oauthScopes = []string{
	"https://www.googleapis.com/auth/classroom.courses.readonly",
	"https://www.googleapis.com/auth/classroom.coursework.me.readonly",
	"https://www.googleapis.com/auth/classroom.student-submissions.me.readonly",
	"https://www.googleapis.com/auth/classroom.courseworkmaterials.readonly",
	// Added after the first logins: a token without it gets 403 on
	// announcements, which is logged and skipped until google-login is rerun.
	"https://www.googleapis.com/auth/classroom.announcements.readonly",
	"https://www.googleapis.com/auth/drive.readonly",
	// Added for GOOGLE_CALENDARS: a token minted before it gets 403 on
	// events.list, with a message saying to run google-login again.
	"https://www.googleapis.com/auth/calendar.events.readonly",
}

// loadOAuthConfig parses the "Desktop app" OAuth client JSON downloaded from
// the Google Cloud Console.
func loadOAuthConfig(credentialsFile string) (*oauth2.Config, error) {
	if credentialsFile == "" {
		return nil, errors.New("classroom: no OAuth credentials file configured (set GOOGLE_CREDENTIALS_FILE)")
	}
	data, err := os.ReadFile(credentialsFile)
	if err != nil {
		return nil, fmt.Errorf("classroom: reading OAuth credentials %s: %w "+
			"(download a \"Desktop app\" OAuth client JSON from the Google Cloud Console)", credentialsFile, err)
	}
	cfg, err := google.ConfigFromJSON(data, oauthScopes...)
	if err != nil {
		return nil, fmt.Errorf("classroom: parsing OAuth credentials %s: %w", credentialsFile, err)
	}
	return cfg, nil
}

// Login runs the one-time installed-app OAuth consent flow and caches the
// resulting token (including a refresh token) at tokenFile with 0600 perms.
//
// It starts a loopback HTTP server on a free 127.0.0.1 port, prints the
// consent URL to stderr, tries to open a browser, and waits for Google to
// redirect back with the authorization code. PKCE is used throughout.
func Login(ctx context.Context, credentialsFile, tokenFile string) error {
	cfg, err := loadOAuthConfig(credentialsFile)
	if err != nil {
		return err
	}
	if tokenFile == "" {
		return errors.New("classroom: no token file configured (set GOOGLE_TOKEN_FILE)")
	}

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return fmt.Errorf("classroom: starting loopback listener: %w", err)
	}
	defer ln.Close()
	cfg.RedirectURL = "http://127.0.0.1:" + fmt.Sprint(ln.Addr().(*net.TCPAddr).Port)

	state, err := randomToken()
	if err != nil {
		return err
	}
	verifier := oauth2.GenerateVerifier()

	type result struct {
		code string
		err  error
	}
	ch := make(chan result, 1)
	var once sync.Once
	send := func(r result) { once.Do(func() { ch <- r }) }

	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		switch {
		case q.Get("error") != "":
			err := fmt.Errorf("classroom: authorization denied: %s", q.Get("error"))
			fmt.Fprint(w, "<h1>Authorization failed</h1><p>You can close this tab.</p>")
			send(result{err: err})
		case q.Get("state") != state:
			fmt.Fprint(w, "<h1>Authorization failed</h1><p>State mismatch. You can close this tab.</p>")
			send(result{err: errors.New("classroom: OAuth state mismatch (possible CSRF); try again")})
		case q.Get("code") == "":
			// Favicon or a stray request: ignore.
			http.NotFound(w, r)
		default:
			fmt.Fprint(w, "<h1>schoolwork-check is authorized</h1><p>You can close this tab and return to the terminal.</p>")
			send(result{code: q.Get("code")})
		}
	})
	srv := &http.Server{Handler: mux, ReadHeaderTimeout: 10 * time.Second}
	go func() { _ = srv.Serve(ln) }()
	defer func() {
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutdownCtx)
	}()

	authURL := cfg.AuthCodeURL(state,
		oauth2.AccessTypeOffline,
		oauth2.ApprovalForce, // force a refresh token even on re-consent
		oauth2.S256ChallengeOption(verifier),
	)
	fmt.Fprintln(os.Stderr, "Open this URL in your browser to authorize schoolwork-check:")
	fmt.Fprintln(os.Stderr, "")
	fmt.Fprintln(os.Stderr, "  "+authURL)
	fmt.Fprintln(os.Stderr, "")
	openBrowser(authURL)

	var r result
	select {
	case <-ctx.Done():
		return ctx.Err()
	case r = <-ch:
	}
	if r.err != nil {
		return r.err
	}

	tok, err := cfg.Exchange(ctx, r.code, oauth2.VerifierOption(verifier))
	if err != nil {
		return fmt.Errorf("classroom: exchanging authorization code: %w", err)
	}
	if tok.RefreshToken == "" {
		fmt.Fprintln(os.Stderr, "classroom: warning: Google did not return a refresh token; "+
			"you may have to re-run google-login when the access token expires")
	}
	if err := saveToken(tokenFile, tok); err != nil {
		return err
	}
	fmt.Fprintf(os.Stderr, "classroom: token saved to %s\n", tokenFile)
	return nil
}

// HTTPClient returns an HTTP client that authenticates with the cached Google
// token, refreshing it and writing the refreshed token back to tokenFile. The
// scopes are the ones google-login consented to, Drive read included, so other
// Google-backed sources (internal/gdoc) share this one login instead of
// asking the user to consent twice.
func HTTPClient(ctx context.Context, credentialsFile, tokenFile string, log *slog.Logger) (*http.Client, error) {
	if log == nil {
		log = slog.Default()
	}
	cfg, err := loadOAuthConfig(credentialsFile)
	if err != nil {
		return nil, err
	}
	tok, err := loadToken(tokenFile)
	if err != nil {
		return nil, err
	}
	ts := &savingTokenSource{
		src:  cfg.TokenSource(ctx, tok),
		path: tokenFile,
		last: tok.AccessToken,
		log:  log,
	}
	return oauth2.NewClient(ctx, ts), nil
}

// New loads the cached token and builds Classroom and Drive clients from it.
// The underlying token source refreshes automatically and writes the refreshed
// token back to tokenFile.
func New(ctx context.Context, credentialsFile, tokenFile string, opts Options) (*Client, error) {
	log := opts.Logger
	if log == nil {
		log = slog.Default()
	}
	hc, err := HTTPClient(ctx, credentialsFile, tokenFile, log)
	if err != nil {
		return nil, err
	}

	cls, err := gclassroom.NewService(ctx, option.WithHTTPClient(hc))
	if err != nil {
		return nil, fmt.Errorf("classroom: building Classroom client: %w", err)
	}
	drv, err := drive.NewService(ctx, option.WithHTTPClient(hc))
	if err != nil {
		return nil, fmt.Errorf("classroom: building Drive client: %w", err)
	}
	opts.Logger = log
	return newWithServices(cls, drv, opts), nil
}

// loadToken reads the cached OAuth token, with an actionable error when it is
// missing or unusable.
func loadToken(path string) (*oauth2.Token, error) {
	if path == "" {
		return nil, errors.New("classroom: no token file configured (set GOOGLE_TOKEN_FILE)")
	}
	data, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, fmt.Errorf("classroom: no cached Google token at %s — run `schoolwork-check google-login` first", path)
		}
		return nil, fmt.Errorf("classroom: reading %s: %w — run `schoolwork-check google-login`", path, err)
	}
	var tok oauth2.Token
	if err := json.Unmarshal(data, &tok); err != nil {
		return nil, fmt.Errorf("classroom: %s is not a valid cached token (%v) — run `schoolwork-check google-login`", path, err)
	}
	if tok.RefreshToken == "" && !tok.Valid() {
		return nil, fmt.Errorf("classroom: cached token at %s is expired and has no refresh token — run `schoolwork-check google-login`", path)
	}
	return &tok, nil
}

// saveToken writes the token JSON with 0600 perms, creating the parent dir.
func saveToken(path string, tok *oauth2.Token) error {
	if dir := filepath.Dir(path); dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return fmt.Errorf("classroom: creating %s: %w", dir, err)
		}
	}
	data, err := json.MarshalIndent(tok, "", "  ")
	if err != nil {
		return fmt.Errorf("classroom: encoding token: %w", err)
	}
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return fmt.Errorf("classroom: writing %s: %w", path, err)
	}
	// Re-apply the mode in case the file already existed with wider perms.
	_ = f.Chmod(0o600)
	if _, err := f.Write(append(data, '\n')); err != nil {
		f.Close()
		return fmt.Errorf("classroom: writing %s: %w", path, err)
	}
	if err := f.Close(); err != nil {
		return fmt.Errorf("classroom: writing %s: %w", path, err)
	}
	return nil
}

// savingTokenSource persists refreshed tokens so a long-lived refresh token
// survives across runs and rotations.
type savingTokenSource struct {
	src  oauth2.TokenSource
	path string
	log  *slog.Logger

	mu   sync.Mutex
	last string // last access token written to disk
}

func (s *savingTokenSource) Token() (*oauth2.Token, error) {
	tok, err := s.src.Token()
	if err != nil {
		if ae := authError(err); ae != nil {
			return nil, ae
		}
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if tok.AccessToken != s.last {
		s.last = tok.AccessToken
		if err := saveToken(s.path, tok); err != nil {
			s.log.Warn("classroom: could not cache refreshed token", "path", s.path, "err", err)
		} else {
			s.log.Debug("classroom: cached refreshed token", "path", s.path)
		}
	}
	return tok, nil
}

func randomToken() (string, error) {
	b := make([]byte, 24)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("classroom: generating random state: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

// openBrowser makes a best-effort attempt to open the consent URL. Failures
// are ignored: the URL is already on stderr.
func openBrowser(url string) {
	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "darwin":
		cmd = exec.Command("open", url)
	case "windows":
		cmd = exec.Command("rundll32", "url.dll,FileProtocolHandler", url)
	default:
		cmd = exec.Command("xdg-open", url)
	}
	if err := cmd.Start(); err == nil {
		go func() { _ = cmd.Wait() }()
	}
}
