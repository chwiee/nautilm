# ADR 0001 — LLM Gateway: Bifrost, Agent Router (ex-Envoy AI Gateway) ou o nosso

- **Data:** 2026-10-21
- **Status:** aceito (revisar depois do piloto)

## Contexto

Hoje cada time tem IRSA própria com `bedrock:InvokeModel` e o ID do modelo
espalhado no código. Trocar de modelo ou de provedor obriga a mexer em todos
os agentes. Queremos:

1. só o gateway com permissão no Bedrock;
2. agentes pedindo **apelidos** (`smart`, `fast`), não IDs de modelo;
3. identidade do agente **sem segredo distribuído** (ServiceAccount do K8s);
4. custo/tokens por agente e integração com o Mirante;
5. tudo em Go, fácil de entender e operar pelo time.

## Opções

| | **Bifrost** (maximhq) | **Agent Router** (ex-Envoy AI Gateway) | **nautilm (ex-pedagio)** (nosso, Go) |
|---|---|---|---|
| Licença / linguagem | Apache-2.0, Go | Apache-2.0, Go (controller) + Envoy (C++) | nosso, Go stdlib + AWS SDK |
| API para os agentes | OpenAI-compatible | OpenAI-compatible | OpenAI-compatible |
| Bedrock | Converse, tool calling, streaming, embeddings, prompt caching | Converse + InvokeModel (Claude), embeddings Titan | Converse + ConverseStream, tool calling |
| Credencial AWS | cadeia padrão (IRSA documentado) | cadeia padrão (IRSA e Pod Identity) | cadeia padrão (Pod Identity, IRSA) |
| Identidade do agente | virtual keys (segredo por agente) | JWT/OIDC via SecurityPolicy do Envoy | **TokenReview da ServiceAccount** (sem segredo) |
| Cota / orçamento | sim (virtual keys, budgets) | rate limit por tokens | não ainda (roadmap) |
| Fallback | sim | sim | sim (1 nível, só em 429/5xx) |
| MCP gateway | sim (parte enterprise) | sim (MCPRoute) | fora de escopo (o Mirante já faz proxy MCP) |
| Peso operacional | 1 Deployment + store (config/log) | Envoy Gateway + CRDs + Gateway API em cada cluster | 1 Deployment, sem estado |
| Mirante | precisaria de plugin | via OTel/access log | nativo (próximo passo) ou mirante na frente |
| Curva de aprendizado | média | alta (Envoy, Gateway API, ext_proc) | baixa (código é nosso) |

Observações:
- Os números de performance do Bifrost (µs de overhead) são **do fornecedor**;
  não achei benchmark independente.
- O Envoy AI Gateway virou **Agent Router** (Agentic AI Foundation), com os
  mesmos CRDs (`AIGatewayRoute`, `aigateway.envoyproxy.io`).
- Guardrails, clustering e MCP gateway do Bifrost estão na parte enterprise.

## Decisão

**Construir o nautilm (ex-pedagio)**, fino, em Go, mantendo o **contrato OpenAI-compatible
+ apelidos** como a coisa que realmente importa.

Por quê:
- O requisito que mais pesa (identidade por ServiceAccount, sem segredo) não
  vem pronto em nenhum dos dois.
- O Agent Router é ótimo se a empresa **já** roda Envoy Gateway; não roda, e
  subir Envoy + CRDs em cada cluster é muito peso para um time pequeno.
- O Bifrost resolve rápido, mas traz virtual keys (segredo por agente) e
  parte das features que queremos fica no enterprise.
- Já temos ~60% do código (tradução Converse do gotoolkit, proxy do mirante).

## Consequências

- **Bom:** controle total, Mirante nativo, sem dependência de fornecedor,
  binário pequeno sem estado.
- **Ruim:** somos donos dos bugs de tradução (streaming, tool calling,
  structured output, caching) e da alta disponibilidade.
- **Saída de emergência:** como os agentes só conhecem *URL + apelido no
  formato OpenAI*, dá para trocar o nautilm (ex-pedagio) por Bifrost/Agent Router depois
  **sem mexer em nenhum agente**. Rever esta decisão se a tradução virar um
  fardo ou se a empresa adotar Envoy Gateway.

## Fontes

- Bifrost: https://github.com/maximhq/bifrost — provider Bedrock: https://docs.getbifrost.ai/providers/supported-providers/bedrock
- Agent Router (ex-Envoy AI Gateway): https://theagentrouter.ai/docs — release v0.6: https://aigateway.envoyproxy.io/release-notes/v0.6
