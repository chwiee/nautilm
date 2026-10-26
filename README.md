# nautilm · Nautilus LLM Gateway

**LLM Gateway** dos agentes: todo tráfego de LLM passa por aqui, só o
nautilm tem credencial nos provedores (Bedrock hoje, API da Anthropic ou
outros amanhã), e ele mantém o **livro-caixa** de quem usou qual modelo,
onde, quantos tokens e quanto custou.

```
 k8s ─────┐                          ┌─► Bedrock (Claude, Nova…)     ← só o nautilm tem IAM
 mid ─────┤                          ├─► API da Anthropic / outros
 marcia ──┼─► nautilm (OpenAI API) ──┤
 claudia ─┤   apelidos · auth ·      └─► Ollama / vLLM / OpenAI-compatible
 maestro ─┘   fallback · mascaramento · cotas · custo · livro-caixa
                       │
                       └─► /dashboard: "última semana: k8s 1M tokens = US$ X"
```

- **Agentes pedem apelidos** (`smart`, `fast`), nunca o ID do modelo. Trocar
  de modelo ou de provedor = mudar uma linha do config.
- **Identidade por agente**: no EKS, o token da própria ServiceAccount
  (TokenReview); fora do cluster, **um token por agente**.
- **Custo por provedor + modelo**, com vigência: o preço é de quem atendeu
  de verdade (inclusive no fallback), e mudar o preço não reescreve o histórico.
- **Livro-caixa** com totais diários por agente × time × centro de custo ×
  provedor × modelo, em Valkey, e um **painel** com CSV para o financeiro.
- **Proteções ligadas por padrão**: mascaramento de dados sensíveis e cotas de tokens.
- **Formato OpenAI**: qualquer SDK/framework funciona; o agente só troca a base URL.

Código, configuração, logs, erros da API e o painel estão em **inglês**; a
documentação do time continua em português.

Antigo **pedagio** (o repositório foi renomeado; o GitHub redireciona o nome
antigo). Por que construir em vez de usar Bifrost ou Agent Router: veja
[docs/adr/0001-construir-vs-comprar.md](docs/adr/0001-construir-vs-comprar.md).
O que falta: [docs/ROADMAP.md](docs/ROADMAP.md).

## Rodar local (com Ollama)

```bash
cp config.example.yaml config.yaml   # ajuste providers/tokens; para dev: auth.modes: [none]
make run
```

```bash
curl -s localhost:8080/v1/chat/completions -H 'X-Nautilm-Agent: me' \
  -d '{"model":"local","messages":[{"role":"user","content":"say hi"}]}'
```

A resposta vem com `"model": "local"` (o apelido). Os headers dizem quem
atendeu e quanto custou: `X-Nautilm-Provider`, `X-Nautilm-Model`,
`X-Nautilm-Cost-USD`, `X-Nautilm-Price-Source` e `X-Nautilm-Billed-To`.

A plataforma inteira (agentes, maestro, Mirante, Grafana) sobe pelo compose do
[plataforma-agentes](https://github.com/chwiee/plataforma-agentes).

## Custo e divisão da conta

### Catálogo de preços: por provedor + modelo, com vigência

O preço é de **onde** a chamada rodou e de **qual modelo** atendeu, não do
apelido: `smart` pode ser Claude no Bedrock hoje e Claude na API da Anthropic
amanhã, e um fallback pode ser atendido por outro modelo em outro lugar.

```yaml
providers:
  bedrock-us-east-2: {type: bedrock, region: us-east-2}   # o NOME do provider é o "onde"
pricing:
  - provider: bedrock-us-east-2
    model: anthropic.claude-sonnet-5-5
    input: 2.20          # USD por 1M tokens (preço regional us-east-2, tabela AWS de 23/10/2026)
    output: 11.00
    cache_read: 0.11     # sem preço de cache = cobra como input (conservador)
    cache_write: 2.75
    effective_from: 2026-10-01
  - provider: ollama-local
    model: "*"           # todos os modelos do provider
    free: true           # infra própria: zero de propósito
strict_pricing: true     # modelo sem preço = erro na subida (recomendado em produção)
```

- **Preço mudou?** Acrescente uma linha com novo `effective_from`. Cada chamada
  usa o preço vigente **no dia em que rodou**; o histórico não muda.
- **Sem preço nunca vira zero calado**: sem `strict_pricing`, a subida avisa e
  as chamadas ficam como **unpriced** (custo desconhecido), com alerta no painel.
  Preço zero exige `free: true`, então esquecer o preço não passa como "grátis".
- **Custo informado pelo provedor vence** a tabela (ex: OpenRouter manda
  `usage.cost`). Origem do preço vai em cada registro: `table`, `provider`,
  `free` ou `unpriced`.
- **Tokens normalizados** para todos os provedores: entrada inclui leitura e
  escrita de cache (semântica da OpenAI). No Bedrock, que informa separado, o
  nautilm soma; o preço de cache é aplicado à parte.
- Só USD (é como os provedores cobram). Converter para BRL é coisa de tela.

### Quem paga

| Situação | Quem paga |
|---|---|
| Agente chama direto | o próprio agente (a identidade autenticada) |
| maestro (com `on_behalf_of: true`) manda `X-Nautilm-On-Behalf-Of: k8s` | **k8s**; o registro guarda `caller: maestro` |
| Agente **sem** `on_behalf_of` manda o header | header **ignorado** (log + métrica): ninguém joga a conta no vizinho |

Cada agente tem `team` e `cost_center` no config; eles entram em cada registro
(se mudarem depois, o histórico não muda). A cota é sempre do **chamador**:
ela protege contra o loop de quem chama.

```yaml
agents:
  k8s:     {models: [smart, fast, embed], team: ce, cost_center: CC-1001}
  maestro: {models: [fast, embed], team: platform, on_behalf_of: true}
```

### Livro-caixa (`usage`)

Cada tentativa de chamada (inclusive as que falharam no fallback, que contam
como erro) soma num total **diário** por dia × agente × chamador × time ×
centro de custo × provedor × modelo.

- **Valkey** (`valkey_url`): um hash por dia com TTL = `usage.retention_days`
  (padrão 400 dias, para comparar o mesmo mês do ano anterior). Sem job de
  limpeza; réplicas somam no mesmo total (`HINCRBY`, atômico).
- **Por que não o Prometheus?** Ele é para operação: ~15 dias, aproximações
  quando perde uma coleta. Dividir conta pede meses de números que fecham.
- **Valkey fora**: a chamada **continua** (200), o registro vai inteiro para o
  log como `usage record lost` (dá para recuperar) e conta em
  `nautilm_usage_lost_total`. O relatório responde 503 em vez de inventar número.
- **Trade-off**: Valkey é rápido e já está na stack, mas não é um banco
  "para sempre". Se o financeiro exigir mais garantia, a interface `usage.Store`
  permite trocar por DynamoDB sem mexer no resto.

### Painel e API

`GET /dashboard` (página embutida no binário, mesma paleta do Mirante):

- período (últimos 7 ou 30 dias, este mês, mês passado ou datas livres);
- agrupar por agente, time, centro de custo, provedor, provedor × modelo,
  agente × provedor × modelo ou agente × chamador;
- custo total e variação contra o período anterior de mesmo tamanho, tokens,
  chamadas e erros, **chamadas sem preço** com alerta;
- custo por grupo, custo por dia, tabela com % do total e **Export CSV**.

Os dados vêm de `GET /v1/usage?from=2026-10-19&to=2026-10-25&group_by=agent,provider`
(JSON) e `GET /v1/usage.csv` (mesmos parâmetros), protegidos por
`dashboard.token`. **Sem token configurado, a API fica desligada** (404): custo
é dado interno. O token do painel não é o de nenhum agente.

> É **estimativa** (tokens informados pelo provedor × tabela). Concilie uma vez
> por mês com a fatura de cada provedor (Cost Explorer no Bedrock, console de
> uso da Anthropic). Diferença grande = tabela desatualizada.

## Proteções (ligadas por padrão)

### Mascaramento de dados sensíveis

Antes de qualquer modelo, o nautilm troca dados sensíveis por marcadores:
`[CPF]`, `[CNPJ]`, `[CREDIT_CARD]` (só número que passa no Luhn), `[EMAIL]`,
`[JWT]`, `[AWS_KEY]`, `Bearer [TOKEN]`, `[PRIVATE_KEY]`, e o valor de `senha=`,
`DB_PASSWORD=`, `"token":"..."` etc. vira `[SECRET]` (a chave fica, para o
modelo entender o contexto).

- Vale para **todas** as mensagens, inclusive resultado e argumentos de tool, e
  para embeddings. Definição de tools e campos de estrutura não são tocados.
- O agente sabe pelo header `X-Nautilm-Masked: cpf=1,secret=2`; log e métrica
  `nautilm_masked_total{agent,rule}` guardam só o **nome** da regra.
- Ajuste: `masking: {disable: [email], extra: [{name: employee_id, regex: 'MAT-\d{6}'}]}`.
- **Trade-off**: é regex. Pega formatos conhecidos com poucos falsos
  positivos, mas não reconhece segredo sem formato ("a senha é batata").

### Cotas por agente

Tokens de entrada + saída por agente, em janelas fixas: **200 mil por minuto** e
**10 milhões por dia** (padrão). Estourou → `429` + `Retry-After`, sem fallback.

- Cota própria em `agents.<id>.tokens_per_minute/tokens_per_day` (`-1` = sem
  limite); padrão de todos em `quotas:`.
- Contador no Valkey (réplicas somam juntas); **fail-open**: Valkey fora = deixa passar e avisa.
- O custo só é conhecido depois da resposta: a última chamada pode passar um pouco do limite.

## Endpoints

| Rota | O que faz |
|---|---|
| `POST /v1/chat/completions` | chat (com ou sem `stream`; tool calling) |
| `POST /v1/embeddings` | embeddings (Titan v2 no Bedrock, `/embeddings` nos compatíveis); **sem fallback** |
| `GET /v1/models` | apelidos que **este** agente pode usar |
| `GET /v1/usage`, `/v1/usage.csv` | livro-caixa (token do painel) |
| `GET /dashboard` | painel de uso e custo |
| `GET /metrics` | Prometheus (operação): requests, tokens, custo, duração, TTFT, fallbacks, bloqueios, mascaramentos, registros perdidos |
| `GET /healthz` | health |

Erros no formato da OpenAI: 401 sem credencial, 403 apelido não permitido,
404 apelido inexistente, 429 limite atingido, 429/5xx do provedor depois do fallback.

Os 429 têm `type: rate_limit_error` e o `code` diz **de quem** é o limite:

| `code` | Limite de quem | Volta quando |
|---|---|---|
| `quota_minute` / `quota_day` | **nautilm** (cota do agente no config) | virada do minuto / meia-noite UTC (`Retry-After`) |
| `provider_quota_day` | **conta no provedor** (ex: quota diária do modelo na conta AWS) | no dia seguinte ou quando a AWS aumentar a quota |
| `ThrottlingException` | provedor, por minuto | em segundos |

No `provider_quota_day` o nautilm **não insiste**: o SDK da AWS não repete a
chamada (a quota diária não volta em segundos) e a mensagem diz o modelo e
que o limite é da conta, não do nautilm. Com `fallback` no apelido, o próximo
modelo é tentado normalmente; sem fallback, o agente recebe o erro.

O config é lido em **modo estrito**: chave desconhecida (erro de digitação ou
chave antiga do pedagio, como `cotas:`) impede a subida com a mensagem dizendo
qual chave usar.

## Streaming

Com `"stream": true` o nautilm repassa os tokens **conforme o modelo gera**
(SSE no formato OpenAI): `ConverseStream` no Bedrock, repasse do SSE nos
compatíveis com OpenAI.

- **Fallback só antes do 1º byte.** Depois do primeiro chunk o 200 já saiu: um
  erro vira um evento `data: {"error": {...}}` e o stream termina sem `[DONE]`.
- Tokens de um stream que quebrou no meio **são cobrados** (o provedor cobra).
- `nautilm_time_to_first_token_seconds`: a latência que o usuário sente.
- Atrás de nginx/ingress, o nautilm manda `X-Accel-Buffering: no`.

## Como um agente usa

Qualquer cliente OpenAI, apontando a base URL para o nautilm e mandando o
token. No EKS: [deployments/k8s/agent-example.yaml](deployments/k8s/agent-example.yaml)
(token projetado com `audience: nautilm`); o agente **não precisa de role IAM**.
O agentkit ainda lê `PEDAGIO_URL`/`PEDAGIO_TOKEN`: o valor é o do nautilm.

## Deploy no EKS

[deployments/k8s/nautilm.yaml](deployments/k8s/nautilm.yaml): Namespace,
ServiceAccount (Pod Identity → role com `bedrock:InvokeModel`), ClusterRoleBinding
`system:auth-delegator`, ConfigMap (com `strict_pricing: true`), Secret
(`VALKEY_URL` do ElastiCache e `NAUTILM_DASHBOARD_TOKEN`), Deployment (2 réplicas,
nonroot, read-only), PDB e Service. O painel do Grafana de operação está em
[deployments/grafana/nautilm.json](deployments/grafana/nautilm.json).

## Estrutura

```
cmd/nautilm/              main: config, providers, auth, Valkey
internal/config/          YAML estrito, apelidos, permissões, cobrança, cotas
internal/auth/            kubernetes (TokenReview), tokens, none
internal/gateway/         HTTP, fallback, SSE, mascaramento, cotas, cobrança, painel
internal/pricing/         catálogo de preços por provedor+modelo com vigência
internal/usage/           livro-caixa (memória/Valkey) e relatórios (JSON/CSV)
internal/quota/           cotas (memória/Valkey)
internal/mask/            regras de mascaramento
internal/provider/        contrato Provider + bedrock + openaicompat
internal/metrics/         contadores Prometheus sem dependência
internal/valkey/          conexão compartilhada
pkg/openai/               tipos do formato OpenAI (agentes Go podem importar)
deployments/              k8s e Grafana
docs/                     ADR e roadmap
```

## Desenvolvimento

```bash
make test         # go test ./...
make test-valkey  # integração com Valkey (cotas e livro-caixa); VALKEY_URL=redis://localhost:6379
make lint         # go vet ./...
make docker       # imagem distroless nonroot
```
