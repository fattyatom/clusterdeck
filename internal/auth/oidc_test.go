package auth

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	jose "github.com/go-jose/go-jose/v4"
)

type fakeIdP struct {
	t         *testing.T
	srv       *httptest.Server
	key       *rsa.PrivateKey
	challenge string
	nonce     string
	claims    map[string]any
	audience  string
}

func newIdP(t *testing.T) *fakeIdP {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	f := &fakeIdP{t: t, key: key, audience: "clusterdeck"}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /.well-known/openid-configuration", func(w http.ResponseWriter, _ *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{
			"issuer":                                f.srv.URL,
			"authorization_endpoint":                f.srv.URL + "/authorize",
			"token_endpoint":                        f.srv.URL + "/token",
			"jwks_uri":                              f.srv.URL + "/jwks",
			"id_token_signing_alg_values_supported": []string{"RS256"},
		})
	})
	mux.HandleFunc("GET /jwks", func(w http.ResponseWriter, _ *http.Request) {
		json.NewEncoder(w).Encode(jose.JSONWebKeySet{Keys: []jose.JSONWebKey{{Key: &key.PublicKey, KeyID: "k1", Algorithm: "RS256", Use: "sig"}}})
	})
	mux.HandleFunc("POST /token", func(w http.ResponseWriter, r *http.Request) {
		r.ParseForm()
		sum := sha256.Sum256([]byte(r.PostForm.Get("code_verifier")))
		if r.PostForm.Get("code") != "good-code" || base64.RawURLEncoding.EncodeToString(sum[:]) != f.challenge {
			http.Error(w, `{"error":"invalid_grant"}`, http.StatusBadRequest)
			return
		}
		user, pass, _ := r.BasicAuth()
		if user != "clusterdeck" || pass != "s3cret" {
			http.Error(w, `{"error":"invalid_client"}`, http.StatusUnauthorized)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{"access_token": "at", "token_type": "Bearer", "id_token": f.sign()})
	})
	f.srv = httptest.NewServer(mux)
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeIdP) sign() string {
	signer, err := jose.NewSigner(jose.SigningKey{Algorithm: jose.RS256, Key: f.key}, (&jose.SignerOptions{}).WithHeader("kid", "k1"))
	if err != nil {
		f.t.Fatal(err)
	}
	claims := map[string]any{
		"iss": f.srv.URL, "aud": f.audience, "sub": "123", "nonce": f.nonce,
		"iat": time.Now().Unix(), "exp": time.Now().Add(time.Hour).Unix(),
		"email": "casey@example.com", "email_verified": true,
	}
	for k, v := range f.claims {
		if v == nil {
			delete(claims, k)
		} else {
			claims[k] = v
		}
	}
	payload, _ := json.Marshal(claims)
	sig, err := signer.Sign(payload)
	if err != nil {
		f.t.Fatal(err)
	}
	out, _ := sig.CompactSerialize()
	return out
}

func newOIDC(t *testing.T, idp *fakeIdP, mut func(*Config)) (*OIDC, *http.ServeMux) {
	cfg := Config{
		IssuerURL: idp.srv.URL, ClientID: "clusterdeck", ClientSecret: "s3cret",
		RedirectURL: "https://deck.example.com/auth/callback",
		Logger:      slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
	if mut != nil {
		mut(&cfg)
	}
	o, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	o.Routes(mux)
	return o, mux
}

func cookie(t *testing.T, rec *httptest.ResponseRecorder, name string) *http.Cookie {
	for _, c := range rec.Result().Cookies() {
		if c.Name == name {
			return c
		}
	}
	t.Fatalf("no %s cookie in %v", name, rec.Result().Cookies())
	return nil
}

func startLogin(t *testing.T, idp *fakeIdP, mux *http.ServeMux, next string) (*http.Cookie, string) {
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest("GET", "/auth/login?next="+url.QueryEscape(next), nil))
	if rec.Code != http.StatusFound {
		t.Fatalf("login: %d %s", rec.Code, rec.Body)
	}
	loc, _ := url.Parse(rec.Header().Get("Location"))
	q := loc.Query()
	if !strings.HasPrefix(loc.String(), idp.srv.URL+"/authorize") || q.Get("client_id") != "clusterdeck" ||
		q.Get("redirect_uri") != "https://deck.example.com/auth/callback" || q.Get("code_challenge_method") != "S256" ||
		!strings.Contains(q.Get("scope"), "openid") {
		t.Fatalf("authorize URL: %s", loc)
	}
	idp.challenge, idp.nonce = q.Get("code_challenge"), q.Get("nonce")
	c := cookie(t, rec, "__Host-clusterdeck_login")
	if !c.Secure || !c.HttpOnly || c.SameSite != http.SameSiteLaxMode || c.Path != "/" {
		t.Fatalf("login cookie attributes: %+v", c)
	}
	return c, q.Get("state")
}

func callback(mux *http.ServeMux, login *http.Cookie, query string) *httptest.ResponseRecorder {
	req := httptest.NewRequest("GET", "/auth/callback?"+query, nil)
	if login != nil {
		req.AddCookie(login)
	}
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	return rec
}

func TestSignInFlow(t *testing.T) {
	idp := newIdP(t)
	o, mux := newOIDC(t, idp, nil)
	login, state := startLogin(t, idp, mux, "/apps?x=1")

	rec := callback(mux, login, "code=good-code&state="+state)
	if rec.Code != http.StatusFound || rec.Header().Get("Location") != "/apps?x=1" {
		t.Fatalf("callback: %d %s %s", rec.Code, rec.Header().Get("Location"), rec.Body)
	}
	session := cookie(t, rec, "__Host-clusterdeck_session")
	if !session.Secure || !session.HttpOnly {
		t.Fatalf("session cookie attributes: %+v", session)
	}
	if cookie(t, rec, "__Host-clusterdeck_login").MaxAge >= 0 {
		t.Fatal("the login cookie is not cleared")
	}

	req := httptest.NewRequest("GET", "/api/config", nil)
	req.AddCookie(session)
	if got := o.User(req); got != "casey@example.com" {
		t.Fatalf("User = %q", got)
	}

}

func TestCallbackRefusals(t *testing.T) {
	cases := []struct {
		name   string
		claims map[string]any
		query  func(state string) string
		noCook bool
		status int
	}{
		{name: "no login cookie", noCook: true, query: func(s string) string { return "code=good-code&state=" + s }, status: 400},
		{name: "wrong state", query: func(string) string { return "code=good-code&state=other" }, status: 400},
		{name: "idp error", query: func(s string) string { return "error=access_denied&state=" + s }, status: 403},
		{name: "bad code", query: func(s string) string { return "code=bad&state=" + s }, status: 502},
		{name: "wrong nonce", claims: map[string]any{"nonce": "forged"}, query: func(s string) string { return "code=good-code&state=" + s }, status: 403},
		{name: "wrong audience", claims: map[string]any{"aud": "someone-else"}, query: func(s string) string { return "code=good-code&state=" + s }, status: 403},
		{name: "expired token", claims: map[string]any{"exp": time.Now().Add(-time.Hour).Unix()}, query: func(s string) string { return "code=good-code&state=" + s }, status: 403},
		{name: "unverified email", claims: map[string]any{"email_verified": false}, query: func(s string) string { return "code=good-code&state=" + s }, status: 403},
		{name: "no email", claims: map[string]any{"email": nil}, query: func(s string) string { return "code=good-code&state=" + s }, status: 403},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			idp := newIdP(t)
			_, mux := newOIDC(t, idp, nil)
			login, state := startLogin(t, idp, mux, "/")
			idp.claims = tc.claims
			if tc.noCook {
				login = nil
			}
			rec := callback(mux, login, tc.query(state))
			if rec.Code != tc.status {
				t.Fatalf("status %d, want %d: %s", rec.Code, tc.status, rec.Body)
			}
			for _, c := range rec.Result().Cookies() {
				if c.Name == "__Host-clusterdeck_session" && c.MaxAge > 0 {
					t.Fatal("a session cookie was set")
				}
			}
		})
	}
}

func TestUserClaim(t *testing.T) {
	idp := newIdP(t)
	idp.claims = map[string]any{"preferred_username": "casey", "email_verified": false}
	o, mux := newOIDC(t, idp, func(c *Config) { c.UserClaim = "preferred_username" })
	login, state := startLogin(t, idp, mux, "/")
	rec := callback(mux, login, "code=good-code&state="+state)
	req := httptest.NewRequest("GET", "/", nil)
	req.AddCookie(cookie(t, rec, "__Host-clusterdeck_session"))
	if got := o.User(req); got != "casey" {
		t.Fatalf("User = %q", got)
	}
}

func TestSessionCookie(t *testing.T) {
	idp := newIdP(t)
	now := time.Unix(1_800_000_000, 0)
	o, _ := newOIDC(t, idp, func(c *Config) { c.Now = func() time.Time { return now }; c.SessionTTL = time.Hour })
	other, _ := newOIDC(t, idp, nil)
	valid := o.seal(map[string]any{"u": "casey", "e": now.Add(time.Hour).Unix()})
	body, _, _ := strings.Cut(valid, ".")

	for name, tc := range map[string]struct {
		value string
		want  string
	}{
		"valid":           {valid, "casey"},
		"expired":         {o.seal(map[string]any{"u": "casey", "e": now.Unix()}), ""},
		"tampered body":   {base64.RawURLEncoding.EncodeToString([]byte(`{"u":"admin","e":9999999999}`)) + "." + strings.SplitN(valid, ".", 2)[1], ""},
		"no signature":    {body, ""},
		"other key":       {other.seal(map[string]any{"u": "casey", "e": now.Add(time.Hour).Unix()}), ""},
		"empty user":      {o.seal(map[string]any{"u": "", "e": now.Add(time.Hour).Unix()}), ""},
		"garbage":         {"%%%.%%%", ""},
		"login as a user": {o.seal(loginState{State: "x", Expires: now.Add(time.Hour).Unix()}), ""},
	} {
		req := httptest.NewRequest("GET", "/", nil)
		req.AddCookie(&http.Cookie{Name: "__Host-clusterdeck_session", Value: tc.value})
		if got := o.User(req); got != tc.want {
			t.Errorf("%s: User = %q, want %q", name, got, tc.want)
		}
	}
	if o.User(httptest.NewRequest("GET", "/", nil)) != "" {
		t.Error("no cookie gives a user")
	}
}

func TestSharedSessionSecret(t *testing.T) {
	idp := newIdP(t)
	secret := []byte(strings.Repeat("k", 32))
	a, _ := newOIDC(t, idp, func(c *Config) { c.SessionSecret = secret })
	b, _ := newOIDC(t, idp, func(c *Config) { c.SessionSecret = secret })
	req := httptest.NewRequest("GET", "/", nil)
	req.AddCookie(&http.Cookie{Name: "__Host-clusterdeck_session", Value: a.seal(map[string]any{"u": "casey", "e": time.Now().Add(time.Hour).Unix()})})
	if b.User(req) != "casey" {
		t.Fatal("a session from one replica is refused by another with the same secret")
	}
}

func TestLogout(t *testing.T) {
	idp := newIdP(t)
	_, mux := newOIDC(t, idp, nil)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest("GET", "/auth/logout", nil))
	if rec.Code != 200 || cookie(t, rec, "__Host-clusterdeck_session").MaxAge >= 0 || !strings.Contains(rec.Body.String(), "/auth/login") {
		t.Fatalf("logout: %d %v %s", rec.Code, rec.Result().Cookies(), rec.Body)
	}
}

func TestPlainHTTPCookieNames(t *testing.T) {
	idp := newIdP(t)
	_, mux := newOIDC(t, idp, func(c *Config) { c.RedirectURL = "http://localhost:8080/auth/callback" })
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest("GET", "/auth/login", nil))
	if c := cookie(t, rec, "clusterdeck_login"); c.Secure {
		t.Fatal("a plain-http install sets a Secure cookie")
	}
}

func TestUnreachableIssuer(t *testing.T) {
	o, err := New(Config{IssuerURL: "http://127.0.0.1:1", ClientID: "c", ClientSecret: "s", RedirectURL: "https://d.example.com/auth/callback", Logger: slog.New(slog.NewTextHandler(io.Discard, nil))})
	if err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	o.Routes(mux)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest("GET", "/auth/login", nil))
	if rec.Code != http.StatusBadGateway {
		t.Fatalf("status %d", rec.Code)
	}
}

func TestNewValidates(t *testing.T) {
	base := Config{IssuerURL: "https://id.example.com", ClientID: "c", ClientSecret: "s", RedirectURL: "https://d.example.com/auth/callback"}
	for name, mut := range map[string]func(*Config){
		"no issuer":         func(c *Config) { c.IssuerURL = "" },
		"no secret":         func(c *Config) { c.ClientSecret = "" },
		"relative redirect": func(c *Config) { c.RedirectURL = "/auth/callback" },
		"wrong path":        func(c *Config) { c.RedirectURL = "https://d.example.com/callback" },
		"short key":         func(c *Config) { c.SessionSecret = []byte("short") },
	} {
		c := base
		mut(&c)
		if _, err := New(c); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	if _, err := New(base); err != nil {
		t.Fatal(err)
	}
}

func TestSafeNext(t *testing.T) {
	for in, want := range map[string]string{
		"":                       "/",
		"/apps":                  "/apps",
		"/#/apps/x":              "/#/apps/x",
		"//evil.example.com":     "/",
		"https://evil.example":   "/",
		"/\\evil.example.com":    "/",
		"/auth/login":            "/",
		"apps":                   "/",
		"/ok\r\nSet-Cookie: x=y": "/",
	} {
		if got := safeNext(in); got != want {
			t.Errorf("safeNext(%q) = %q, want %q", in, got, want)
		}
	}
	o := &OIDC{}
	if got := o.LoginURL("//evil"); got != "/auth/login?next=%2F" {
		t.Errorf("LoginURL = %q", got)
	}
}
