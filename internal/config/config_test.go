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
		"actions without identity": {"ACTIONS_ENABLED": "true"},
		"bad bool":                 {"ACTIONS_ENABLED": "yes please"},
		"signoz without key":       {"METRICS_PROVIDER": "signoz", "SIGNOZ_URL": "http://s"},
		"prometheus without url":   {"METRICS_PROVIDER": "prometheus"},
		"unknown provider":         {"METRICS_PROVIDER": "datadog"},
	}
	for name, e := range cases {
		if _, err := FromEnv(env(e)); err == nil {
			t.Errorf("%s: want an error", name)
		}
	}
}

func TestFullConfig(t *testing.T) {
	c, err := FromEnv(env(map[string]string{
		"ACTIONS_ENABLED":    "true",
		"AUTH_HEADER":        "X-Forwarded-Email",
		"AUTH_ALLOWED_USERS": " a@x.io, b@x.io ,",
		"METRICS_PROVIDER":   "signoz",
		"SIGNOZ_URL":         "http://signoz",
		"SIGNOZ_API_KEY":     "k",
		"HIDE_NAMESPACES":    "",
	}))
	if err != nil {
		t.Fatal(err)
	}
	if !c.ActionsEnabled || len(c.AllowedUsers) != 2 || c.AllowedUsers[1] != "b@x.io" {
		t.Fatalf("config = %+v", c)
	}
}
