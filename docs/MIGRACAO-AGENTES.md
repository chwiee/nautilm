# Migrar um agente para o nautilm

Pré-requisito: nautilm implantado e validado (`docs/IMPLANTACAO-EMPRESA.md`, fases 0 a 8).

Mesmas convenções do runbook: **🧑 HUMANO** = decisão ou ação de uma pessoa; nunca pedir segredo no chat.

## 1. Decisões por agente

| Pergunta | Opções |
|---|---|
| Onde o agente roda? | **mesmo cluster** do nautilm → token da ServiceAccount; **outro cluster/fora** → token `naut_` do painel |
| O cliente relê a credencial a cada chamada? | sim → token projetado (renovado pelo kubelet); não → token `naut_` (vale até 366 dias) |
| Quais apelidos ele usa? | os que existem no nautilm (`smart`, `fast`, `embed`…) |
| Time e centro de custo | entram em cada registro de uso |
| Usa prompt cache (CAG/prefixo grande)? | apelido com `prompt_cache: explicit` + agente mandando `cache_control` |
| Tem mirante na frente? | o agente aponta para o mirante, que aponta para o nautilm |
| Usa embeddings? | trocar o modelo de embedding = **reindexar** (vetores não se comparam) |

## 2. Passo a passo genérico

1. **Registrar o agente no nautilm**
   - ServiceAccount: entrada em `agents` do ConfigMap (`<namespace>/<serviceaccount>`: `models`, `team`,
     `cost_center`, cota se precisar) → PR → `rollout restart`.
   - Token: 🧑 admin cria em **Tokens** (nome, time, centro de custo, apelidos, validade, cota) e grava o
     `naut_…` direto na Secret do agente (mostrado uma vez).
2. **Conferir preço e apelido** na aba Config (apelido testado; nenhum "No price").
3. **Teste isolado** (pod de teste com a mesma ServiceAccount ou `curl` com o token) → 200 com o apelido.
4. **Trocar a configuração do agente** (normalmente só variáveis de ambiente):
   - base URL → `http://nautilm.llm-gateway/v1` (ou o mirante, ver 3);
   - modelo → o **apelido**;
   - credencial → arquivo do token projetado ou `naut_`.
5. **Observar** por um dia: Usage (custo do agente, sem "unpriced"), erros 4xx/5xx, latência, cotas.
6. **Tirar a permissão antiga**: 🧑 remover `bedrock:*`/`InvokeModel` da role do agente (IRSA/Pod Identity)
   depois de estável. A partir daí, **só o nautilm** chama modelos.

↩️ Desfazer = voltar as variáveis de ambiente do agente (por isso a permissão antiga sai só no passo 6).

## 3. Com o mirante na frente

```
agente ──▶ mirante  /p/<agente>/llm/nautilm/v1/...  ──▶  nautilm /v1/...  ──▶ provedor
```
- mirante: `--llm nautilm=http://nautilm.llm-gateway.svc` (ou `MIRANTE_LLM`).
- agente: base URL `http://mirante.<ns>:8080/p/<agente>/llm/nautilm/v1`.
- O mirante repassa a requisição e o `Authorization` como vieram (inclusive partes com `cache_control`)
  e passa a mostrar as chamadas de LLM no mapa.
- **NetworkPolicy**: quem chega ao nautilm é o **pod do mirante**: libere o namespace do mirante.
- Identidade: continua sendo a do token do agente (não a do mirante).

## 4. MidTS

O MidTS hoje usa `LLM_PROVIDER=bedrock` (SDK direto, role própria). Para ir ao nautilm não há mudança de
código (o provider `openai` já envia `LLM_API_KEY` como Bearer e chama `/embeddings`).

**No nautilm** (aba Config ou ConfigMap):
```yaml
models:
  midts-smart: {provider: <bedrock-da-regiao>, model: <modelo aprovado, ex. us.anthropic.claude-sonnet-5-5>,
                max_tokens: 4096, prompt_cache: explicit}
  embed:       {type: embedding, provider: <bedrock-da-regiao>, model: amazon.titan-embed-text-v2:0, dimensions: 1024}
```
- Mantenha o **mesmo modelo** que o MidTS usa hoje e **sem fallback** no início: comportamento idêntico.
- Preços do modelo **com `cache_read` e `cache_write`** (senão o cache é cobrado como input).

**Credencial**: token `naut_` (o MidTS lê `LLM_API_KEY` uma vez, não relê arquivo). 🧑 admin cria o token
`midts` com `midts-smart` e `embed`, time e centro de custo; grava numa Secret do namespace do MidTS.

**Variáveis do MidTS**:
| Variável | Valor |
|---|---|
| `LLM_PROVIDER` | `openai` |
| `LLM_BASE_URL` | `http://mirante.<ns>:8080/p/midts/llm/nautilm/v1` (com mirante) ou `http://nautilm.llm-gateway/v1` |
| `LLM_MODEL` | `midts-smart` |
| `EMBED_MODEL` | `embed` |
| `LLM_API_KEY` | da Secret (token `naut_`) |
| `LLM_PROMPT_CACHE` | `on` (a mensagem `CachedSystem`, com os guias do CAG, vai com `cache_control`) |

**Efeitos esperados**
- O índice do RAG é **refeito uma vez** (o nome do modelo de embedding entra no hash). Custo pequeno com Titan.
- Prompt cache: a partir da 2ª chamada com os mesmos guias, `cached_tokens` > 0 (headers/uso no painel).
  Sem `LLM_PROMPT_CACHE=on` ou sem `prompt_cache` no apelido, o CAG perde o cache: custo e latência sobem.
- O mirante passa a mostrar as chamadas de LLM do MidTS (antes só via MCP).

**Rollback**: voltar `LLM_PROVIDER=bedrock`, `LLM_MODEL`/`EMBED_MODEL` antigos e tirar `LLM_BASE_URL`/`LLM_API_KEY`.
Mantenha a permissão de Bedrock na role do MidTS até a migração estabilizar; depois deixe só S3.

## 5. agentkit e maestro

- **agentkit** ainda lê `PEDAGIO_URL`/`PEDAGIO_TOKEN` (nome antigo): aponte os **valores** para o nautilm
  (URL `/v1` e token). O header `X-Pedagio-Agent` só vale no modo `none` (dev); em produção a identidade
  vem do token. Pendência: renomear para `NAUTILM_*`.
- **maestro** (orquestrador): `agents.<maestro>.on_behalf_of: true` no ConfigMap (nunca em token do
  painel). Para a conta ir para o agente atendido, ele precisa mandar `X-Nautilm-On-Behalf-Of: <agente>`
  (pendência no maestro); sem isso, o roteamento e a síntese ficam na conta do maestro.

## 6. Cliente OpenAI genérico (qualquer linguagem)

```text
base_url = http://nautilm.llm-gateway/v1
api_key  = <conteúdo de /var/run/secrets/nautilm/token>  (relido a cada chamada)  ou  naut_...
model    = <apelido>
```
Manifest de exemplo do token projetado: `deployments/k8s/agent-example.yaml` (`audience: nautilm`,
`expirationSeconds: 3600`). Erros vêm no formato OpenAI; o `code` diz de quem é o limite
(`quota_*` = nautilm, `provider_quota_day` = conta no provedor).

## Checklist de migração (copie por agente)

- [ ] Credencial definida (SA ou `naut_`) e registrada no nautilm
- [ ] Apelidos com preço; `prompt_cache` se o agente usa cache
- [ ] Teste isolado 200
- [ ] Variáveis trocadas; rollback anotado
- [ ] 1 dia observado: custo no Usage, sem `unpriced`, sem 5xx anormal
- [ ] Permissão antiga de Bedrock removida da role do agente
