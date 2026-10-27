# Convenções, como estender e armadilhas

## Estilo

- Go 1.26, stdlib primeiro. Dependências diretas: AWS SDK v2 (Bedrock, Secrets Manager), pgx v5,
  go-oidc v3 + x/oauth2, valkey-go, yaml v3, smithy-go. Nada de framework web.
- Código, comentários, logs (JSON `slog`), erros de API e textos do painel em **inglês**; docs em pt-BR.
- Comentários explicam o **porquê** (decisão, risco), não o óbvio.
- Erros de API no formato OpenAI (`{"error":{"message","type","code"}}`); `code` diz **de quem** é o
  problema (`quota_day` = nautilm, `provider_quota_day` = conta no provedor…).
- Painel: um único `internal/gateway/web/dashboard.html` (JS puro, sem CDN, sem build), tokens de cor
  do mirante. Texto em inglês.

## Como estender

| Quero… | Onde mexer |
|---|---|
| Novo tipo de provedor | implementar `provider.Provider` (+ `Streamer`/`Embedder` opcionais) em `internal/provider/<tipo>`; registrar em `live.DefaultBuilder` e no `switch` de `config.Validate`; no assistente (`live.TestProvider`) adicionar o teste do tipo |
| Nova chave de config | struct em `internal/config/config.go` (tag yaml) + validação + `config.example.yaml` + tabela em `.claude/contexto/configuracao.md` |
| Algo editável na aba Config | novo kind em `store.RevisionKinds`, campo em `live.Overrides`, `Overrides.With`, `live.Merge`, handler em `gateway/configadmin.go`, UI |
| Novo endpoint admin | `authRoutes`/`configRoutes` com `adminOnly`/`configOnly` (sessão admin + Origin) e `auditLog` |
| Nova migração | `internal/store/migrations/000N_nome.sql` (aplicada em ordem, com advisory lock) + Postgres e Memory + `conformance` em `store_test.go` |
| Nova regra de mascaramento | `internal/mask` + teste |

## Testes

- `go test ./...` precisa ficar verde; `gofmt -l .` vazio; `go vet ./...` limpo.
- Gateway: `helpers_test.go` (`testConfig`, provedor `fake`), `admin_test.go` (`setupAdmin`: memória +
  Entra simulado + gerenciador de config + `fakeSecrets`), `configadmin_test.go` (provedor HTTP falso
  `upstream` que exige `Bearer good-key` e recusa `messages` vazio).
- Store: o mesmo teste de conformidade roda em Memory e em Postgres real (`NAUTILM_TEST_DATABASE_URL`).
- Bedrock: tradução testada sem AWS (`buildInput`, `parseOutput`, `mapError`).
- Mudança visível no painel: validar no laboratório kind pelo navegador (não só em teste unitário).

## Armadilhas conhecidas (já aconteceram)

- **perl com `|` como delimitador** + `\|` no padrão corrompe arquivos Go. Use a ferramenta de edição
  ou outro delimitador.
- Heredoc com aspas no bash do Windows quebra: escreva o arquivo com a ferramenta de escrita.
- `sed` com `\n` no texto de substituição inseriu quebra de linha dentro de string Go.
- CSS `display: grid` numa classe anula o atributo `hidden`: use `.classe[hidden] { display: none; }`.
- Provedor OpenAI-compatível reenvia o **JSON bruto** (`provider.Call.Raw`): chamadas montadas pelo
  próprio nautilm (testes da aba Config) precisam preencher `Raw`, senão vão sem `messages`.
- Entra simulado precisa de `_claim_sources` no caso de overage, senão o go-oidc recusa o token.
- `git push` pode travar esperando login do Git Credential Manager; o `gh` já está autenticado:
  `git -c credential.helper= -c 'credential.helper=!gh auth git-credential' push` (só naquela chamada).
- Bedrock: em us-east-2 o Claude 5.5 só existe via perfil `us.`/`global.`; conta de região única não tem.
  Conta nova pode vir com quota **0** por modelo (`provider_quota_day`).
- Laboratório: Ollama precisa estar rodando no host (`host.docker.internal:11434`); depois de reiniciar o
  Docker o cluster kind volta sozinho, o Ollama não.
