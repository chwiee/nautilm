# Roadmap do pedagio

## Feito

- (v0.1) `POST /v1/chat/completions` OpenAI-compatible, `GET /v1/models`, `/metrics`, `/healthz`
- Apelidos de modelo + fallback (1 nível, só em 429/5xx)
- Providers: `bedrock` (Converse, tool calling, tokens de cache) e `openai` (Ollama, vLLM, LiteLLM…)
- Auth: `kubernetes` (TokenReview), `tokens` (estáticos via env), `none` (dev)
- Permissão por agente (quais apelidos cada um usa)
- Log JSON por chamada com `trace_id` (do `traceparent`) e tokens
- Streaming de verdade (v0.2): `ConverseStream` no Bedrock e repasse do SSE no provider openai; fallback só antes do 1º byte; métrica de tempo até o 1º token

## Próximos passos (ordem sugerida)

1. **Validar na AWS real:** Pod Identity + Bedrock + um agente piloto (kubets). Medir quanto de latência o pedagio adiciona.
2. **Mirante nativo:** o pedagio manda os eventos com a identidade autenticada (hoje: mirante sidecar na frente).
3. **Cotas:** limite de tokens/minuto e orçamento diário por agente (429 com `Retry-After`).
4. **`/v1/embeddings`** (Titan v2) — necessário para RAG/CRAG nos agentes.
5. **Structured output** (`response_format: json_schema`) no Bedrock.
6. **Prompt caching** automático (cachePoint no system prompt e nas tools).
7. **OTel** `gen_ai.*` (spans) além do log.
8. **Recarregar o config** sem reiniciar (ConfigMap).
