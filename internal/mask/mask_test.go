package mask

import (
	"strings"
	"testing"
)

func TestBuiltin(t *testing.T) {
	m, _ := New(nil)
	cases := map[string]string{
		"meu cpf é 123.456.789-09":                                "meu cpf é [CPF]",
		"empresa 12.345.678/0001-90":                              "empresa [CNPJ]",
		"key AKIAABCDEFGHIJKLMNOP leaked":                         "key [AWS_KEY] leaked",
		"Authorization: Bearer abcdefghijklmnopqrstuvwxyz":        "Authorization: Bearer [TOKEN]",
		"DB_PASSWORD=hunter2 and api_key: 'xyz'":                  "DB_PASSWORD=[SECRET] and api_key: '[SECRET]'",
		"GITHUB_TOKEN: ghp_abc and MAX_TOKENS=500":                "GITHUB_TOKEN: [SECRET] and MAX_TOKENS=500",
		`{"token":"abc","pod":"x"}`:                               `{"token":"[SECRET]","pod":"x"}`,
		"senha=hunter2":                                           "senha=[SECRET]",
		"jwt eyJhbGciOiJIUzI1NiJ9.eyJzdWIiOiIxMjM0In0.abcdefgh12": "jwt [JWT]",
		"card 4111 1111 1111 1111":                                "card [CREDIT_CARD]",
		"talk to ana@company.com.br":                              "talk to [EMAIL]",
	}
	for in, want := range cases {
		counts := map[string]int{}
		if got := m.Apply(in, counts); got != want {
			t.Errorf("%q\n  got  %q\n  want %q", in, got, want)
		}
	}
}

// What must NOT be masked: it would get in the way of troubleshooting.
func TestDoesNotMaskNonSensitiveText(t *testing.T) {
	m, _ := New(nil)
	for _, s := range []string{
		"pod checkout-7d9f is in CrashLoopBackOff",
		"error AMQ2058 on channel QM1.TO.QM2, port 1414",
		"exit code 137, 52310 messages in queue orders",
		"order number 1234567890123", // 13 digits without a valid Luhn
		"access token expired",       // the word "token" alone
	} {
		if got := m.Apply(s, map[string]int{}); got != s {
			t.Errorf("must not change: %q → %q", s, got)
		}
	}
}

func TestPrivateKeyAndCounts(t *testing.T) {
	m, _ := New(nil)
	in := "-----BEGIN RSA PRIVATE KEY-----\nMIIEpAIBAAKCAQEA\n-----END RSA PRIVATE KEY-----\nand cpf 111.222.333-44 and 555.666.777-88"
	counts := map[string]int{}
	got := m.Apply(in, counts)
	if strings.Contains(got, "MIIE") || counts["private_key"] != 1 || counts["cpf"] != 2 {
		t.Fatalf("got=%q counts=%v", got, counts)
	}
}

func TestConfig(t *testing.T) {
	m, err := New(&Config{Disable: []string{"email"}, Extra: []Extra{{Name: "employee_id", Regex: `MAT-\d{6}`}}})
	if err != nil {
		t.Fatal(err)
	}
	if got := m.Apply("ana@x.com has MAT-123456", map[string]int{}); got != "ana@x.com has [EMPLOYEE_ID]" {
		t.Errorf("got %q", got)
	}
	if off, _ := New(&Config{Disabled: true}); off.Apply("123.456.789-09", map[string]int{}) != "123.456.789-09" {
		t.Error("disabled must not mask anything")
	}
	if _, err := New(&Config{Disable: []string{"nope"}}); err == nil {
		t.Error("an unknown rule must give a clear error")
	}
	if _, err := New(&Config{Extra: []Extra{{Name: "x", Regex: "("}}}); err == nil {
		t.Error("an invalid regex must give an error")
	}
}
