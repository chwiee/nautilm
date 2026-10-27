# Estado atual e roadmap

Histórico de versões: `docs/ROADMAP.md`. Última atualização deste arquivo: 2026-10-27 (v0.8).

## Validado

| O quê | Como |
|---|---|
| Gateway (chat, SSE, embeddings, fallback, mascaramento, cotas, custo, livro-caixa) | testes + compose da plataforma-agentes + Ollama |
| Erros reais do Bedrock (quota diária da conta = `provider_quota_day`) | conta de laboratório us-east-2 |
| Login Entra ID **real** (admin e viewer), tokens `naut_`, auditoria | kind local + tenant de teste do dono |
| Postgres em pod: migrações, restart sem perda, backup `pg_dump` | kind local |
| Aba Config: assistente lendo secret de verdade (Floci), apelido testado, 2 réplicas, rollback | kind local + navegador |
| Prompt cache: tradução de marcas para `cachePoint` | testes unitários e de gateway |

## NÃO validado ainda (primeira prova será o piloto na empresa)

- Bedrock com quota > 0: custo real × Cost Explorer, latência adicionada, **prompt cache de verdade**.
- IAM real: `AccessDenied` do assistente (o Floci não aplica IAM), Pod Identity, statement `nautilm/providers/*`
  (revalidar no Access Analyzer).
- ElastiCache com TLS, VPC endpoints, Ingress/ALB HTTPS, App Registration do tenant da empresa.
- Postgres em EBS gp3 e NetworkPolicy aplicada pelo CNI.

## Pendências conhecidas

- **Imagem**: não há CI nem imagem publicada (`ghcr.io/chwiee/nautilm:0.5.0` no manifest não existe).
  A organização deve construir e enviar ao próprio ECR (`docs/IMPLANTACAO-EMPRESA.md`, fase 2).
- Sem manifest de Ingress e de NetworkPolicy de entrada do nautilm (dependem da empresa).
- agentkit ainda lê `PEDAGIO_URL`/`PEDAGIO_TOKEN` e manda `X-Pedagio-Agent` (aceito como legado).
- maestro ainda não manda `X-Nautilm-On-Behalf-Of`.
- MidTS lê a chave do LLM uma vez da variável de ambiente: atrás do nautilm usa token `naut_`
  (não o token projetado, que é renovado em arquivo).
- O client secret do App Registration de teste do dono foi exposto em chat: **rotacionar**.

## Próximos passos (ordem sugerida)

1. Piloto na conta da empresa (`docs/IMPLANTACAO-EMPRESA.md`).
2. Migrar o MidTS (`docs/MIGRACAO-AGENTES.md`), com `LLM_PROMPT_CACHE=on` + `prompt_cache: explicit`.
3. agentkit/maestro: renomear `PEDAGIO_*` → `NAUTILM_*`, maestro enviando on-behalf-of.
4. Provider `anthropic` nativo; conciliação automática com a fatura; orçamento por time.
