# nautilm · Nautilus LLM Gateway

**LLM Gateway** dos agentes: todo tráfego de LLM passa por aqui, só o
nautilm tem credencial nos provedores (Bedrock hoje, API da Anthropic ou
outros amanhã), e ele mantém o **livro-caixa** de quem usou qual modelo,
onde, quantos tokens e quanto custou.

> **Vai implantar ou migrar um agente?** Comece por
> [docs/IMPLANTACAO-EMPRESA.md](docs/IMPLANTACAO-EMPRESA.md) (runbook por fases, com os pontos que
> exigem uma pessoa) e [docs/MIGRACAO-AGENTES.md](docs/MIGRACAO-AGENTES.md). **IAs** (Claude Code,
> Kiro, Copilot…): o contexto completo do projeto está em [.claude/CLAUDE.md](.claude/CLAUDE.md)
> (e [AGENTS.md](AGENTS.md)).

```
 k8s ─────┐                          ┌─► Bedrock (Claude, Nova…)     ← só o nautilm tem IAM
 mid ─────┤                          ├─► API da Anthropic / outros
 marcia ──┼─► nautilm (OpenAI API) ──┤
 claudia ─┤   apelidos · auth ·      └─► Ollama / vLLM / OpenAI-compatible
 maestro ─┘   fallback · mascaramento · cotas · custo · livro-caixa
                       │
                       └─► /dashboard: "última semana: k8s 1M tokens = US$ X"
```

- **Agentes pedem apelidos** (`smart`, `fast`), nunca o ID do modelo. Trocar
  de modelo ou de provedor = mudar uma linha do config.
- **Identidade por agente**: no EKS, o token da própria ServiceAccount
  (TokenReview); fora do cluster, **um token por agente**.
- **Custo por provedor + modelo**, com vigência: o preço é de quem atendeu
  de verdade (inclusive no fallback), e mudar o preço não reescreve o histórico.
- **Livro-caixa** com totais diários por agente × time × centro de custo ×
  provedor × modelo, em Valkey, e um **painel** com CSV para o financeiro.
- **Proteções ligadas por padrão**: mascaramento de dados sensíveis e cotas de tokens.
- **Formato OpenAI**: qualquer SDK/framework funciona; o agente só troca a base URL.

Código, configuração, logs, erros da API e o painel estão em **inglês**; a
documentação do time continua em português.

Antigo **pedagio** (o repositório foi renomeado; o GitHub redireciona o nome
antigo). Por que construir em vez de usar Bifrost ou Agent Router: veja
[docs/adr/0001-construir-vs-comprar.md](docs/adr/0001-construir-vs-comprar.md).
O que falta: [docs/ROADMAP.md](docs/ROADMAP.md).

## Rodar local (com Ollama)

```bash
cp config.example.yaml config.yaml   # ajuste providers/tokens; para dev: auth.modes: [none]
make run
```

```bash
curl -s localhost:8080/v1/chat/completions -H 'X-Nautilm-Agent: me' \
  -d '{"model":"local","messages":[{"role":"user","content":"say hi"}]}'
```

A resposta vem com `"model": "local"` (o apelido). Os headers dizem quem
atendeu e quanto custou: `X-Nautilm-Provider`, `X-Nautilm-Model`,
`X-Nautilm-Cost-USD`, `X-Nautilm-Price-Source` e `X-Nautilm-Billed-To`.

A plataforma inteira (agentes, maestro, Mirante, Grafana) sobe pelo compose do
[plataforma-agentes](https://github.com/chwiee/plataforma-agentes).

## Custo e divisão da conta

### Catálogo de preços: por provedor + modelo, com vigência

O preço é de **onde** a chamada rodou e de **qual modelo** atendeu, não do
apelido: `smart` pode ser Claude no Bedrock hoje e Claude na API da Anthropic
amanhã, e um fallback pode ser atendido por outro modelo em outro lugar.

```yaml
providers:
  bedrock-us-east-2: {type: bedrock, region: us-east-2}   # o NOME do provider é o "onde"
pricing:
  - provider: bedrock-us-east-2
    model: us.anthropic.claude-sonnet-5-5   # perfil de inferência "us." (o Claude 5.5 não tem versão direta em us-east-2)
    input: 2.20          # USD por 1M tokens (perfil "us.", tabela AWS de 23/10/2026)
    output: 11.00
    cache_read: 0.11     # sem preço de cache = cobra como input (conservador)
    cache_write: 2.75
    effective_from: 2026-10-01
  - provider: ollama-local
    model: "*"           # todos os modelos do provider
    free: true           # infra própria: zero de propósito
strict_pricing: true     # modelo sem preço = erro na subida (recomendado em produção)
```

- **Preço mudou?** Acrescente uma linha com novo `effective_from`. Cada chamada
  usa o preço vigente **no dia em que rodou**; o histórico não muda.
- **Sem preço nunca vira zero calado**: sem `strict_pricing`, a subida avisa e
  as chamadas ficam como **unpriced** (custo desconhecido), com alerta no painel.
  Preço zero exige `free: true`, então esquecer o preço não passa como "grátis".
- **Custo informado pelo provedor vence** a tabela (ex: OpenRouter manda
  `usage.cost`). Origem do preço vai em cada registro: `table`, `provider`,
  `free` ou `unpriced`.
- **Tokens normalizados** para todos os provedores: entrada inclui leitura e
  escrita de cache (semântica da OpenAI). No Bedrock, que informa separado, o
  nautilm soma; o preço de cache é aplicado à parte.
- Só USD (é como os provedores cobram). Converter para BRL é coisa de tela.

### Quem paga

| Situação | Quem paga |
|---|---|
| Agente chama direto | o próprio agente (a identidade autenticada) |
| maestro (com `on_behalf_of: true`) manda `X-Nautilm-On-Behalf-Of: k8s` | **k8s**; o registro guarda `caller: maestro` |
| Agente **sem** `on_behalf_of` manda o header | header **ignorado** (log + métrica): ninguém joga a conta no vizinho |

Cada agente tem `team` e `cost_center` no config; eles entram em cada registro
(se mudarem depois, o histórico não muda). A cota é sempre do **chamador**:
ela protege contra o loop de quem chama.

```yaml
agents:
  k8s:     {models: [smart, fast, embed], team: ce, cost_center: CC-1001}
  maestro: {models: [fast, embed], team: platform, on_behalf_of: true}
```

### Livro-caixa (`usage`)

Cada tentativa de chamada (inclusive as que falharam no fallback, que contam
como erro) soma num total **diário** por dia × agente × chamador × time ×
centro de custo × provedor × modelo.

- **Valkey** (`valkey_url`): um hash por dia com TTL = `usage.retention_days`
  (padrão 400 dias, para comparar o mesmo mês do ano anterior). Sem job de
  limpeza; réplicas somam no mesmo total (`HINCRBY`, atômico).
- **Por que não o Prometheus?** Ele é para operação: ~15 dias, aproximações
  quando perde uma coleta. Dividir conta pede meses de números que fecham.
- **Valkey fora**: a chamada **continua** (200), o registro vai inteiro para o
  log como `usage record lost` (dá para recuperar) e conta em
  `nautilm_usage_lost_total`. O relatório responde 503 em vez de inventar número.
- **Trade-off**: Valkey é rápido e já está na stack, mas não é um banco
  "para sempre". Se o financeiro exigir mais garantia, a interface `usage.Store`
  permite trocar por DynamoDB sem mexer no resto.

### Painel e API

`GET /dashboard` (página embutida no binário, mesma paleta do Mirante):

- período (últimos 7 ou 30 dias, este mês, mês passado ou datas livres);
- agrupar por agente, time, centro de custo, provedor, provedor × modelo,
  agente × provedor × modelo ou agente × chamador;
- custo total e variação contra o período anterior de mesmo tamanho, tokens,
  chamadas e erros, **chamadas sem preço** com alerta;
- custo por grupo, custo por dia, tabela com % do total e **Export CSV**.

Os dados vêm de `GET /v1/usage?from=2026-10-19&to=2026-10-25&group_by=agent,provider`
(JSON) e `GET /v1/usage.csv` (mesmos parâmetros). Quem lê: pessoa logada pelo
**Entra ID** (grupo viewer ou admin, ver abaixo) ou automação com
`dashboard.token` (Grafana, scripts; só leitura). **Sem Entra e sem token, a
API fica desligada** (404): custo é dado interno. O token do painel não é o de
nenhum agente.

> É **estimativa** (tokens informados pelo provedor × tabela). Concilie uma vez
> por mês com a fatura de cada provedor (Cost Explorer no Bedrock, console de
> uso da Anthropic). Diferença grande = tabela desatualizada.

## Login com Entra ID e tokens de aplicação

O painel tem login **Microsoft Entra ID** e dois papéis, pelos **grupos** do usuário:

| Papel | Grupo (`dashboard.entra.*`) | Pode |
|---|---|---|
| **viewer** | `viewer_groups` | ver uso e custo (aba Usage) |
| **admin** | `admin_groups` (time de gestão do nautilm) | tudo do viewer + **criar, rotacionar e revogar tokens** de aplicação + ver a auditoria |

Fora dos dois grupos = "Access denied" (e a tentativa fica na auditoria).

### Tokens criados no painel

Aba **Tokens** (só admin) → *New application token*: nome do agente, time,
centro de custo, validade (padrão 90 dias), cotas próprias e **quais apelidos**
ele pode usar. O token (`naut_...`) aparece **uma única vez**; no banco fica só
o hash SHA-256.

- **Rotacionar** cria um token novo com o mesmo perfil e **não derruba o
  antigo**: troque no agente e depois revogue o antigo (sem downtime).
- **Revogar** vale na hora na réplica que recebeu e em até **15 s** nas outras (cache).
- Token do painel **nunca** pode agir em nome de outro (`on_behalf_of`), e não
  pode ter o nome de um agente do config (409): quem é orquestrador fica no config.
- Agentes **dentro do EKS** continuam usando o token da ServiceAccount; o
  token do painel é para apps fora do cluster.
- Banco fora do ar: chamadas com `naut_...` recebem **503** (não 401), para o
  agente não achar que o token morreu.
- Toda ação (login, login negado, criar, rotacionar, revogar, logout) vai para
  a tabela `audit_log` com quem fez e quando: aba **Audit**.

### Como funciona (segurança)

- Authorization Code + **PKCE** no servidor; `state` e `nonce` em cookie
  temporário. O ID token é validado (assinatura, emissor, audiência, nonce).
- Sessão: cookie `nautilm_session` **HttpOnly**, `SameSite=Lax`, `Secure`
  quando `public_url` é https; no banco só o hash. Duração em `session_ttl` (8h).
- POST de admin exige `Origin` igual ao `public_url` (proteção CSRF).
- Os grupos vêm do claim `groups` do token. Usuário com **mais de 200 grupos**
  recebe "overage" no lugar da lista e é negado com a dica: configure a App
  Registration para mandar **só os grupos atribuídos à aplicação**.

### App Registration no Entra (uma vez, por um admin do tenant)

1. **Entra ID → App registrations → New registration**: nome `nautilm`,
   *Single tenant*, Redirect URI tipo **Web**: `https://<public_url>/auth/callback`.
2. **Certificates & secrets → New client secret**: vai para
   `NAUTILM_ENTRA_CLIENT_SECRET` (anote a validade: expirou = ninguém loga).
3. **Token configuration → Add groups claim → Groups assigned to the
   application**, emitido como *Group ID* no ID token.
4. **Enterprise applications → nautilm → Users and groups**: adicione o grupo
   admin e o grupo viewer. Com *Assignment required = Yes*, quem não está em
   nenhum dos dois nem chega ao nautilm.
5. No config: `tenant_id`, `client_id` (Application ID) e os **Object IDs** dos grupos.

```yaml
dashboard:
  public_url: https://nautilm.example.com
  session_ttl: 8h
  entra:
    tenant_id: <tenant-id>
    client_id: <application-client-id>
    client_secret: ${NAUTILM_ENTRA_CLIENT_SECRET}
    admin_groups: [<object-id>]
    viewer_groups: [<object-id>]
database:
  aws_secret_arn: arn:aws:secretsmanager:us-east-2:<ACCOUNT_ID>:secret:nautilm/database-XXXXXX
```

### Banco: PostgreSQL num pod com PVC

Tokens, sessões e auditoria ficam no **PostgreSQL** (o livro-caixa continua no
Valkey). As tabelas são criadas sozinhas na subida (migrações com *advisory
lock*: várias réplicas sobem juntas sem conflito). Se o banco ainda não
respondeu, o nautilm tenta de novo (2s → 30s, até 5 min).

- **EKS**: [deployments/k8s/postgres.yaml](deployments/k8s/postgres.yaml):
  StatefulSet + volume EBS gp3, `pg_dump` diário (7 cópias) e NetworkPolicy
  liberando só o nautilm.
- **URL de conexão**: na AWS, num segredo do **Secrets Manager**
  (`database.aws_secret_arn`; texto `postgres://...` ou JSON com a chave `url`,
  outra chave via `aws_secret_key`). Local, numa **Secret do Kubernetes**
  (`database.url: ${NAUTILM_DATABASE_URL}`). Um ou outro, nunca os dois.
- **Trade-off** do banco em pod: barato e simples, mas **o backup é seu**. O
  `pg_dump` fica num PVC da mesma zona; para desastre, tire também snapshot do
  EBS (AWS Backup). Se o nautilm virar peça crítica de muitos times, migrar
  para **RDS/Aurora PostgreSQL** é só trocar a URL no segredo.

### Testar tudo local (kind + Postgres + Entra simulado)

```bash
bash deployments/local-kind/up.sh
```

Sobe um cluster kind `nautilm-lab` com Postgres (PVC), Valkey, um **Entra
simulado** e 2 réplicas do nautilm usando o Ollama da máquina. As senhas são
geradas na hora e ficam só em Secrets do cluster. Abra
`http://localhost:18095/dashboard` → *Sign in with Microsoft* e escolha:
**ana** (admin), **bruno** (viewer), **carla** (sem grupo, negada) ou
**dani** (overage, negada). Apagar: `kind delete cluster --name nautilm-lab`.

> O Entra simulado ([examples/mock-entra](examples/mock-entra)) deixa qualquer
> um entrar como qualquer usuário: **só laboratório**, nunca em cluster real.

O laboratório também sobe o **Floci** (AWS emulada) no lugar do Secrets
Manager, para testar o assistente de provedor sem conta AWS. Para criar uma
secret nele:

```bash
kubectl --context kind-nautilm-lab -n nautilm port-forward svc/floci 14566:4566
AWS_ACCESS_KEY_ID=test AWS_SECRET_ACCESS_KEY=test aws --endpoint-url http://localhost:14566 --region us-east-2 \
  secretsmanager create-secret --name nautilm/providers/ollama-lab \
  --secret-string '{"api_key":"x","base_url":"http://host.docker.internal:11434/v1"}'
```

O Floci **não aplica IAM**: o caso "role sem permissão" é coberto pelos testes
unitários, não pelo laboratório.

## Aba Config (admin)

Admins mudam a config **sem reiniciar** e sem mexer no arquivo. O arquivo
continua sendo a base: o que vem dele aparece como *config file* e é somente
leitura na tela; o painel **adiciona ou sobrepõe**.

| Seção | O que dá para fazer | Proteção |
|---|---|---|
| **Providers** | adicionar provedor OpenAI-compatível (chave no Secrets Manager) ou Bedrock (outra região), editar, apagar | assistente com teste; não apaga provedor em uso |
| **Aliases** | criar apelido, trocar o modelo/provedor de um apelido, fallback, max tokens; "Restore file" desfaz a mudança num apelido do arquivo | **teste com chamada real** (≤ 8 tokens) antes de salvar |
| **Prices** | preço por provedor + modelo, com vigência | com `strict_pricing`, apelido sem preço não salva |
| **Entra groups** | mapear mais grupos para admin/viewer | grupos do arquivo nunca saem (ninguém fica trancado para fora) |
| **History** | ver cada revisão (quem, quando, o quê) e **restaurar** | restaurar cria uma revisão nova; o histórico nunca é reescrito |

### Assistente de novo provedor

1. **Provider**: nome e tipo. OpenAI-compatível (OpenRouter, vLLM, LiteLLM,
   gateway da empresa…) ou Amazon Bedrock (sem secret: usa o role do nautilm).
2. **Secret**: mostra o JSON e o comando da AWS CLI (lê a chave sem eco).
   Formato: `{"api_key": "...", "base_url": "https://.../v1"}` (`base_url`
   opcional se for informado na tela) ou só a chave em texto.
3. **ARN**: o ARN completo. O nautilm confere o formato, a conta, a região e
   se o nome está sob `nautilm/providers/`.
4. **Permission**: dentro do prefixo, o statement único já cobre (aplique uma
   vez). Fora dele, mostra o statement exato daquela secret (e sugere mover
   para o prefixo). No Bedrock, mostra o `bedrock:InvokeModel` do modelo.
5. **Test**: lê a secret com o role do nautilm, confere o formato e chama o
   provedor (`GET /models`, ou 1 token no Bedrock). Passou tudo = **salva
   sozinho**. Falhou = diz o passo, o motivo e o que fazer:

| Erro | O que a tela diz |
|---|---|
| `AccessDeniedException` | o role não lê a secret + o statement pronto para copiar |
| KMS negado | a secret usa chave KMS própria: falta `kms:Decrypt` |
| `ResourceNotFoundException` | conta, região ou sufixo do ARN errados |
| JSON sem `api_key` | os **nomes** das chaves encontradas (nunca os valores) |
| 401/403 do provedor | a secret foi lida, mas a chave foi recusada |
| conexão recusada | `base_url` errada ou sem rota de rede |

O provedor salvo **não recebe tráfego** até um apelido usá-lo. No banco fica só
o ARN; a chave fica em memória e é relida a cada 10 min (acompanha a rotação
da secret). Se a leitura falhar depois, só aquele provedor responde 503
`provider_unavailable` (com fallback, se houver) e a tela mostra ⚠.

### Como as mudanças viram realidade

- Cada salvamento valida a config **inteira** (arquivo + painel) com as
  mesmas regras da subida. Config inválida não é salva.
- Cada requisição usa um retrato da config do começo ao fim: uma mudança no
  meio não mistura duas configs.
- A réplica que salvou aplica na hora; as outras em até 5 s.
- Dois admins editando ao mesmo tempo: o segundo recebe 409 ("recarregue"),
  em vez de desfazer a mudança do outro sem saber.
- As chamadas de teste vão para o livro-caixa como `nautilm-config-test`
  (custo visível).
- Tudo vai para a auditoria (`config.*`), inclusive testes que falharam.
- Se a subida encontrar uma mudança do painel que ficou inválida (ex.: o
  arquivo removeu um provedor que um apelido do painel usava), o nautilm sobe
  **só com o arquivo** e a aba Config mostra o erro em destaque.

**Trade-off**: mudança pelo painel não passa por PR (GitOps). O histórico, a
restauração e a auditoria compensam; para mudanças estruturais (provedores
com credencial no env, agentes, cotas), o arquivo continua sendo o caminho.

## Proteções (ligadas por padrão)

### Mascaramento de dados sensíveis

Antes de qualquer modelo, o nautilm troca dados sensíveis por marcadores:
`[CPF]`, `[CNPJ]`, `[CREDIT_CARD]` (só número que passa no Luhn), `[EMAIL]`,
`[JWT]`, `[AWS_KEY]`, `Bearer [TOKEN]`, `[PRIVATE_KEY]`, e o valor de `senha=`,
`DB_PASSWORD=`, `"token":"..."` etc. vira `[SECRET]` (a chave fica, para o
modelo entender o contexto).

- Vale para **todas** as mensagens, inclusive resultado e argumentos de tool, e
  para embeddings. Definição de tools e campos de estrutura não são tocados.
- O agente sabe pelo header `X-Nautilm-Masked: cpf=1,secret=2`; log e métrica
  `nautilm_masked_total{agent,rule}` guardam só o **nome** da regra.
- Ajuste: `masking: {disable: [email], extra: [{name: employee_id, regex: 'MAT-\d{6}'}]}`.
- **Trade-off**: é regex. Pega formatos conhecidos com poucos falsos
  positivos, mas não reconhece segredo sem formato ("a senha é batata").

### Cotas por agente

Tokens de entrada + saída por agente, em janelas fixas: **200 mil por minuto** e
**10 milhões por dia** (padrão). Estourou → `429` + `Retry-After`, sem fallback.

- Cota própria em `agents.<id>.tokens_per_minute/tokens_per_day` (`-1` = sem
  limite); padrão de todos em `quotas:`.
- Contador no Valkey (réplicas somam juntas); **fail-open**: Valkey fora = deixa passar e avisa.
- O custo só é conhecido depois da resposta: a última chamada pode passar um pouco do limite.

## Endpoints

| Rota | O que faz |
|---|---|
| `POST /v1/chat/completions` | chat (com ou sem `stream`; tool calling) |
| `POST /v1/embeddings` | embeddings (Titan v2 no Bedrock, `/embeddings` nos compatíveis); **sem fallback** |
| `GET /v1/models` | apelidos que **este** agente pode usar |
| `GET /v1/usage`, `/v1/usage.csv` | livro-caixa (sessão viewer/admin ou token do painel) |
| `GET /dashboard` | painel de uso, custo, tokens e auditoria |
| `GET /auth/login`, `/auth/callback`, `POST /auth/logout`, `GET /auth/me` | login Entra ID e sessão |
| `GET/POST /v1/admin/tokens`, `POST /v1/admin/tokens/{id}/rotate`, `/{id}/revoke` | tokens de aplicação (**admin**) |
| `GET /v1/admin/audit`, `/v1/admin/aliases` | auditoria e apelidos (**admin**) |
| `GET /v1/admin/config`, `POST /v1/admin/config/{arn,providers,models,pricing,groups,rollback}`, `GET /v1/admin/config/history` | aba Config (**admin**) |
| `GET /metrics` | Prometheus (operação): requests, tokens, custo, duração, TTFT, fallbacks, bloqueios, mascaramentos, registros perdidos |
| `GET /healthz` | health |

Erros no formato da OpenAI: 401 sem credencial, 403 apelido não permitido,
404 apelido inexistente, 429 limite atingido, 429/5xx do provedor depois do fallback.

Os 429 têm `type: rate_limit_error` e o `code` diz **de quem** é o limite:

| `code` | Limite de quem | Volta quando |
|---|---|---|
| `quota_minute` / `quota_day` | **nautilm** (cota do agente no config) | virada do minuto / meia-noite UTC (`Retry-After`) |
| `provider_quota_day` | **conta no provedor** (ex: quota diária do modelo na conta AWS) | no dia seguinte ou quando a AWS aumentar a quota |
| `ThrottlingException` | provedor, por minuto | em segundos |

No `provider_quota_day` o nautilm **não insiste**: o SDK da AWS não repete a
chamada (a quota diária não volta em segundos) e a mensagem diz o modelo e
que o limite é da conta, não do nautilm. Com `fallback` no apelido, o próximo
modelo é tentado normalmente; sem fallback, o agente recebe o erro.

O config é lido em **modo estrito**: chave desconhecida (erro de digitação ou
chave antiga do pedagio, como `cotas:`) impede a subida com a mensagem dizendo
qual chave usar.

## Prompt cache (Bedrock)

Agentes que repetem um prefixo grande em toda chamada (instruções, guias,
definição de tools: o **CAG** do MidTS) economizam com o prompt cache do
Bedrock: a parte em cache custa ~10% na leitura e responde mais rápido.

O agente marca **onde termina o trecho estável** com a convenção da
Anthropic/OpenRouter/LiteLLM, no formato OpenAI:

```json
{"role": "system", "content": [
  {"type": "text", "text": "<guias longos>", "cache_control": {"type": "ephemeral"}}
]}
```

(também vale `"cache_control"` na mensagem inteira). O apelido decide o que
fazer com a marca:

| `prompt_cache` | O que acontece no Bedrock |
|---|---|
| `off` (padrão) | marcas ignoradas: nada muda |
| `explicit` | cada marca vira um bloco `cachePoint` no mesmo lugar (system, user, assistant ou depois de um tool result) |
| `auto` | como `explicit`; se o agente não mandou marca, põe um `cachePoint` no fim do system (cobre tools + system) |

```yaml
models:
  midts-smart: {provider: bedrock-us-east-2, model: us.anthropic.claude-sonnet-5-5, prompt_cache: explicit}
```

- **Ligue só em modelos com prompt cache** (Claude, Nova). Os demais recusam
  o `cachePoint`. Cuidado com `fallback` para um modelo sem suporte.
- No máximo **4** pontos por requisição (limite do Bedrock): marcas a mais são
  descartadas, mantendo as **primeiras** (o prefixo mais estável).
- Prefixo menor que o mínimo do modelo (ex.: ~1.024 tokens no Claude) não é
  cacheado, mas a chamada funciona.
- Custo: leitura e escrita do cache vêm separadas do Bedrock e são cobradas
  com `cache_read`/`cache_write` do `pricing` (sem eles, cobradas como input:
  conservador). A resposta traz `usage.prompt_tokens_details.cached_tokens`.
- A marca passa pelo **mascaramento** junto com o texto (a parte marcada
  também é mascarada).
- Provedores OpenAI-compatíveis recebem o JSON do agente como veio, com as
  marcas (OpenRouter e LiteLLM as usam).
- Também dá para escolher na aba Config (campo *Prompt cache* do apelido).

## Streaming

Com `"stream": true` o nautilm repassa os tokens **conforme o modelo gera**
(SSE no formato OpenAI): `ConverseStream` no Bedrock, repasse do SSE nos
compatíveis com OpenAI.

- **Fallback só antes do 1º byte.** Depois do primeiro chunk o 200 já saiu: um
  erro vira um evento `data: {"error": {...}}` e o stream termina sem `[DONE]`.
- Tokens de um stream que quebrou no meio **são cobrados** (o provedor cobra).
- `nautilm_time_to_first_token_seconds`: a latência que o usuário sente.
- Atrás de nginx/ingress, o nautilm manda `X-Accel-Buffering: no`.

## Como um agente usa

Qualquer cliente OpenAI, apontando a base URL para o nautilm e mandando o
token. No EKS: [deployments/k8s/agent-example.yaml](deployments/k8s/agent-example.yaml)
(token projetado com `audience: nautilm`); o agente **não precisa de role IAM**.
O agentkit ainda lê `PEDAGIO_URL`/`PEDAGIO_TOKEN`: o valor é o do nautilm.

## Permissões necessárias

Resumo: **só o nautilm tem permissão na AWS**, e só para invocar os modelos que
serve. Os agentes não têm role IAM nenhuma; eles só precisam de um token
(ServiceAccount no EKS ou token do nautilm).

### 1. AWS: role do nautilm (EKS Pod Identity)

Arquivos prontos, validados no **IAM Access Analyzer** sem nenhum achado
(erro, aviso de segurança ou sugestão):

- [deployments/aws/nautilm-policy.json](deployments/aws/nautilm-policy.json): permissões
- [deployments/aws/nautilm-pod-identity-trust.json](deployments/aws/nautilm-pod-identity-trust.json): confiança

Troque `<ACCOUNT_ID>` e `<CLUSTER_NAME>` antes de usar.

**Política de permissão** (as ações vêm da *Service Authorization Reference* da
AWS para as chamadas que o código faz: `Converse`, `ConverseStream` e `InvokeModel`):

| Statement | Ações | Recurso | Por quê |
|---|---|---|---|
| `InvokeRegionalModels` | `bedrock:InvokeModel`, `bedrock:InvokeModelWithResponseStream` | ARN `foundation-model` de **cada modelo direto** em us-east-2 (Nova Lite, Titan) | chat, streaming e embeddings |
| `InvokeClaudeThroughTheUsInferenceProfile` | as mesmas | ARN do **perfil** `us.anthropic.claude-sonnet-5-5` | o Claude 5.5 só existe via perfil cross-Region |
| `ClaudeModelsOnlyViaThatProfile` | as mesmas | ARN do modelo nas 3 regiões do perfil, com condição `bedrock:InferenceProfileArn` | o perfil roteia entre us-east-1/2 e us-west-2; a condição impede chamar o modelo por fora do perfil |
| `ConverseAuthorized*` | `bedrock:ApplyGuardrail`, `bedrock:InvokeTool`, `bedrock:CallWithBearerToken` | guardrails e system tools da conta | a AWS lista essas ações como parte da autorização do `Converse`; hoje o nautilm não usa guardrails, tools do Bedrock nem API keys do Bedrock |
| `ReadTheDatabaseConnectionSecret` | `secretsmanager:GetSecretValue` | `secret:nautilm/database-??????` (só esse segredo) | URL do Postgres na subida. Se o segredo usar **chave KMS própria** (CMK), acrescente `kms:Decrypt` nela; com a chave padrão `aws/secretsmanager`, não precisa |
| `ReadNautilmProviderSecrets` | `secretsmanager:GetSecretValue` | `secret:nautilm/providers/*` | chaves dos provedores adicionados na **aba Config**. Um statement para todos: provedor novo não exige mudar IAM. O prefixo deve ser exclusivo do nautilm |

Não há `bedrock:*` nem `Resource: "*"` nas ações de invocação: **modelo novo =
acrescentar o ARN dele** (e o preço em `pricing`, e o apelido em `models`).

**Política de confiança**: `pods.eks.amazonaws.com` com `sts:AssumeRole` e
`sts:TagSession` (o que o EKS Pod Identity exige), restrita por `aws:SourceAccount`
e `aws:SourceArn` ao **seu cluster**: o cluster de outra conta não consegue
assumir o role.

```bash
aws iam create-role --role-name nautilm \
  --assume-role-policy-document file://deployments/aws/nautilm-pod-identity-trust.json
aws iam put-role-policy --role-name nautilm --policy-name nautilm \
  --policy-document file://deployments/aws/nautilm-policy.json
aws eks create-pod-identity-association --cluster-name <CLUSTER_NAME> --region us-east-2 \
  --namespace llm-gateway --service-account nautilm --role-arn arn:aws:iam::<ACCOUNT_ID>:role/nautilm
```

Antes do primeiro teste na conta, confira também (foi o que travou no lab):

- **Modelos liberados**: modelos de terceiros (Anthropic etc.) vêm do AWS
  Marketplace e, na primeira vez, precisam ser habilitados na conta por um
  administrador (catálogo do Bedrock; a Anthropic pede um formulário de uso).
  O role do nautilm **não** precisa de permissão do Marketplace.
- **Quotas do Bedrock acima de zero** para cada modelo, na região:
  `aws service-quotas list-service-quotas --service-code bedrock --region us-east-2`.
  Conta nova pode vir com quota **0** (erro `provider_quota_day`); aumento pelo
  Support Center quando a quota não é ajustável.
- **Cross-Region permitido?** Conta de região única (como o lab) não usa perfis
  `us.`/`global.` e, portanto, **não tem Claude 5.5**. Nesse caso tire os dois
  statements do Claude e use só modelos com `ON_DEMAND` na região
  (`aws bedrock list-foundation-models --by-inference-type ON_DEMAND`).

### 2. Kubernetes

| Quem | Permissão | Onde |
|---|---|---|
| ServiceAccount `nautilm` | ClusterRole `system:auth-delegator` (criar `TokenReview`) | já no [manifest](deployments/k8s/nautilm.yaml) |
| Agentes | **nenhuma** RBAC; só o token projetado da própria ServiceAccount com `audience: nautilm` | [agent-example.yaml](deployments/k8s/agent-example.yaml) |
| Cluster | add-on **EKS Pod Identity Agent** instalado | `aws eks create-addon --addon-name eks-pod-identity-agent` |
| Cluster | add-on **Amazon EBS CSI driver** + StorageClass `gp3` (PVC do Postgres) | `aws eks create-addon --addon-name aws-ebs-csi-driver` (o driver precisa do próprio role IAM) |

### 3. Rede

| De → para | Porta | Observação |
|---|---|---|
| nautilm → Bedrock Runtime (`bedrock-runtime.us-east-2.amazonaws.com`) | 443 | sem NAT, use o VPC endpoint `com.amazonaws.us-east-2.bedrock-runtime`; mesmo com perfil cross-Region a chamada sai por us-east-2 |
| nautilm → API do Kubernetes | 443 | TokenReview |
| nautilm → ElastiCache (Valkey) | 6379 (TLS, `rediss://`) | security group do ElastiCache liberando só o nautilm; não usa IAM |
| agentes → nautilm | 80 (Service) / 8080 | NetworkPolicy liberando só os namespaces dos agentes |
| nautilm → Postgres | 5432 | NetworkPolicy do [postgres.yaml](deployments/k8s/postgres.yaml) libera só nautilm e o backup |
| nautilm → Secrets Manager (`secretsmanager.us-east-2.amazonaws.com`) | 443 | só na subida; sem NAT, VPC endpoint `com.amazonaws.us-east-2.secretsmanager` |
| nautilm → Entra ID (`login.microsoftonline.com`) | 443 | discovery, chaves e troca do código no login |
| navegador → nautilm | 443 (Ingress/ALB com HTTPS) | o cookie de sessão é `Secure`; sem HTTPS o login não funciona |
| Prometheus → nautilm `/metrics` | 8080 | |

### 4. Segredos (Kubernetes Secret ou Secrets Manager)

| Variável | Para quê |
|---|---|
| `VALKEY_URL` | endpoint do ElastiCache com TLS e AUTH |
| `NAUTILM_DASHBOARD_TOKEN` | leitura de `/v1/usage` por automação (Grafana, scripts) |
| `NAUTILM_ENTRA_CLIENT_SECRET` | client secret da App Registration (login do painel) |
| Secrets Manager `nautilm/database` | URL do Postgres (`postgres://nautilm:<senha>@postgres.llm-gateway:5432/nautilm`) |
| Secret `postgres-auth` | senha do Postgres para o pod e o backup (**a mesma** da URL acima) |
| `NAUTILM_TOKEN_<AGENTE>` | só se usar `auth.modes: [tokens]` com tokens fixos no config (o painel substitui isso) |

### 5. Dentro do nautilm (config)

- `agents.<id>.models`: quais apelidos cada agente pode usar (o resto dá 403).
- `agents.<id>.on_behalf_of: true`: **só** o orquestrador; quem tem isso pode
  jogar a conta de uma chamada para outro agente.
- Token do painel: quem tem vê o custo de todos os agentes; dê só a quem cuida
  do orçamento.
- Grupos do Entra: `admin_groups` só para o time de gestão do nautilm (cria
  credencial que gasta dinheiro); `viewer_groups` para quem acompanha custo.

## Deploy no EKS

[deployments/k8s/nautilm.yaml](deployments/k8s/nautilm.yaml): Namespace,
ServiceAccount (Pod Identity → role da [seção de permissões](#permissões-necessárias)), ClusterRoleBinding
`system:auth-delegator`, ConfigMap (com `strict_pricing: true`, Entra e
`database.aws_secret_arn`), Secret (`VALKEY_URL`, `NAUTILM_DASHBOARD_TOKEN`,
`NAUTILM_ENTRA_CLIENT_SECRET`), Deployment (2 réplicas, nonroot, read-only), PDB
e Service. O banco: [deployments/k8s/postgres.yaml](deployments/k8s/postgres.yaml)
(aplique antes). O painel do Grafana de operação está em
[deployments/grafana/nautilm.json](deployments/grafana/nautilm.json).

Ordem: role IAM + Pod Identity → segredo `nautilm/database` no Secrets Manager
e Secret `postgres-auth` (mesma senha) → `postgres.yaml` → Secret
`nautilm-secrets` → `nautilm.yaml` → Ingress HTTPS com o `public_url`.

## Estrutura

```
cmd/nautilm/              main: config, providers, auth, Valkey
internal/config/          YAML estrito, apelidos, permissões, cobrança, cotas
internal/auth/            kubernetes (TokenReview), tokens, tokens do banco, none
internal/gateway/         HTTP, fallback, SSE, mascaramento, cotas, cobrança, painel, admin
internal/webauth/         login Entra ID (OIDC + PKCE), sessão, papéis
internal/store/           Postgres (tokens, sessões, auditoria) e versão em memória
internal/secrets/         URL do banco no Secrets Manager
internal/oidcmock/        Entra simulado (testes e laboratório)
internal/live/            config em vigor: arquivo + aba Config, recarga, assistente de provedor
internal/pricing/         catálogo de preços por provedor+modelo com vigência
internal/usage/           livro-caixa (memória/Valkey) e relatórios (JSON/CSV)
internal/quota/           cotas (memória/Valkey)
internal/mask/            regras de mascaramento
internal/provider/        contrato Provider + bedrock + openaicompat
internal/metrics/         contadores Prometheus sem dependência
internal/valkey/          conexão compartilhada
pkg/openai/               tipos do formato OpenAI (agentes Go podem importar)
deployments/              k8s (EKS), local-kind, aws (IAM) e Grafana
examples/                 agentes de teste, local-aws, mock-entra
docs/                     ADR e roadmap
```

## Desenvolvimento

```bash
make test         # go test ./...
make test-valkey  # integração com Valkey (cotas e livro-caixa); VALKEY_URL=redis://localhost:6379
NAUTILM_TEST_DATABASE_URL=postgres://... go test ./internal/store/   # mesmo teste contra Postgres real
make lint         # go vet ./...
make docker       # imagem distroless nonroot
go run ./cmd/nautilm -check -config config.yaml   # valida o config e sai
```
