# pedagio

**LLM Gateway** dos agentes: todo tráfego de LLM passa por aqui, e só o
pedagio tem credencial no Bedrock.

```
 kubets ─┐                         ┌─► Bedrock (Claude, Nova…)   ← só o pedagio tem IAM
 midts  ─┼─► pedagio  (OpenAI API) ┤
 ce-cli ─┘   apelidos · auth ·     └─► Ollama / vLLM / OpenAI-compatible
             fallback · tokens
```

- **Agentes pedem apelidos** (`smart`, `fast`), nunca o ID do modelo. Trocar
  de modelo = mudar uma linha do config.
- **Identidade sem segredo**: no EKS o agente manda o token da própria
  ServiceAccount; o pedagio valida com TokenReview (`namespace/serviceaccount`).
- **Formato OpenAI**: qualquer SDK/framework funciona; o agente só troca a base URL.
- **Fallback** automático em 429/5xx, **métricas** de tokens por agente em `/metrics`.

Por que construir em vez de usar Bifrost ou Agent Router (ex-Envoy AI
Gateway): veja [docs/adr/0001-construir-vs-comprar.md](docs/adr/0001-construir-vs-comprar.md).
O que falta: [docs/ROADMAP.md](docs/ROADMAP.md).

## Rodar local (com Ollama)

```bash
cp config.example.yaml config.yaml   # para dev: auth.modes: [none] e só o provider ollama
make run
```

```bash
curl -s localhost:8080/v1/chat/completions -H 'X-Pedagio-Agent: eu' \
  -d '{"model":"local","messages":[{"role":"user","content":"diga oi"}]}'
```

A resposta vem com `"model": "local"` (o apelido) e os headers
`X-Pedagio-Provider` / `X-Pedagio-Model` dizem quem atendeu de verdade.

## Como um agente usa

Qualquer cliente OpenAI. Em Go com o `llm.Client` da trilha-ia-go/gotoolkit:

```go
c := llm.NewOpenAI(llm.OpenAIConfig{
    BaseURL: "http://pedagio.llm-gateway/v1",
    Model:   "smart",                          // apelido
    APIKey:  readFile("/var/run/secrets/pedagio/token"), // token da ServiceAccount
})
```

No EKS: [deployments/k8s/agente-exemplo.yaml](deployments/k8s/agente-exemplo.yaml)
(token projetado com `audience: pedagio`). O agente **não precisa de role IAM**.
Como o token é renovado pelo kubelet, releia o arquivo periodicamente.

## Endpoints

| Rota | O que faz |
|---|---|
| `POST /v1/chat/completions` | chat (com ou sem `stream`; tool calling) |
| `POST /v1/embeddings` | embeddings (Titan v2 no Bedrock, `/embeddings` nos compatíveis com OpenAI); **sem fallback** |
| `GET /v1/models` | apelidos que **este** agente pode usar |
| `GET /metrics` | Prometheus: requests, tokens (input/output/cached), custo estimado, duração, fallbacks, rejeições (inclusive cota), mascaramentos |
| `GET /healthz` | health |

Erros seguem o formato da OpenAI (`{"error":{"message","type","code"}}`):
401 sem credencial, 403 apelido não permitido, 404 apelido inexistente, 429 cota do agente estourada,
429/5xx repassados do provedor depois de esgotar o fallback.

## Proteções (ligadas por padrão)

Nenhuma precisa de config para funcionar. O bloco no YAML serve só para
**ajustar** (veja [config.example.yaml](config.example.yaml)).

### Mascaramento de dados sensíveis

Antes de qualquer modelo, o pedagio troca dados sensíveis por marcadores:
`[CPF]`, `[CNPJ]`, `[CARTAO]` (só número que passa no Luhn), `[EMAIL]`,
`[JWT]`, `[AWS_KEY]`, `Bearer [TOKEN]`, `[CHAVE_PRIVADA]`, e o valor de
`senha=`, `DB_PASSWORD=`, `"token":"..."` etc. vira `[SEGREDO]` (a chave fica,
para o modelo entender o contexto).

- Vale para **todas** as mensagens, inclusive resultado de tool e argumentos
  de tool_call (é ali que log com segredo costuma entrar), e para embeddings.
- Definição de tools e campos de estrutura (`role`, `tool_call_id`...) não são tocados.
- O agente sabe pelo header `X-Pedagio-Masked: cpf=1,segredo=2`; o log e a
  métrica `pedagio_masked_total{agent,rule}` guardam só o **nome** da regra, nunca o valor.
- Ajuste: `mascarar: {desligar: [email], extra: [{nome: matricula, regex: 'MAT-\d{6}'}]}`.
  Regra com nome errado derruba a subida com mensagem clara.

Trade-off: é regex, não um classificador. Pega formatos conhecidos com
poucos falsos positivos (os testes garantem que `CrashLoopBackOff`, códigos
`AMQ2058` e números de pedido passam intactos), mas não reconhece um segredo
sem formato ("a senha do banco é batata"). Para isso existe o Bedrock
Guardrails (PII), que dá para ligar no provider depois, com custo por chamada.

### Cotas por agente

Tokens de entrada + saída, por agente, em duas janelas fixas: **200 mil por
minuto** e **10 milhões por dia** (padrão). Estourou → `429` com
`Retry-After` e **sem fallback** (outro modelo gastaria do mesmo orçamento).

- Cota própria por agente em `agents.<id>.tokens_por_minuto/tokens_por_dia`
  (`-1` = sem limite). Padrão de todos em `cotas:`.
- Contador em memória (1 réplica) ou no Valkey (`cotas.valkey_url`: as
  réplicas somam no mesmo contador com `INCRBY`, atômico).
- **Fail-open:** Valkey fora = as cotas deixam passar e o log avisa. A cota
  protege custo; não pode derrubar os agentes.
- O custo só é conhecido depois da resposta, então a última chamada pode
  passar um pouco do limite (sem tokenizer por modelo, de propósito).
- Header `X-Pedagio-Quota-Day-Remaining` e métrica
  `pedagio_rejected_total{reason="quota_minute|quota_day"}`.

### Custo e painel do Grafana

Cada apelido pode ter preço (USD por **milhão** de tokens, da tabela do provedor):

```yaml
models:
  smart: {provider: bedrock, model: ..., preco: {entrada: 3.00, saida: 15.00}}
```

O pedagio soma `pedagio_cost_usd_total{agent,alias,model}` a cada chamada
(inclusive embeddings). Preço por apelido, e não um preço único no painel,
porque `smart` e `fast` costumam diferir 10×+ e trocar de modelo já é mudar
essa mesma linha. Sem `preco` = custo 0 (ex: Ollama). É **estimativa**: a
fatura oficial continua no Cost Explorer (tokens em cache, descontos e
arredondamentos ficam de fora).

[deployments/grafana/pedagio.json](deployments/grafana/pedagio.json) é o
painel pronto (fonte de dados como variável, serve em qualquer Grafana):
custo no período, tokens, % de erro, **custo por agente**, tokens/min por
agente (compare com a cota), latência e TTFT por apelido, bloqueios por
motivo (cota, permissão), mascaramentos por regra, fallbacks e uma tabela
agente × apelido × modelo. No compose do
[plataforma-agentes](https://github.com/chwiee/plataforma-agentes) ele já sobe provisionado.

## Streaming

Com `"stream": true` o pedagio repassa os tokens **conforme o modelo gera**
(SSE no formato OpenAI): `ConverseStream` no Bedrock, repasse do SSE nos
servidores compatíveis com OpenAI.

- **Fallback só antes do 1º byte.** Se o modelo falhar ao abrir (429, 5xx,
  conexão), o pedagio tenta o próximo apelido sem o agente perceber. Depois
  que o primeiro chunk saiu, o status 200 já foi enviado: um erro vira um
  evento `data: {"error": {...}}` e o stream termina sem `[DONE]`.
- **Usage:** o pedagio sempre conta os tokens (pede `include_usage` ao
  upstream); o chunk de usage só vai ao agente se ele pediu
  `stream_options.include_usage`.
- **Campos extras** do upstream (ex: `reasoning_content`) passam intactos;
  só `id` e `model` (o apelido) são trocados.
- `/metrics` ganha `pedagio_time_to_first_token_seconds` — a latência que o
  usuário sente no chat.
- O `timeout` do provider `openai` vale para a resposta **inteira**, inclusive
  o stream: ajuste para respostas longas em CPU.
- Atrás de nginx/ingress, o pedagio manda `X-Accel-Buffering: no` para o
  proxy não segurar o stream em buffer.

## Mirante

Hoje: coloque o mirante (modo sidecar) na frente — o agente aponta para
`http://mirante:8080/p/<agente>/llm/pedagio/v1` e o mirante repassa ao
pedagio. Próximo passo: o pedagio mandar os eventos direto, já com a
identidade autenticada (ver ROADMAP).

## Deploy no EKS

[deployments/k8s/pedagio.yaml](deployments/k8s/pedagio.yaml): Namespace,
ServiceAccount (com Pod Identity → role com `bedrock:InvokeModel`),
ClusterRoleBinding `system:auth-delegator` (para o TokenReview), ConfigMap,
Deployment (2 réplicas, nonroot, read-only), PDB e Service.

## Estrutura

```
cmd/pedagio/              main: lê config, monta providers e auth
internal/config/          YAML, apelidos, permissões, fallback
internal/auth/            kubernetes (TokenReview), tokens, none
internal/gateway/         HTTP, roteamento, fallback, SSE
internal/provider/        contrato Provider + bedrock + openaicompat
internal/metrics/         contadores Prometheus sem dependência
pkg/openai/               tipos do formato OpenAI (agentes Go podem importar)
deployments/k8s/          manifests
docs/                     ADR e roadmap
```

## Desenvolvimento

```bash
make test    # go test ./...
make lint    # go vet ./...
make docker  # imagem distroless nonroot
```
