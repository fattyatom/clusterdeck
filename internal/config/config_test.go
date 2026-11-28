package config

import "testing"

func env(m map[string]string) func(string) string { return func(k string) string { return m[k] } }

func TestDefaults(t *testing.T) {
	c, err := FromEnv(env(nil))
	if err != nil {
		t.Fatal(err)
	}
	if c.ActionsEnabled || c.MetricsProvider != "none" || c.ListenAddr != ":8080" || c.BackupSelector != "clusterdeck.io/role=backup" {
		t.Fatalf("defaults = %+v", c)
	}
	if len(c.HideNamespaces) != 3 {
		t.Fatalf("hidden = %v", c.HideNamespaces)
	}
}

func TestValidation(t *testing.T) {
	cases := map[string]map[string]string{
		"actions without identity":     {"ACTIONS_ENABLED": "true"},
		"bad bool":                     {"ACTIONS_ENABLED": "yes please"},
		"signoz without key":           {"METRICS_PROVIDER": "signoz", "SIGNOZ_URL": "http://s"},
		"prometheus without url":       {"METRICS_PROVIDER": "prometheus"},
		"unknown provider":             {"METRICS_PROVIDER": "datadog"},
		"auto rollback alone":          {"AUTO_ROLLBACK_ENABLED": "true"},
		"interval too short":           {"AUTO_ROLLBACK_INTERVAL": "1s"},
		"oidc half set":                {"OIDC_ISSUER_URL": "https://id", "OIDC_CLIENT_ID": "c"},
		"oidc and header":              {"AUTH_HEADER": "X-User", "OIDC_ISSUER_URL": "https://id", "OIDC_CLIENT_ID": "c", "OIDC_CLIENT_SECRET": "s", "OIDC_REDIRECT_URL": "https://d/auth/callback"},
		"short session secret":         {"SESSION_SECRET": "short"},
		"session ttl too short":        {"SESSION_TTL": "1m"},
		"inflight repo not owner/name": {"INFLIGHT_REPO": "homelab", "GITHUB_TOKEN": "t"},
		"bad soak":                     {"SOAK_SECONDS": "1h"},
		"negative watch":               {"WATCH_SECONDS": "-1"},
	}
	for name, e := range cases {
		if _, err := FromEnv(env(e)); err == nil {
			t.Errorf("%s: want an error", name)
		}
	}
}

func TestFullConfig(t *testing.T) {
	c, err := FromEnv(env(map[string]string{
		"ACTIONS_ENABLED":       "true",
		"AUTH_HEADER":           "X-Forwarded-Email",
		"AUTH_ALLOWED_USERS":    " a@x.io, b@x.io ,",
		"METRICS_PROVIDER":      "signoz",
		"SIGNOZ_URL":            "http://signoz",
		"SIGNOZ_API_KEY":        "k",
		"HIDE_NAMESPACES":       "",
		"ACTIONS_NAMESPACES":    "shop,photos",
		"AUTO_ROLLBACK_ENABLED": "true",
	}))
	if err != nil {
		t.Fatal(err)
	}
	if c.GitHubAPIURL != "https://api.github.com" || c.GitHubToken != "" {
		t.Fatalf("github defaults = %+v", c)
	}
	if !c.AutoRollback || c.AutoRollbackInterval.Seconds() != 30 || len(c.ActionsNamespaces) != 2 {
		t.Fatalf("auto rollback config = %+v", c)
	}
	if !c.ActionsEnabled || len(c.AllowedUsers) != 2 || c.AllowedUsers[1] != "b@x.io" {
		t.Fatalf("config = %+v", c)
	}
}

func TestOIDCConfig(t *testing.T) {
	c, err := FromEnv(env(map[string]string{
		"ACTIONS_ENABLED":    "true",
		"OIDC_ISSUER_URL":    "https://id.example.com",
		"OIDC_CLIENT_ID":     "c",
		"OIDC_CLIENT_SECRET": "s",
		"OIDC_REDIRECT_URL":  "https://deck.example.com/auth/callback",
	}))
	if err != nil {
		t.Fatal(err)
	}
	if !c.OIDC() || c.OIDCUserClaim != "email" || len(c.OIDCScopes) != 3 || c.SessionTTL.Hours() != 12 || c.SessionSecret != "" {
		t.Fatalf("oidc config = %+v", c)
	}
	if d, _ := FromEnv(env(nil)); d.OIDC() {
		t.Fatal("OIDC is on by default")
	}
}

func TestGitHubAppNeedsIDAndKeyTogether(t *testing.T) {
	if _, err := FromEnv(env(map[string]string{"GITHUB_APP_ID": "1"})); err == nil {
		t.Fatal("an App id without a key must be rejected")
	}
	if _, err := FromEnv(env(map[string]string{"GITHUB_APP_PRIVATE_KEY": "k"})); err == nil {
		t.Fatal("a key without an App id must be rejected")
	}
	c, err := FromEnv(env(map[string]string{"GITHUB_APP_ID": "1", "GITHUB_APP_PRIVATE_KEY": "k"}))
	if err != nil || c.GitHubAppID != "1" || c.GitHubAppPrivateKey != "k" {
		t.Fatalf("config = %+v, %v", c, err)
	}
}

func TestInFlight(t *testing.T) {
	c, err := FromEnv(env(map[string]string{"INFLIGHT_REPO": "fattyatom/homelab", "GITHUB_TOKEN": "t", "SOAK_SECONDS": "1800"}))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := FromEnv(env(map[string]string{"INFLIGHT_REPO": "o/r"})); err != nil {
		t.Fatalf("INFLIGHT_REPO without GitHub credentials must not stop the start: %v", err)
	}
	if c.InFlightRepo != "fattyatom/homelab" || c.InFlightBranch != "main" || c.SoakSeconds != 1800 || c.WatchSeconds != 3600 {
		t.Fatalf("config = %+v", c)
	}
}
