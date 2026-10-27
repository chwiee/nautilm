# Configuração do nautilm: chaves, variáveis e onde cada valor mora

Fonte da verdade: `internal/config/config.go` (structs com comentários) e `config.example.yaml`
(exemplo comentado e testado: `go test ./internal/config` falha se ele ficar inválido).

O YAML passa por `os.ExpandEnv`: **todo segredo vem de `${VAR}`**. Chave desconhecida = erro na
subida (com dica para chaves antigas do pedagio).

## Chaves do arquivo

| Chave | O que é | Padrão | Também na aba Config? |
|---|---|---|---|
| `listen` | endereço HTTP | `:8080` | não |
| `providers.<nome>.type` | `bedrock` ou `openai` | — | sim (adicionar) |
| `providers.<nome>.region` | região do Bedrock | — | sim |
| `providers.<nome>.base_url` / `api_key` / `timeout` | OpenAI-compatível (`api_key: ${VAR}`) | timeout 120s | sim (chave via Secrets Manager) |
| `models.<apelido>.provider` / `model` | onde roda e o ID real | — | sim (criar/sobrepor) |
| `models.<apelido>.type` | `chat` ou `embedding` | `chat` | sim |
| `models.<apelido>.max_tokens` | limite padrão de saída | 0 (o do agente) | sim |
| `models.<apelido>.fallback` | apelidos tentados em 429/5xx | — | sim |
| `models.<apelido>.dimensions` | dimensões do embedding | do modelo | sim |
| `models.<apelido>.prompt_cache` | `off`/`explicit`/`auto` (Bedrock) | `off` | sim |
| `pricing[]` | `provider`, `model` (`*` = todos), `input`, `output`, `cache_read`, `cache_write`, `free`, `effective_from` (USD/1M tokens) | — | sim (adicionar) |
| `strict_pricing` | modelo sem preço impede subir/salvar | `false` | não |
| `auth.modes` | ordem: `kubernetes`, `tokens`, `none` | `[kubernetes]` | não |
| `auth.audience` | audience do token projetado | `nautilm` | não |
| `auth.tokens.<agente>` | token estático (`${VAR}`) | — | não (use tokens do painel) |
| `agents.<id>.models` | apelidos permitidos (`*` = todos) | — | não |
| `agents.<id>.team` / `cost_center` | entram em cada registro de uso | — | não |
| `agents.<id>.tokens_per_minute` / `tokens_per_day` | cota própria (0 = padrão, -1 = sem limite) | — | não |
| `agents.<id>.on_behalf_of` | pode cobrar em nome de outro (orquestrador) | `false` | não |
| `agents."*"` | regra para qualquer agente autenticado não listado | — | não |
| `masking.disable` / `extra` / `disabled` | regras de mascaramento | todas ligadas | não |
| `quotas.tokens_per_minute` / `tokens_per_day` / `disabled` | padrão de todos os agentes | 200k/min, 10M/dia | não |
| `usage.retention_days` / `disabled` | livro-caixa | 400 dias | não |
| `dashboard.token` | leitura de `/v1/usage` por automação (`${VAR}`) | vazio = desligado | não |
| `dashboard.public_url` | URL que o navegador usa (redirect do Entra e checagem de Origin) | — | não |
| `dashboard.session_ttl` | duração da sessão | `8h` | não |
| `dashboard.provider_secret_prefix` | prefixo das secrets de provedor | `nautilm/providers/` | não |
| `dashboard.entra.tenant_id` / `client_id` / `client_secret` | App Registration (`client_secret: ${VAR}`) | — | não |
| `dashboard.entra.admin_groups` / `viewer_groups` | Object IDs de grupos | — | **sim (adiciona; os do arquivo ficam)** |
| `dashboard.entra.issuer_url` | só para o Entra simulado | login.microsoftonline.com | não |
| `database.url` | `${NAUTILM_DATABASE_URL}` (Secret do K8s) | — | não |
| `database.aws_secret_arn` / `aws_secret_key` | URL do banco no Secrets Manager (texto ou JSON, chave `url`) | — | não |
| `valkey_url` | `${VALKEY_URL}` (`rediss://` no ElastiCache) | vazio = memória | não |

Regras de validação que mais pegam: Entra exige `database` e `public_url`; `database.url` **ou**
`aws_secret_arn`, nunca os dois; `admin_groups` não pode ser vazio; embedding sem fallback;
fallback precisa existir e não pode ser embedding.

## Variáveis de ambiente

| Variável | Uso |
|---|---|
| `NAUTILM_CONFIG` | caminho do config (padrão `config.yaml`; o manifest usa `-config /etc/nautilm/config.yaml`) |
| `VALKEY_URL` | endpoint do ElastiCache com TLS e AUTH |
| `NAUTILM_DASHBOARD_TOKEN` | token de leitura do uso (automação) |
| `NAUTILM_ENTRA_CLIENT_SECRET` | client secret da App Registration |
| `NAUTILM_DATABASE_URL` | só quando `database.url` (lab); na AWS use `aws_secret_arn` |
| `NAUTILM_TOKEN_<AGENTE>` | tokens estáticos (legado; prefira tokens do painel) |
| `AWS_*` padrão do SDK | credenciais vêm do Pod Identity; `AWS_ENDPOINT_URL` só no laboratório (Floci) |
| `NAUTILM_TEST_DATABASE_URL` | só testes do `internal/store` |

## Onde cada coisa mora em produção

| Valor | Onde | Quem cria |
|---|---|---|
| Config base | ConfigMap `nautilm` (namespace `llm-gateway`) | GitOps/PR |
| `VALKEY_URL`, `NAUTILM_DASHBOARD_TOKEN`, `NAUTILM_ENTRA_CLIENT_SECRET` | Secret `nautilm-secrets` | humano (fora do Git) |
| URL do Postgres | Secrets Manager `nautilm/database` | humano |
| Senha do Postgres | Secret `postgres-auth` (**a mesma** da URL) | humano |
| Chave de cada provedor extra | Secrets Manager `nautilm/providers/<nome>` | humano (assistente mostra o comando) |
| Provedores, apelidos, preços e grupos do dia a dia | aba Config (Postgres) | admins do nautilm |
| Agentes, cotas, mascaramento | ConfigMap | PR |

## Valores usados pelos agentes

| Agente usa | Valor |
|---|---|
| Base URL | `http://nautilm.llm-gateway/v1` (dentro do cluster) |
| `model` | um **apelido** (`smart`, `fast`, `embed`…), nunca o ID do modelo |
| Credencial | token projetado da ServiceAccount com `audience: nautilm` (relido a cada chamada) **ou** token `naut_` |
| Header opcional | `X-Nautilm-On-Behalf-Of: <agente>` (só orquestrador); `traceparent` é propagado |
| Marca de cache | `"cache_control": {"type": "ephemeral"}` em partes do conteúdo (precisa `prompt_cache` no apelido) |
