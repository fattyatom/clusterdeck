package gitops

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/go-jose/go-jose/v4"
	"github.com/go-jose/go-jose/v4/jwt"
)

type fakeApp struct {
	mu      sync.Mutex
	key     *rsa.PrivateKey
	now     time.Time
	mints   int
	lookups int
	missing bool
	repos   []string
}

func (f *fakeApp) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	raw := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
	parsed, err := jwt.ParseSigned(raw, []jose.SignatureAlgorithm{jose.RS256})
	if err != nil {
		http.Error(w, "bad jwt", http.StatusUnauthorized)
		return
	}
	var claims jwt.Claims
	if err := parsed.Claims(&f.key.PublicKey, &claims); err != nil || claims.Issuer != "1234" {
		http.Error(w, "bad claims", http.StatusUnauthorized)
		return
	}
	switch {
	case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/installation"):
		f.lookups++
		if f.missing {
			http.NotFound(w, r)
			return
		}
		json.NewEncoder(w).Encode(map[string]any{"id": 77})
	case r.Method == http.MethodPost && r.URL.Path == "/app/installations/77/access_tokens":
		f.mints++
		var body struct {
			Repositories []string `json:"repositories"`
		}
		json.NewDecoder(r.Body).Decode(&body)
		f.repos = body.Repositories
		json.NewEncoder(w).Encode(map[string]any{
			"token":      "ghs_" + strings.Join(body.Repositories, ""),
			"expires_at": f.now.Add(time.Hour).UTC().Format(time.RFC3339),
		})
	default:
		http.NotFound(w, r)
	}
}

func newApp(t *testing.T) (*AppAuth, *fakeApp, *time.Time) {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	f := &fakeApp{key: key, now: now}
	srv := httptest.NewServer(f)
	t.Cleanup(srv.Close)
	clock := &now
	a := &AppAuth{API: srv.URL, AppID: "1234", Key: key, Now: func() time.Time { f.now = *clock; return *clock }}
	return a, f, clock
}

func TestAppTokenIsMintedPerRepoAndScoped(t *testing.T) {
	a, f, _ := newApp(t)
	tok, err := a.Token(context.Background(), "fattyatom", "homelab")
	if err != nil || tok != "ghs_homelab" {
		t.Fatalf("token = %q, %v", tok, err)
	}
	if len(f.repos) != 1 || f.repos[0] != "homelab" {
		t.Fatalf("token must be scoped to one repository, got %v", f.repos)
	}
	if other, _ := a.Token(context.Background(), "fattyatom", "other"); other != "ghs_other" {
		t.Fatalf("second repo token = %q", other)
	}
	if f.mints != 2 {
		t.Fatalf("mints = %d, want one per repository", f.mints)
	}
}

func TestAppTokenIsCachedUntilTheRefreshMargin(t *testing.T) {
	a, f, clock := newApp(t)
	ctx := context.Background()
	if _, err := a.Token(ctx, "o", "r"); err != nil {
		t.Fatal(err)
	}
	*clock = clock.Add(39 * time.Minute)
	if _, err := a.Token(ctx, "o", "r"); err != nil || f.mints != 1 {
		t.Fatalf("mints = %d, err = %v: the token still has more than 20 minutes", f.mints, err)
	}
	*clock = clock.Add(2 * time.Minute)
	if _, err := a.Token(ctx, "o", "r"); err != nil || f.mints != 2 {
		t.Fatalf("mints = %d, err = %v: the token is inside the refresh margin", f.mints, err)
	}
}

func TestAppNotInstalledOnTheRepoIsAnError(t *testing.T) {
	a, f, _ := newApp(t)
	f.missing = true
	_, err := a.Token(context.Background(), "o", "r")
	if err == nil || !strings.Contains(err.Error(), "find installation on o/r") || !isStatus(err, 404) {
		t.Fatalf("err = %v", err)
	}
	if f.mints != 0 {
		t.Fatal("no token may be minted without an installation")
	}
}

func TestGitHubUsesTheAppTokenForTheRepoInThePath(t *testing.T) {
	a, _, _ := newApp(t)
	var got string
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r.Header.Get("Authorization")
		w.Write([]byte(`{}`))
	}))
	defer api.Close()
	g := &GitHub{API: api.URL, Auth: a}
	if err := g.do(context.Background(), http.MethodGet, "/repos/o/shop/pulls?state=open", nil, &struct{}{}); err != nil {
		t.Fatal(err)
	}
	if got != "Bearer ghs_shop" {
		t.Fatalf("Authorization = %q", got)
	}
	if err := g.do(context.Background(), http.MethodGet, "/user", nil, nil); err == nil {
		t.Fatal("a non-repository path has no installation to mint for")
	}
}

func TestParsePrivateKeyAcceptsEveryShape(t *testing.T) {
	key, _ := rsa.GenerateKey(rand.Reader, 2048)
	pkcs1 := pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)})
	der, _ := x509.MarshalPKCS8PrivateKey(key)
	pkcs8 := pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der})
	for name, in := range map[string]string{
		"pkcs1":        string(pkcs1),
		"pkcs8":        string(pkcs8),
		"escaped":      strings.ReplaceAll(string(pkcs1), "\n", `\n`),
		"base64":       base64.StdEncoding.EncodeToString(pkcs1),
		"base64 pkcs8": base64.StdEncoding.EncodeToString(pkcs8),
	} {
		got, err := ParsePrivateKey(in)
		if err != nil || !got.Equal(key) {
			t.Errorf("%s: %v", name, err)
		}
	}
	for _, bad := range []string{"", "not a key", "-----BEGIN RSA PRIVATE KEY-----\nAAAA\n-----END RSA PRIVATE KEY-----"} {
		if _, err := ParsePrivateKey(bad); err == nil {
			t.Errorf("%q must be rejected", bad)
		}
	}
}
