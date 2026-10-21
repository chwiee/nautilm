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
| `GET /v1/models` | apelidos que **este** agente pode usar |
| `GET /metrics` | Prometheus: requests, tokens (input/output/cached), duração, fallbacks, rejeições |
| `GET /healthz` | health |

Erros seguem o formato da OpenAI (`{"error":{"message","type","code"}}`):
401 sem credencial, 403 apelido não permitido, 404 apelido inexistente,
429/5xx repassados do provedor depois de esgotar o fallback.

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
