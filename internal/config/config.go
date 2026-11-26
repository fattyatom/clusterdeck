package config

import (
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"
)

type Config struct {
	ListenAddr              string
	ClusterName             string
	Kubeconfig              string
	AuthHeader              string
	AllowedUsers            []string
	ActionsEnabled          bool
	ActionsNamespaces       []string
	AutoRollback            bool
	AutoRollbackInterval    time.Duration
	HideNamespaces          []string
	BackupSelector          string
	RestoreCheckSelector    string
	RestoreTemplateSelector string
	MetricsProvider         string
	SigNozURL               string
	SigNozAPIKey            string
	SigNozFilter            string
	PrometheusURL           string
	GitHubToken             string
	GitHubAPIURL            string
	GitHubAppID             string
	GitHubAppPrivateKey     string
	OIDCIssuerURL           string
	OIDCClientID            string
	OIDCClientSecret        string
	OIDCRedirectURL         string
	OIDCUserClaim           string
	OIDCScopes              []string
	SessionSecret           string
	SessionTTL              time.Duration
}

func (c *Config) OIDC() bool { return c.OIDCIssuerURL != "" }

func FromEnv(getenv func(string) string) (*Config, error) {
	get := func(key, def string) string {
		if v := strings.TrimSpace(getenv(key)); v != "" {
			return v
		}
		return def
	}
	c := &Config{
		ListenAddr:              get("LISTEN_ADDR", ":8080"),
		ClusterName:             get("CLUSTER_NAME", "cluster"),
		Kubeconfig:              get("KUBECONFIG", ""),
		AuthHeader:              get("AUTH_HEADER", ""),
		AllowedUsers:            list(get("AUTH_ALLOWED_USERS", "")),
		ActionsNamespaces:       list(get("ACTIONS_NAMESPACES", "")),
		HideNamespaces:          list(get("HIDE_NAMESPACES", "kube-system,kube-public,kube-node-lease")),
		BackupSelector:          get("BACKUP_SELECTOR", "clusterdeck.io/role=backup"),
		RestoreCheckSelector:    get("RESTORE_CHECK_SELECTOR", "clusterdeck.io/role=restore-check"),
		RestoreTemplateSelector: get("RESTORE_TEMPLATE_SELECTOR", "clusterdeck.io/role=restore-template"),
		MetricsProvider:         get("METRICS_PROVIDER", "none"),
		SigNozURL:               get("SIGNOZ_URL", ""),
		SigNozAPIKey:            get("SIGNOZ_API_KEY", ""),
		SigNozFilter:            get("SIGNOZ_FILTER", ""),
		PrometheusURL:           get("PROMETHEUS_URL", ""),
		GitHubToken:             get("GITHUB_TOKEN", ""),
		GitHubAPIURL:            get("GITHUB_API_URL", "https://api.github.com"),
		GitHubAppID:             get("GITHUB_APP_ID", ""),
		GitHubAppPrivateKey:     get("GITHUB_APP_PRIVATE_KEY", ""),
		OIDCIssuerURL:           get("OIDC_ISSUER_URL", ""),
		OIDCClientID:            get("OIDC_CLIENT_ID", ""),
		OIDCClientSecret:        get("OIDC_CLIENT_SECRET", ""),
		OIDCRedirectURL:         get("OIDC_REDIRECT_URL", ""),
		OIDCUserClaim:           get("OIDC_USER_CLAIM", "email"),
		OIDCScopes:              list(get("OIDC_SCOPES", "openid,email,profile")),
		SessionSecret:           get("SESSION_SECRET", ""),
	}
	var err error
	if c.ActionsEnabled, err = strconv.ParseBool(get("ACTIONS_ENABLED", "false")); err != nil {
		return nil, fmt.Errorf("ACTIONS_ENABLED: %w", err)
	}
	if c.AutoRollback, err = strconv.ParseBool(get("AUTO_ROLLBACK_ENABLED", "false")); err != nil {
		return nil, fmt.Errorf("AUTO_ROLLBACK_ENABLED: %w", err)
	}
	if c.AutoRollbackInterval, err = time.ParseDuration(get("AUTO_ROLLBACK_INTERVAL", "30s")); err != nil || c.AutoRollbackInterval < 10*time.Second {
		return nil, fmt.Errorf("AUTO_ROLLBACK_INTERVAL must be a duration of at least 10s")
	}
	if c.AutoRollback && !c.ActionsEnabled {
		return nil, fmt.Errorf("AUTO_ROLLBACK_ENABLED=true needs ACTIONS_ENABLED=true")
	}
	if c.SessionTTL, err = time.ParseDuration(get("SESSION_TTL", "12h")); err != nil || c.SessionTTL < 5*time.Minute {
		return nil, fmt.Errorf("SESSION_TTL must be a duration of at least 5m")
	}
	if (c.GitHubAppID == "") != (c.GitHubAppPrivateKey == "") {
		return nil, fmt.Errorf("GitHub App auth needs both GITHUB_APP_ID and GITHUB_APP_PRIVATE_KEY")
	}
	oidcSet := c.OIDCIssuerURL != "" || c.OIDCClientID != "" || c.OIDCClientSecret != "" || c.OIDCRedirectURL != ""
	if oidcSet && (c.OIDCIssuerURL == "" || c.OIDCClientID == "" || c.OIDCClientSecret == "" || c.OIDCRedirectURL == "") {
		return nil, fmt.Errorf("OIDC login needs all of OIDC_ISSUER_URL, OIDC_CLIENT_ID, OIDC_CLIENT_SECRET and OIDC_REDIRECT_URL")
	}
	if oidcSet && c.AuthHeader != "" {
		return nil, fmt.Errorf("set AUTH_HEADER or OIDC_ISSUER_URL, not both: they are two ways to identify the user")
	}
	if c.SessionSecret != "" && len(c.SessionSecret) < 32 {
		return nil, fmt.Errorf("SESSION_SECRET must be at least 32 characters")
	}
	if c.ActionsEnabled && c.AuthHeader == "" && !oidcSet {
		return nil, fmt.Errorf("ACTIONS_ENABLED=true needs AUTH_HEADER or OIDC login: actions are only allowed for an identified user")
	}
	switch c.MetricsProvider {
	case "none":
	case "signoz":
		if c.SigNozURL == "" || c.SigNozAPIKey == "" {
			return nil, fmt.Errorf("METRICS_PROVIDER=signoz needs SIGNOZ_URL and SIGNOZ_API_KEY")
		}
	case "prometheus":
		if c.PrometheusURL == "" {
			return nil, fmt.Errorf("METRICS_PROVIDER=prometheus needs PROMETHEUS_URL")
		}
	default:
		return nil, fmt.Errorf("METRICS_PROVIDER must be none, signoz or prometheus, got %q", c.MetricsProvider)
	}
	return c, nil
}

func FromOS() (*Config, error) { return FromEnv(os.Getenv) }

func list(s string) []string {
	var out []string
	for _, p := range strings.Split(s, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}
