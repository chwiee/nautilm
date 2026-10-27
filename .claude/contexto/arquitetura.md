# Arquitetura do nautilm

## Visão geral

```
                    ┌──────────────── nautilm (Deployment, 2+ réplicas, sem estado local) ────────────────┐
agente ─ OpenAI ──▶ │ auth → corpo → mascaramento → apelido → permissão → cota → provedor (+fallback)      │ ──▶ Bedrock
 (SA token ou       │        → registra uso/custo de CADA tentativa → resposta JSON ou SSE                  │ ──▶ OpenAI-compatível
  token naut_)      │ painel /dashboard · login Entra · /v1/admin/* · aba Config                             │
                    └───────┬───────────────┬──────────────────────┬────────────────────────┬───────────────┘
                            │               │                      │                        │
                  API do Kubernetes    Valkey (ElastiCache)   PostgreSQL (pod+PVC)     Secrets Manager
                  (TokenReview)        cotas + livro-caixa    tokens, sessões,         URL do banco e chaves
                                                              auditoria, revisões      de provedores
```

O **mirante** pode ficar entre o agente e o nautilm (`/p/<agente>/llm/nautilm`): ele só observa e
repassa a requisição como veio.

## Fluxo de `POST /v1/chat/completions`

1. **Autenticar** (`internal/auth`, cadeia em ordem):
   - `naut_...` → tokens criados no painel (hash no Postgres, cache de 15 s; banco fora = **503**, não 401);
   - modos do config (`auth.modes`): `kubernetes` (TokenReview, identidade `namespace/serviceaccount`,
     cache 1 min, **só no mesmo cluster** do nautilm), `tokens` (estáticos via env), `none` (dev).
2. **Ler o corpo** (até 10 MB) e **mascarar** dados sensíveis no JSON bruto; a requisição é remontada
   a partir dele, então provedor Bedrock (struct) e OpenAI-compatível (JSON bruto) recebem o mesmo texto limpo.
3. **Retrato da config** (`g.snap()`): a requisição usa um único `live.State` do começo ao fim.
4. **Apelido** existe? É do tipo certo (chat × embedding)? O agente pode usá-lo? (404/400/403)
5. **Cota** do agente (por minuto e por dia; 429 com `Retry-After`; Valkey fora = deixa passar).
6. **Quem paga**: o chamador, ou o agente do header `X-Nautilm-On-Behalf-Of` se o chamador tiver
   `on_behalf_of: true` (só o orquestrador).
7. **Provedor + fallback**: tenta o apelido e depois os `fallback` **só em 429/5xx** (4xx é culpa da
   requisição). No streaming, fallback **só antes do 1º byte**.
8. **Registrar cada tentativa**: métricas, custo (`pricing`: custo informado pelo provedor vence a tabela;
   sem preço = `unpriced`) e livro-caixa (Valkey; falhou = registro completo no log + métrica).
9. **Responder**: ao agente o `model` é sempre o apelido; o modelo real e o custo vão em headers
   `X-Nautilm-Provider/Model/Cost-USD`.

`/v1/embeddings` segue as mesmas regras, **sem fallback**.

## Config em vigor: arquivo + aba Config (`internal/live`)

- O **arquivo** (`config.yaml`, ConfigMap) é a base, lida em modo estrito.
- A **aba Config** salva **revisões append-only** no Postgres (`config_revisions`), uma por tipo:
  `providers`, `models`, `pricing`, `groups`, cada uma com o conjunto **inteiro** daquele tipo.
- `live.Merge` aplica as revisões mais novas **sobre uma cópia** do arquivo e roda o mesmo `Validate`
  da subida. O que vem do arquivo é somente leitura na tela; o painel adiciona ou sobrepõe.
- Troca atômica (`atomic.Pointer`): a réplica que salvou aplica na hora; as outras consultam
  `max(id)` a cada **5 s**. Chaves de provedor (Secrets Manager) são relidas a cada **10 min**.
- Edição concorrente: cada save informa a revisão-pai; se mudou, **409** (trava por tipo com advisory lock).
- Revisão que ficou inválida na subida (ex.: o arquivo tirou um provedor usado pelo painel): o nautilm
  sobe **só com o arquivo** e mostra o erro na aba Config. Nunca deixa de subir por causa do painel.
- Grupos do Entra do arquivo **sempre** valem (o painel só adiciona): ninguém fica trancado para fora.

## Dados e onde moram

| Dado | Onde | Perda |
|---|---|---|
| Cotas (contadores) | Valkey (memória se vazio) | sem Valkey: cotas deixam passar (fail-open) |
| Livro-caixa (totais diários, 400 dias) | Valkey | Valkey fora: registro vai para o log (`nautilm_usage_lost_total`) |
| Tokens `naut_` (hash), sessões (hash), auditoria, revisões da aba Config | PostgreSQL | sem banco: painel/admin indisponíveis; tokens `naut_` → 503 |
| URL do banco, chaves de provedores | Secrets Manager (ou Secret do K8s no lab) | só o ARN fica no banco |
| Config base | ConfigMap | — |

## Decisões e trade-offs

| Decisão | Por quê | Custo |
|---|---|---|
| Construir em Go em vez de Bifrost/Agent Router (ADR 0001) | identidade por ServiceAccount, código simples, integração com mirante | manter o código |
| Apelidos em vez de IDs de modelo | trocar modelo sem mexer em agente | agente não escolhe modelo fino |
| Fallback de 1 nível, só 429/5xx | previsível e fácil de explicar | sem cascata longa |
| Mascaramento por regex | rápido, sem custo por chamada | não pega segredo sem formato |
| Livro-caixa no Valkey | já está na stack, rápido | não é "para sempre" (interface permite DynamoDB) |
| Postgres em pod com PVC | barato e simples | backup é nosso (pg_dump + snapshot EBS); RDS é só trocar a URL |
| Revisões append-only | histórico e rollback grátis | mudança pelo painel não passa por PR (auditoria compensa) |
| Aba Config valida + testa antes de salvar | config salva sempre funciona | o teste gasta poucos tokens (`nautilm-config-test`) |
| Papéis recalculados a cada requisição | remover grupo corta acesso na hora | 1 leitura de sessão por requisição do painel |
| Prompt cache opt-in por apelido (`prompt_cache`) | modelos sem suporte recusam `cachePoint` | admin precisa ligar no apelido |

## Limites conhecidos

- Só texto (sem imagens); `response_format: json_schema` não suportado no Bedrock ainda.
- `kubernetes` auth só valida tokens do **próprio cluster**; agentes de outros clusters usam tokens `naut_`.
- Sem provider nativo da API da Anthropic (roadmap); dá para usar a API dela via OpenAI-compatível se o
  endpoint compatível atender.
- Sem recarga do **arquivo** sem reiniciar (a aba Config cobre o dia a dia).
