package live

import (
	"errors"
	"strings"
	"testing"

	"github.com/chwiee/nautilm/internal/config"
)

const base = `
providers:
  a: {type: openai, base_url: http://a/v1}
models:
  fast: {provider: a, model: m1}
pricing:
  - {provider: a, model: m1, input: 1, output: 2}
strict_pricing: true
database: {url: "postgres://x"}
dashboard:
  public_url: http://localhost
  entra: {tenant_id: t, client_id: c, client_secret: s, admin_groups: [g-admin], viewer_groups: [g-view]}
`

func baseCfg(t *testing.T) *config.Config {
	c, err := config.Parse([]byte(base))
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func TestMergeAddsAndKeepsTheFileIntact(t *testing.T) {
	b := baseCfg(t)
	o := Overrides{}.clone()
	o.Providers["remote"] = ProviderDef{Type: "openai", SecretARN: "arn:x"}
	o.Models["fast"] = ModelDef{Provider: "remote", Model: "r1"}
	o.Models["new"] = ModelDef{Provider: "remote", Model: "r1", Fallback: []string{"fast"}}
	o.Pricing = []PriceDef{{Provider: "remote", Model: "*", Free: true}}
	o.Groups = Groups{Admin: []Group{{ID: "g-extra"}}}
	c, err := Merge(b, o, map[string]Key{"remote": {APIKey: "k", BaseURL: "http://r/v1"}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if p := c.Providers["remote"]; p.APIKey != "k" || p.BaseURL != "http://r/v1" {
		t.Errorf("provider from the secret: %+v", p)
	}
	if c.Models["fast"].Provider != "remote" || c.Models["new"].Type != "chat" {
		t.Errorf("models: %+v", c.Models)
	}
	if got := c.Dashboard.Entra.AdminGroups; len(got) != 2 || got[0] != "g-admin" {
		t.Errorf("groups are added to the file ones: %v", got)
	}
	// The base config is never modified (other snapshots still use it).
	if b.Models["fast"].Provider != "a" || len(b.Providers) != 1 || len(b.Dashboard.Entra.AdminGroups) != 1 || len(b.Pricing) != 1 {
		t.Error("Merge changed the base config")
	}
}

func TestMergeValidatesLikeTheStartup(t *testing.T) {
	b := baseCfg(t)
	cases := map[string]func(*Overrides){
		"strict_pricing": func(o *Overrides) { o.Models["x"] = ModelDef{Provider: "a", Model: "no-price"} },
		"does not exist": func(o *Overrides) { o.Models["x"] = ModelDef{Provider: "ghost", Model: "m1"} },
		"config file":    func(o *Overrides) { o.Providers["a"] = ProviderDef{Type: "openai", BaseURL: "http://z"} },
		"fallback":       func(o *Overrides) { o.Models["x"] = ModelDef{Provider: "a", Model: "m1", Fallback: []string{"nope"}} },
		"empty group":    func(o *Overrides) { o.Groups.Viewer = []Group{{Name: "no id"}} },
	}
	for want, change := range cases {
		o := Overrides{}.clone()
		change(&o)
		if _, err := Merge(b, o, nil, nil); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%s: %v", want, err)
		}
	}
}

func TestBrokenSecretKeepsTheConfigValid(t *testing.T) {
	o := Overrides{}.clone()
	o.Providers["remote"] = ProviderDef{Type: "openai", SecretARN: "arn:x"}
	c, err := Merge(baseCfg(t), o, nil, map[string]error{"remote": errors.New("AccessDenied")})
	if err != nil || c.Providers["remote"].BaseURL != unavailable {
		t.Fatalf("a provider whose secret fails must not break the config: %v", err)
	}
}

func TestParseKey(t *testing.T) {
	if k, err := ParseKey(` {"api_key":"sk","base_url":"http://u/v1"} `); err != nil || k.APIKey != "sk" || k.BaseURL != "http://u/v1" {
		t.Errorf("json: %+v %v", k, err)
	}
	if k, err := ParseKey("sk-plain"); err != nil || k.APIKey != "sk-plain" {
		t.Errorf("plain: %+v %v", k, err)
	}
	_, err := ParseKey(`{"token":"do-not-print-me","x":"y"}`)
	if err == nil || !strings.Contains(err.Error(), "token, x") || strings.Contains(err.Error(), "do-not-print-me") {
		t.Errorf("missing api_key must list key NAMES only: %v", err)
	}
	for _, bad := range []string{"", "  ", "{not json"} {
		if _, err := ParseKey(bad); err == nil {
			t.Errorf("%q must fail", bad)
		}
	}
}

func TestInspectARN(t *testing.T) {
	i := InspectARN("arn:aws:secretsmanager:us-east-2:123456789012:secret:nautilm/providers/claude-AbC123", "")
	if !i.Valid || !i.MatchesPrefix || i.Partial || i.Account != "123456789012" || i.Region != "us-east-2" {
		t.Errorf("%+v", i)
	}
	if !strings.Contains(i.PolicyPrefix, `"Resource": "arn:aws:secretsmanager:us-east-2:123456789012:secret:nautilm/providers/*"`) ||
		strings.Index(i.PolicyPrefix, "Sid") > strings.Index(i.PolicyPrefix, "Resource") {
		t.Errorf("prefix policy (Sid first): %s", i.PolicyPrefix)
	}
	// Outside the prefix, and partial (no 6-character suffix).
	i = InspectARN("arn:aws:secretsmanager:sa-east-1:123456789012:secret:team/key", "")
	if !i.Valid || i.MatchesPrefix || !i.Partial {
		t.Errorf("%+v", i)
	}
	if i = InspectARN("arn:aws:s3:::bucket", ""); i.Valid {
		t.Error("an S3 ARN is not a secret")
	}
}
