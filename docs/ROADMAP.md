# Roadmap do nautilm (Nautilus LLM Gateway)

## Feito

- (v0.1) `POST /v1/chat/completions` OpenAI-compatible, `GET /v1/models`, `/metrics`, `/healthz`
- Apelidos de modelo + fallback (1 nível, só em 429/5xx)
- Providers: `bedrock` (Converse, tool calling, tokens de cache) e `openai` (Ollama, vLLM, LiteLLM, OpenRouter…)
- Auth: `kubernetes` (TokenReview), `tokens` (um por agente, via env), `none` (dev)
- Permissão por agente (quais apelidos cada um usa)
- Log JSON por chamada com `trace_id` (do `traceparent`), tokens e custo
- (v0.2) Streaming de verdade: `ConverseStream` no Bedrock e repasse do SSE; fallback só antes do 1º byte; tempo até o 1º token
- (v0.3) Embeddings: `POST /v1/embeddings`, Titan v2 e OpenAI-compatible, sem fallback de propósito
- (v0.4) Proteções ligadas por padrão: mascaramento de dados sensíveis e cotas por agente (memória/Valkey, 429, fail-open); painel do Grafana
- (v0.5) **pedagio → nautilm**, código/config/logs/painel em inglês, config em modo estrito (avisa chave antiga)
- (v0.5) **Custo e divisão da conta**: catálogo de preços por provedor + modelo com vigência (cache read/write, `free`, `strict_pricing`, custo informado pelo provedor vence); livro-caixa diário por agente × chamador × time × centro de custo × provedor × modelo (memória/Valkey, retenção 400 dias, registro perdido vai para o log); `X-Nautilm-On-Behalf-Of` só para quem tem `on_behalf_of`; `/v1/usage` (JSON/CSV) e `/dashboard`
- (v0.5) Erro `provider_quota_day`: quota diária da conta no provedor, sem retry; permissões documentadas e validadas no IAM Access Analyzer
- (v0.6) **Login Entra ID** (OIDC + PKCE, grupos admin/viewer, overage negado com dica), **tokens de aplicação pelo painel** (hash, mostrado uma vez, validade, cotas e apelidos próprios, rotação sem downtime, revogação), **auditoria**; PostgreSQL em pod com PVC + backup; URL do banco no Secrets Manager (local: Secret do cluster); laboratório kind com Entra simulado

## Próximos passos (ordem sugerida)

1. **Validar na AWS real:** Pod Identity + Bedrock + ElastiCache + um agente piloto (kubets). Medir a latência que o nautilm adiciona e conciliar o custo estimado com o Cost Explorer.
2. **maestro mandar `X-Nautilm-On-Behalf-Of`**: atribuir roteamento e síntese ao agente atendido (o nautilm já aceita; falta o maestro enviar).
3. **Provider `anthropic`** (Messages API direta), para usar a API da Claude sem passar pelo Bedrock; o catálogo e o painel já estão prontos para ele.
4. **Conciliação automática**: puxar a fatura (Cost Explorer por modelo; Usage & Cost API da Anthropic) e mostrar "estimado × fatura" por provedor no painel.
5. **Application inference profiles do Bedrock por agente** (com tag de custo): custo real por agente no Cost Explorer, além da estimativa.
6. **Orçamento por time** (soma dos agentes) com alerta antes de estourar.
7. **Mirante nativo:** o nautilm manda os eventos com a identidade autenticada.
8. **Structured output** (`response_format: json_schema`) e **prompt caching** automático no Bedrock.
9. **OTel** `gen_ai.*` (spans) além do log; **recarregar o config** sem reiniciar.
10. **Bedrock Guardrails (PII)** opcional, para o que regex não pega (custo por chamada).
11. **Livro-caixa em DynamoDB** se o financeiro exigir garantia maior que a do Valkey.
12. **Painel**: aviso de token perto de expirar (e do client secret do Entra); editar cotas/apelidos de um token sem rotacionar.
13. **Banco gerenciado** (RDS/Aurora PostgreSQL) se o nautilm virar peça crítica de muitos times; hoje é só trocar a URL no segredo.
