# nautilm — contexto do projeto para assistentes de IA

> Porta de entrada para qualquer IA (Claude Code, Kiro, Copilot, Codex…) que for **entender, mudar ou
> implantar** este repositório. Leia este arquivo inteiro antes de agir; os detalhes estão nos links.

## O que é

**nautilm (Nautilus LLM Gateway)** é o gateway de LLM da plataforma de agentes: todo agente fala com
ele no **formato OpenAI** (`/v1/chat/completions`, `/v1/embeddings`) e **só o nautilm** tem credencial
nos provedores (Amazon Bedrock, e qualquer API OpenAI-compatível). Em troca, a organização ganha:

- **apelidos** (`smart`, `fast`, `embed`): trocar de modelo ou de provedor sem mexer em agente;
- **identidade sem segredo** no EKS (token da ServiceAccount, `TokenReview`) ou tokens `naut_` criados no painel;
- **custo por agente/time/centro de custo** (livro-caixa diário, preço por provedor + modelo, com vigência);
- **proteções ligadas por padrão**: mascaramento de dados sensíveis, cotas de tokens, fallback 1 nível;
- **painel** com login **Microsoft Entra ID** (admin/viewer), tokens de aplicação, auditoria e a
  **aba Config** (provedores, apelidos, preços e grupos sem reiniciar, com teste, histórico e rollback);
- **prompt cache do Bedrock** (marcas `cache_control` viram `cachePoint`).

- Módulo Go: `github.com/chwiee/nautilm` · Go 1.26 · repositório **público** https://github.com/chwiee/nautilm (`main`).
- Faz parte da plataforma de agentes junto com **mirante** (painel/telão dos agentes), **maestro**
  (orquestrador), **agentkit** (agente padrão) e **MidTS** (agente de middleware).

## Regras que não se negociam

1. **Só Go.** Nada de Python/Node no projeto. Scripts de apoio só em bash curto (`deployments/local-kind`).
2. **Idioma:** código, config, logs, mensagens de API e textos do painel em **inglês**. README, `docs/`
   e este contexto em **português do Brasil**. Conversa com o time em pt-BR.
3. **Nenhum segredo no Git nem no banco.** Valores vêm de `${ENV}` (Kubernetes Secret) ou do AWS Secrets
   Manager (só o ARN fica no banco). Tokens `naut_` só como hash SHA-256. IDs reais (conta, tenant,
   grupos) nunca no repositório: use `<ACCOUNT_ID>`, `<tenant-id>`… Nunca peça segredo no chat.
4. **Só o nautilm invoca modelos.** Agentes não recebem role IAM de Bedrock. A política IAM é por ARN
   de modelo (nada de `bedrock:*` nem `Resource: "*"` em invocação).
5. **Config em modo estrito.** Chave desconhecida no YAML impede a subida (de propósito). Toda mudança
   (arquivo ou aba Config) passa pelo mesmo `config.Validate`.
6. **Modelo sem preço nunca é zero:** vira `unpriced`. Em produção use `strict_pricing: true`.
7. **Embeddings nunca têm fallback** (vetores de modelos diferentes não se comparam).
8. **Mudanças irreversíveis ou pagas na AWS/Entra são do humano:** aplicar IAM, criar App Registration,
   criar secrets com valores, subir/derrubar infraestrutura paga. A IA prepara, explica e valida.
9. **Commit/push:** neste repositório o dono costuma pedir commit+push ao fim de cada entrega; em
   repositórios de outros projetos (ex.: MidTS) **só com pedido explícito**.

## Mapa do código

```
cmd/nautilm/            main: lê config, providers, banco, auth, Entra, Valkey, servidor HTTP
internal/config/        YAML estrito: providers, models (apelidos), pricing, agents, quotas, dashboard, database
internal/live/          config EM VIGOR = arquivo + revisões da aba Config; recarga; assistente de provedor
internal/gateway/       HTTP: chat, SSE, embeddings, fallback, mascaramento, cotas, cobrança, painel, admin, aba Config
internal/gateway/web/   dashboard.html (embutido no binário; JS puro, sem CDN)
internal/auth/          kubernetes (TokenReview), tokens estáticos, tokens do banco (naut_), none (dev)
internal/webauth/       login Entra ID (OIDC + PKCE), sessões, papéis por grupo
internal/store/         Postgres (tokens, sessões, auditoria, revisões) + Memory (testes); migrations/*.sql
internal/secrets/       Secrets Manager (URL do banco e chaves de provedor)
internal/provider/      contrato Provider; bedrock (Converse/ConverseStream); openaicompat
internal/pricing/       catálogo de preços por provedor+modelo com vigência
internal/usage/         livro-caixa diário (memória/Valkey), relatórios JSON/CSV
internal/quota/         cotas por agente (memória/Valkey, fail-open)
internal/mask/          regras de mascaramento (CPF, CNPJ, cartão, e-mail, JWT, chaves…)
internal/metrics/       Prometheus sem dependência
internal/oidcmock/      Entra simulado (testes e laboratório; NUNCA em cluster real)
pkg/openai/             tipos do formato OpenAI (inclui marcas de prompt cache)
deployments/k8s/        EKS: nautilm.yaml, postgres.yaml, agent-example.yaml
deployments/aws/        política IAM e trust do Pod Identity (com placeholders)
deployments/local-kind/ laboratório local: kind + Postgres + Valkey + Entra simulado + Floci
deployments/grafana/    painel de operação
examples/               agentes de teste, teste local com Bedrock real, mock do Entra
docs/                   ROADMAP, ADR, IMPLANTACAO-EMPRESA, MIGRACAO-AGENTES
```

## Comandos

```bash
go test ./...                     # unitários (todos os pacotes)
gofmt -l . && go vet ./...        # lint
make docker                       # imagem distroless nonroot (nautilm:dev)
go run ./cmd/nautilm -check -config <arquivo>   # valida o config (regras da subida) e sai
make test-valkey                  # integração com Valkey (VALKEY_URL=redis://localhost:6379)
NAUTILM_TEST_DATABASE_URL=postgres://postgres:test@localhost:15432/postgres?sslmode=disable go test ./internal/store/

bash deployments/local-kind/up.sh                       # laboratório completo (kind nautilm-lab)
bash deployments/local-kind/use-entra.sh <tenant> <client> <grupo-admin> <grupo-viewer>   # Entra real no lab
kind delete cluster --name nautilm-lab                  # apaga o laboratório
```

Laboratório: `http://localhost:18095/dashboard` (nautilm) e `http://localhost:18096` (Entra simulado:
ana = admin, bruno = viewer, carla = sem grupo, dani = overage).

## Onde aprofundar

| Assunto | Arquivo |
|---|---|
| Arquitetura, fluxo de uma chamada, decisões e trade-offs | `.claude/contexto/arquitetura.md` |
| Toda chave de config, variável de ambiente e onde cada valor mora | `.claude/contexto/configuracao.md` |
| Modelo de segurança (identidades, segredos, IAM, CSRF, mascaramento) | `.claude/contexto/seguranca.md` |
| Convenções de código, como estender, testes e armadilhas conhecidas | `.claude/contexto/convencoes.md` |
| O que está validado, o que não está, pendências | `.claude/contexto/estado-e-roadmap.md` |
| **Implantar na organização (runbook para IA, com pontos de decisão humana)** | `docs/IMPLANTACAO-EMPRESA.md` |
| **Demo num EKS existente (um namespace, sem disco, custo ~zero)** | `deployments/demo-eks/README.md` |
| **Migrar um agente para o nautilm (MidTS, agentkit, genérico)** | `docs/MIGRACAO-AGENTES.md` |
| Manual do usuário (pt-BR): custo, painel, aba Config, prompt cache, permissões | `README.md` |
| Por que construir em vez de usar Bifrost/Agent Router | `docs/adr/0001-construir-vs-comprar.md` |
