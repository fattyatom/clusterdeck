package auth

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"html/template"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/coreos/go-oidc/v3/oidc"
	"golang.org/x/oauth2"
)

type Config struct {
	IssuerURL     string
	ClientID      string
	ClientSecret  string
	RedirectURL   string
	UserClaim     string
	Scopes        []string
	SessionSecret []byte
	SessionTTL    time.Duration
	HTTPClient    *http.Client
	Logger        *slog.Logger
	Now           func() time.Time
}

type OIDC struct {
	cfg     Config
	key     []byte
	secure  bool
	session string
	login   string

	mu       sync.Mutex
	oauth    *oauth2.Config
	verifier *oidc.IDTokenVerifier
}

const (
	loginTTL     = 10 * time.Minute
	LoginPath    = "/auth/login"
	CallbackPath = "/auth/callback"
	LogoutPath   = "/auth/logout"
)

func New(cfg Config) (*OIDC, error) {
	if cfg.IssuerURL == "" || cfg.ClientID == "" || cfg.ClientSecret == "" || cfg.RedirectURL == "" {
		return nil, errors.New("OIDC needs an issuer URL, client ID, client secret and redirect URL")
	}
	redirect, err := url.Parse(cfg.RedirectURL)
	if err != nil || redirect.Host == "" || (redirect.Scheme != "https" && redirect.Scheme != "http") {
		return nil, fmt.Errorf("OIDC redirect URL %q must be an absolute http(s) URL", cfg.RedirectURL)
	}
	if redirect.Path != CallbackPath {
		return nil, fmt.Errorf("OIDC redirect URL must end in %s, got %q", CallbackPath, redirect.Path)
	}
	if cfg.UserClaim == "" {
		cfg.UserClaim = "email"
	}
	if len(cfg.Scopes) == 0 {
		cfg.Scopes = []string{oidc.ScopeOpenID, "email", "profile"}
	}
	if cfg.SessionTTL <= 0 {
		cfg.SessionTTL = 12 * time.Hour
	}
	if cfg.HTTPClient == nil {
		cfg.HTTPClient = &http.Client{Timeout: 15 * time.Second}
	}
	if cfg.Logger == nil {
		cfg.Logger = slog.Default()
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	key := cfg.SessionSecret
	if len(key) == 0 {
		key = make([]byte, 32)
		if _, err := rand.Read(key); err != nil {
			return nil, err
		}
	} else if len(key) < 32 {
		return nil, errors.New("the session secret must be at least 32 bytes")
	}
	o := &OIDC{cfg: cfg, key: key, secure: redirect.Scheme == "https"}
	o.session, o.login = "clusterdeck_session", "clusterdeck_login"
	if o.secure {
		o.session, o.login = "__Host-"+o.session, "__Host-"+o.login
	}
	return o, nil
}

func (o *OIDC) Routes(mux *http.ServeMux) {
	mux.HandleFunc("GET "+LoginPath, o.handleLogin)
	mux.HandleFunc("GET "+CallbackPath, o.handleCallback)
	mux.HandleFunc("GET "+LogoutPath, o.handleLogout)
}

func (o *OIDC) User(r *http.Request) string {
	c, err := r.Cookie(o.session)
	if err != nil {
		return ""
	}
	var s struct {
		User    string `json:"u"`
		Expires int64  `json:"e"`
	}
	if !o.open(c.Value, &s) || s.User == "" || o.cfg.Now().Unix() >= s.Expires {
		return ""
	}
	return s.User
}

func (o *OIDC) LoginURL(next string) string {
	return LoginPath + "?next=" + url.QueryEscape(safeNext(next))
}

type loginState struct {
	State    string `json:"s"`
	Nonce    string `json:"n"`
	Verifier string `json:"v"`
	Next     string `json:"r"`
	Expires  int64  `json:"e"`
}

func (o *OIDC) handleLogin(w http.ResponseWriter, r *http.Request) {
	oauth, _, err := o.provider(r.Context())
	if err != nil {
		o.cfg.Logger.Error("oidc discovery failed", "issuer", o.cfg.IssuerURL, "error", err.Error())
		http.Error(w, "the identity provider is unreachable", http.StatusBadGateway)
		return
	}
	st := loginState{
		State:    random(),
		Nonce:    random(),
		Verifier: oauth2.GenerateVerifier(),
		Next:     safeNext(r.URL.Query().Get("next")),
		Expires:  o.cfg.Now().Add(loginTTL).Unix(),
	}
	o.setCookie(w, o.login, o.seal(st), loginTTL)
	http.Redirect(w, r, oauth.AuthCodeURL(st.State, oidc.Nonce(st.Nonce), oauth2.S256ChallengeOption(st.Verifier)), http.StatusFound)
}

func (o *OIDC) handleCallback(w http.ResponseWriter, r *http.Request) {
	c, err := r.Cookie(o.login)
	var st loginState
	if err != nil || !o.open(c.Value, &st) || o.cfg.Now().Unix() >= st.Expires {
		o.fail(w, http.StatusBadRequest, "the sign-in expired or was started in another browser", nil)
		return
	}
	o.clearCookie(w, o.login)
	q := r.URL.Query()
	if e := q.Get("error"); e != "" {
		o.fail(w, http.StatusForbidden, "the identity provider refused the sign-in: "+e, nil)
		return
	}
	if q.Get("state") == "" || !hmac.Equal([]byte(q.Get("state")), []byte(st.State)) {
		o.fail(w, http.StatusBadRequest, "the sign-in state does not match", nil)
		return
	}
	oauth, verifier, err := o.provider(r.Context())
	if err != nil {
		o.fail(w, http.StatusBadGateway, "the identity provider is unreachable", err)
		return
	}
	ctx := oidc.ClientContext(r.Context(), o.cfg.HTTPClient)
	token, err := oauth.Exchange(ctx, q.Get("code"), oauth2.VerifierOption(st.Verifier))
	if err != nil {
		o.fail(w, http.StatusBadGateway, "the code exchange failed", err)
		return
	}
	raw, _ := token.Extra("id_token").(string)
	if raw == "" {
		o.fail(w, http.StatusBadGateway, "the identity provider returned no ID token", nil)
		return
	}
	idToken, err := verifier.Verify(ctx, raw)
	if err != nil {
		o.fail(w, http.StatusForbidden, "the ID token is not valid", err)
		return
	}
	if !hmac.Equal([]byte(idToken.Nonce), []byte(st.Nonce)) {
		o.fail(w, http.StatusForbidden, "the ID token nonce does not match", nil)
		return
	}
	var claims map[string]any
	if err := idToken.Claims(&claims); err != nil {
		o.fail(w, http.StatusForbidden, "the ID token claims are unreadable", err)
		return
	}
	if v, ok := claims["email_verified"].(bool); ok && !v && o.cfg.UserClaim == "email" {
		o.fail(w, http.StatusForbidden, "the email address is not verified", nil)
		return
	}
	user, _ := claims[o.cfg.UserClaim].(string)
	if user = strings.TrimSpace(user); user == "" {
		o.fail(w, http.StatusForbidden, "the ID token has no "+o.cfg.UserClaim+" claim", nil)
		return
	}
	session := struct {
		User    string `json:"u"`
		Expires int64  `json:"e"`
	}{user, o.cfg.Now().Add(o.cfg.SessionTTL).Unix()}
	o.setCookie(w, o.session, o.seal(session), o.cfg.SessionTTL)
	o.cfg.Logger.Info("signed in", "user", user)
	http.Redirect(w, r, st.Next, http.StatusFound)
}

var signedOut = template.Must(template.New("out").Parse(`<!doctype html><meta charset="utf-8"><title>Signed out</title><p>Signed out of clusterdeck. <a href="{{.}}">Sign in again</a></p>`))

func (o *OIDC) handleLogout(w http.ResponseWriter, r *http.Request) {
	o.clearCookie(w, o.session)
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	signedOut.Execute(w, LoginPath)
}

func (o *OIDC) provider(ctx context.Context) (*oauth2.Config, *oidc.IDTokenVerifier, error) {
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.oauth != nil {
		return o.oauth, o.verifier, nil
	}
	p, err := oidc.NewProvider(oidc.ClientContext(context.WithoutCancel(ctx), o.cfg.HTTPClient), o.cfg.IssuerURL)
	if err != nil {
		return nil, nil, err
	}
	o.oauth = &oauth2.Config{
		ClientID:     o.cfg.ClientID,
		ClientSecret: o.cfg.ClientSecret,
		RedirectURL:  o.cfg.RedirectURL,
		Endpoint:     p.Endpoint(),
		Scopes:       o.cfg.Scopes,
	}
	o.verifier = p.Verifier(&oidc.Config{ClientID: o.cfg.ClientID, Now: o.cfg.Now})
	return o.oauth, o.verifier, nil
}

func (o *OIDC) fail(w http.ResponseWriter, status int, msg string, err error) {
	attrs := []any{"reason", msg}
	if err != nil {
		attrs = append(attrs, "error", err.Error())
	}
	o.cfg.Logger.Warn("sign-in failed", attrs...)
	w.Header().Set("Cache-Control", "no-store")
	http.Error(w, "Sign-in failed: "+msg+". Try again at "+LoginPath, status)
}

func (o *OIDC) setCookie(w http.ResponseWriter, name, value string, ttl time.Duration) {
	http.SetCookie(w, &http.Cookie{
		Name: name, Value: value, Path: "/", MaxAge: int(ttl.Seconds()),
		Secure: o.secure, HttpOnly: true, SameSite: http.SameSiteLaxMode,
	})
}

func (o *OIDC) clearCookie(w http.ResponseWriter, name string) {
	http.SetCookie(w, &http.Cookie{Name: name, Value: "", Path: "/", MaxAge: -1, Secure: o.secure, HttpOnly: true, SameSite: http.SameSiteLaxMode})
}

func (o *OIDC) seal(v any) string {
	payload, _ := json.Marshal(v)
	body := base64.RawURLEncoding.EncodeToString(payload)
	return body + "." + base64.RawURLEncoding.EncodeToString(o.mac(body))
}

func (o *OIDC) open(value string, v any) bool {
	body, sig, ok := strings.Cut(value, ".")
	if !ok {
		return false
	}
	got, err := base64.RawURLEncoding.DecodeString(sig)
	if err != nil || !hmac.Equal(got, o.mac(body)) {
		return false
	}
	payload, err := base64.RawURLEncoding.DecodeString(body)
	return err == nil && json.Unmarshal(payload, v) == nil
}

func (o *OIDC) mac(body string) []byte {
	h := hmac.New(sha256.New, o.key)
	h.Write([]byte(body))
	return h.Sum(nil)
}

func random() string {
	b := make([]byte, 24)
	rand.Read(b)
	return base64.RawURLEncoding.EncodeToString(b)
}

func safeNext(next string) string {
	if next == "" || !strings.HasPrefix(next, "/") || strings.HasPrefix(next, "//") || strings.ContainsAny(next, "\\\r\n") || strings.HasPrefix(next, "/auth/") {
		return "/"
	}
	return next
}
