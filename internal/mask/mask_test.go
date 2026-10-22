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
		"chave AKIAABCDEFGHIJKLMNOP vazou":                        "chave [AWS_KEY] vazou",
		"Authorization: Bearer abcdefghijklmnopqrstuvwxyz":        "Authorization: Bearer [TOKEN]",
		"DB_PASSWORD=hunter2 e api_key: 'xyz'":                    "DB_PASSWORD=[SEGREDO] e api_key: '[SEGREDO]'",
		"GITHUB_TOKEN: ghp_abc e MAX_TOKENS=500":                  "GITHUB_TOKEN: [SEGREDO] e MAX_TOKENS=500",
		`{"token":"abc","pod":"x"}`:                               `{"token":"[SEGREDO]","pod":"x"}`,
		"senha=hunter2":                                           "senha=[SEGREDO]",
		"jwt eyJhbGciOiJIUzI1NiJ9.eyJzdWIiOiIxMjM0In0.abcdefgh12": "jwt [JWT]",
		"cartão 4111 1111 1111 1111":                              "cartão [CARTAO]",
		"fale com ana@empresa.com.br":                             "fale com [EMAIL]",
	}
	for in, want := range cases {
		counts := map[string]int{}
		if got := m.Apply(in, counts); got != want {
			t.Errorf("%q\n  got  %q\n  want %q", in, got, want)
		}
	}
}

// O que NÃO pode ser mascarado: atrapalharia o troubleshooting.
func TestNaoMascaraOQueNaoESensivel(t *testing.T) {
	m, _ := New(nil)
	for _, s := range []string{
		"o pod checkout-7d9f está em CrashLoopBackOff",
		"erro AMQ2058 no canal QM1.TO.QM2, porta 1414",
		"exit code 137, 52310 mensagens na fila pedidos",
		"pedido número 1234567890123", // 13 dígitos sem Luhn válido
		"token de acesso expirou",     // a palavra "token" sozinha
	} {
		if got := m.Apply(s, map[string]int{}); got != s {
			t.Errorf("não deveria mudar: %q → %q", s, got)
		}
	}
}

func TestChavePrivadaEContagem(t *testing.T) {
	m, _ := New(nil)
	in := "-----BEGIN RSA PRIVATE KEY-----\nMIIEpAIBAAKCAQEA\n-----END RSA PRIVATE KEY-----\ne cpf 111.222.333-44 e 555.666.777-88"
	counts := map[string]int{}
	got := m.Apply(in, counts)
	if strings.Contains(got, "MIIE") || counts["chave_privada"] != 1 || counts["cpf"] != 2 {
		t.Fatalf("got=%q counts=%v", got, counts)
	}
}

func TestConfig(t *testing.T) {
	m, err := New(&Config{Desligar: []string{"email"}, Extra: []Extra{{Nome: "matricula", Regex: `MAT-\d{6}`}}})
	if err != nil {
		t.Fatal(err)
	}
	got := m.Apply("ana@x.com tem MAT-123456", map[string]int{})
	if got != "ana@x.com tem [MATRICULA]" {
		t.Errorf("got %q", got)
	}
	if off, _ := New(&Config{Desligado: true}); off.Apply("123.456.789-09", map[string]int{}) != "123.456.789-09" {
		t.Error("desligado não mascara nada")
	}
	if _, err := New(&Config{Desligar: []string{"inexistente"}}); err == nil {
		t.Error("regra desconhecida deveria dar erro claro")
	}
	if _, err := New(&Config{Extra: []Extra{{Nome: "x", Regex: "("}}}); err == nil {
		t.Error("regex inválida deveria dar erro")
	}
}
