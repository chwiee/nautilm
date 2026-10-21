# Roadmap do pedagio

## Feito (v0.1 — scaffold)

- `POST /v1/chat/completions` OpenAI-compatible, `GET /v1/models`, `/metrics`, `/healthz`
- Apelidos de modelo + fallback (1 nível, só em 429/5xx)
- Providers: `bedrock` (Converse, tool calling, tokens de cache) e `openai` (Ollama, vLLM, LiteLLM…)
- Auth: `kubernetes` (TokenReview), `tokens` (estáticos via env), `none` (dev)
- Permissão por agente (quais apelidos cada um usa)
- Log JSON por chamada com `trace_id` (do `traceparent`) e tokens
- "Stream simulado": aceita `stream: true`, mas só responde depois de receber a resposta inteira

## Próximos passos (ordem sugerida)

1. **Validar na AWS real:** Pod Identity + Bedrock + um agente piloto (kubets). Medir quanto de latência o pedagio adiciona.
2. **Streaming de verdade:** `ConverseStream` no Bedrock e repasse do SSE no provider openai.
3. **Mirante nativo:** o pedagio manda os eventos com a identidade autenticada (hoje: mirante sidecar na frente).
4. **Cotas:** limite de tokens/minuto e orçamento diário por agente (429 com `Retry-After`).
5. **`/v1/embeddings`** (Titan v2) — necessário para RAG/CRAG nos agentes.
6. **Structured output** (`response_format: json_schema`) no Bedrock.
7. **Prompt caching** automático (cachePoint no system prompt e nas tools).
8. **OTel** `gen_ai.*` (spans) além do log.
9. **Recarregar o config** sem reiniciar (ConfigMap).
