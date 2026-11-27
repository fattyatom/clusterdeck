package gitops

import (
	"bytes"
	"context"
	"crypto/rsa"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/go-jose/go-jose/v4"
	"github.com/go-jose/go-jose/v4/jwt"
)

const tokenRefreshMargin = 20 * time.Minute

type TokenSource interface {
	Token(ctx context.Context, owner, repo string) (string, error)
}

type AppAuth struct {
	API    string
	AppID  string
	Key    *rsa.PrivateKey
	Client *http.Client
	Now    func() time.Time

	mu    sync.Mutex
	cache map[string]cachedToken
}

type cachedToken struct {
	token   string
	expires time.Time
}

func ParsePrivateKey(raw string) (*rsa.PrivateKey, error) {
	s := strings.TrimSpace(raw)
	s = strings.ReplaceAll(s, `\n`, "\n")
	if !strings.HasPrefix(s, "-----BEGIN") {
		dec, err := base64.StdEncoding.DecodeString(s)
		if err != nil {
			return nil, errors.New("private key is neither PEM nor base64 of a PEM")
		}
		s = string(dec)
	}
	block, _ := pem.Decode([]byte(s))
	if block == nil {
		return nil, errors.New("private key has no PEM block")
	}
	if k, err := x509.ParsePKCS1PrivateKey(block.Bytes); err == nil {
		return k, nil
	}
	parsed, err := x509.ParsePKCS8PrivateKey(block.Bytes)
	if err != nil {
		return nil, fmt.Errorf("private key is not PKCS#1 or PKCS#8: %w", err)
	}
	k, ok := parsed.(*rsa.PrivateKey)
	if !ok {
		return nil, errors.New("private key is not an RSA key")
	}
	return k, nil
}

func (a *AppAuth) now() time.Time {
	if a.Now != nil {
		return a.Now()
	}
	return time.Now()
}

func (a *AppAuth) jwt() (string, error) {
	signer, err := jose.NewSigner(
		jose.SigningKey{Algorithm: jose.RS256, Key: a.Key},
		(&jose.SignerOptions{}).WithType("JWT"),
	)
	if err != nil {
		return "", err
	}
	now := a.now()
	return jwt.Signed(signer).Claims(jwt.Claims{
		Issuer:   a.AppID,
		IssuedAt: jwt.NewNumericDate(now.Add(-time.Minute)),
		Expiry:   jwt.NewNumericDate(now.Add(9 * time.Minute)),
	}).Serialize()
}

func (a *AppAuth) Token(ctx context.Context, owner, repo string) (string, error) {
	key := owner + "/" + repo
	a.mu.Lock()
	defer a.mu.Unlock()
	if c, ok := a.cache[key]; ok && c.expires.Sub(a.now()) > tokenRefreshMargin {
		return c.token, nil
	}
	token, expires, err := a.mint(ctx, owner, repo)
	if err != nil {
		return "", err
	}
	if a.cache == nil {
		a.cache = map[string]cachedToken{}
	}
	a.cache[key] = cachedToken{token: token, expires: expires}
	return token, nil
}

func (a *AppAuth) mint(ctx context.Context, owner, repo string) (string, time.Time, error) {
	bearer, err := a.jwt()
	if err != nil {
		return "", time.Time{}, fmt.Errorf("github app: sign jwt: %w", err)
	}
	var inst struct {
		ID int64 `json:"id"`
	}
	if err := a.call(ctx, http.MethodGet, "/repos/"+owner+"/"+repo+"/installation", bearer, nil, &inst); err != nil {
		return "", time.Time{}, fmt.Errorf("github app: find installation on %s/%s: %w", owner, repo, err)
	}
	var tok struct {
		Token     string    `json:"token"`
		ExpiresAt time.Time `json:"expires_at"`
	}
	path := fmt.Sprintf("/app/installations/%d/access_tokens", inst.ID)
	if err := a.call(ctx, http.MethodPost, path, bearer, map[string]any{"repositories": []string{repo}}, &tok); err != nil {
		return "", time.Time{}, fmt.Errorf("github app: mint token for %s/%s: %w", owner, repo, err)
	}
	if tok.Token == "" {
		return "", time.Time{}, errors.New("github app: empty installation token")
	}
	return tok.Token, tok.ExpiresAt, nil
}

func (a *AppAuth) call(ctx context.Context, method, p, bearer string, body, out any) error {
	var r io.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			return err
		}
		r = bytes.NewReader(raw)
	}
	api := strings.TrimRight(a.API, "/")
	if api == "" {
		api = "https://api.github.com"
	}
	req, err := http.NewRequestWithContext(ctx, method, api+p, r)
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	req.Header.Set("Authorization", "Bearer "+bearer)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	client := a.Client
	if client == nil {
		client = &http.Client{Timeout: 30 * time.Second}
	}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode >= 300 {
		msg := string(raw)
		if len(msg) > 300 {
			msg = msg[:300]
		}
		return &apiError{Status: resp.StatusCode, Body: msg}
	}
	return json.Unmarshal(raw, out)
}
