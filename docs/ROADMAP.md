# Roadmap do pedagio

## Feito

- (v0.1) `POST /v1/chat/completions` OpenAI-compatible, `GET /v1/models`, `/metrics`, `/healthz`
- Apelidos de modelo + fallback (1 nível, só em 429/5xx)
- Providers: `bedrock` (Converse, tool calling, tokens de cache) e `openai` (Ollama, vLLM, LiteLLM…)
- Auth: `kubernetes` (TokenReview), `tokens` (estáticos via env), `none` (dev)
- Permissão por agente (quais apelidos cada um usa)
- Log JSON por chamada com `trace_id` (do `traceparent`) e tokens
- Embeddings (v0.3): `POST /v1/embeddings`, apelidos `type: embedding`, Titan v2 e OpenAI-compatible, sem fallback de propósito
- Streaming de verdade (v0.2): `ConverseStream` no Bedrock e repasse do SSE no provider openai; fallback só antes do 1º byte; métrica de tempo até o 1º token
- Proteções (v0.4, ligadas por padrão): mascaramento de dados sensíveis (9 regras, ajustável), cotas de tokens por agente por minuto/dia (memória ou Valkey, 429 + Retry-After, fail-open), custo por apelido (`preco`) e painel do Grafana

## Próximos passos (ordem sugerida)

1. **Validar na AWS real:** Pod Identity + Bedrock + um agente piloto (kubets). Medir quanto de latência o pedagio adiciona.
2. **Mirante nativo:** o pedagio manda os eventos com a identidade autenticada (hoje: mirante sidecar na frente).
3. ~~Cotas~~ feito (v0.4). Próximo: cota por time (soma de agentes) e alerta antes de estourar.
4. ~~`/v1/embeddings`~~ feito (v0.3)
5. **Structured output** (`response_format: json_schema`) no Bedrock.
6. **Prompt caching** automático (cachePoint no system prompt e nas tools).
7. **OTel** `gen_ai.*` (spans) além do log.
8. **Recarregar o config** sem reiniciar (ConfigMap).
9. **Bedrock Guardrails (PII)** opcional no provider, para o que regex não pega (custo por chamada).
