# Implantar o nautilm na organização (runbook para IA e para pessoas)

Este runbook leva o nautilm do repositório até produção num **EKS da empresa**. Foi escrito para uma
IA conseguir conduzir a implantação junto com o time, sem adivinhar.

## Como usar (leia antes, IA)

- Siga as fases **em ordem**. Cada fase tem **Entradas**, **Passos**, **✅ Verificação** e **↩️ Desfazer**.
- **🧑 HUMANO** marca o que só uma pessoa decide ou executa: aplicar IAM, criar App Registration,
  digitar segredos, criar infraestrutura paga. A IA prepara o comando ou o arquivo, explica o efeito
  e **espera a confirmação**.
- **Nunca peça segredos no chat** (client secret, senhas, chaves). Dê o comando que lê o valor sem eco
  (`read -rs`) e grava direto no destino.
- **Nunca grave IDs reais no Git** (conta, tenant, grupos, ARNs). Preencha placeholders em arquivos
  fora do repositório (ex.: `deploy-<ambiente>/`) ou direto no cluster.
- Ao terminar cada fase, diga ao humano o que mudou e o que vem a seguir. Se uma verificação falhar,
  consulte a seção **Problemas comuns** antes de improvisar.
- Para entender o produto: `.claude/CLAUDE.md` e `.claude/contexto/*`. Manual do usuário: `README.md`.

## Arquitetura alvo

```
Namespace llm-gateway (EKS)
  nautilm (Deployment, 2 réplicas, SA nautilm → role IAM via Pod Identity)
  postgres (StatefulSet + EBS gp3) + CronJob de backup
  Service nautilm:80  ← agentes no cluster (token da ServiceAccount)
  Ingress HTTPS (ALB interno) ← pessoas no painel (Entra ID) e apps fora do cluster (token naut_)
AWS: Bedrock (modelos aprovados) · ElastiCache Valkey (TLS) · Secrets Manager · ECR
Microsoft Entra ID: App Registration + grupos admin/viewer
```

---

## Fase 0 — Levantar informações

**Pergunte (🧑 HUMANO) e anote num arquivo fora do Git:**

| # | Pergunta | Por quê |
|---|---|---|
| 1 | Conta AWS e **região** do EKS | tudo (IAM, Bedrock, Secrets) é regional |
| 2 | Nome do cluster EKS e versão; os nós são x86 ou Graviton (arm64)? | trust policy; arquitetura da imagem |
| 3 | Add-ons instalados: **EKS Pod Identity Agent**, **Amazon EBS CSI driver**, StorageClass `gp3` | credencial e volume do Postgres |
| 4 | O CNI aplica NetworkPolicy? (VPC CNI com network policy, Calico, Cilium) | isolamento do Postgres |
| 5 | Os nós têm NAT ou só VPC endpoints? Quais existem (`bedrock-runtime`, `secretsmanager`, `eks-auth`, `ecr.api`, `ecr.dkr`, `s3`)? | o nautilm precisa alcançar esses serviços |
| 6 | Existe **AWS Load Balancer Controller**? Qual domínio interno e certificado ACM para o painel? | `public_url` precisa ser HTTPS |
| 7 | Saída para `login.microsoftonline.com:443` (proxy corporativo?) | login do painel |
| 8 | **Modelos de LLM aprovados** pela empresa; perfis cross-Region (`us.`/`global.`) são permitidos pelas SCPs? | apelidos e IAM |
| 9 | Quotas do Bedrock dessas contas/modelos (tokens/minuto e **por dia**) | conta nova pode vir com 0 |
| 10 | ElastiCache Valkey existente ou criar? | cotas e livro-caixa entre réplicas |
| 11 | Quem administra o tenant Entra? O tenant é P1/P2 ou Free? | App Registration, claim de grupos |
| 12 | Quem será **admin** do nautilm (cria tokens, muda config) e quem será **viewer** (vê custo)? | grupos |
| 13 | Quais agentes vão primeiro (piloto)? Estão no mesmo cluster do nautilm? | ServiceAccount × token `naut_` |
| 14 | Times e centros de custo de cada agente | divisão da conta |
| 15 | Onde fica a config deste ambiente (repo GitOps, Argo CD, Flux…)? | onde gravar os manifests preenchidos |

✅ Todas respondidas. Se 8 ou 9 estiverem em aberto, **pare**: sem modelo liberado com quota não há o que testar.

---

## Fase 1 — Bedrock pronto na conta

**Passos** (troque `<região>`):
```bash
aws bedrock list-foundation-models --region <região> --by-inference-type ON_DEMAND --query 'modelSummaries[].modelId'
aws bedrock list-inference-profiles --region <região> --query 'inferenceProfileSummaries[].inferenceProfileId'
aws service-quotas list-service-quotas --service-code bedrock --region <região> \
  --query 'Quotas[?contains(QuotaName, `tokens per day`)].[QuotaName,Value]' --output table
```
- 🧑 HUMANO: habilitar o acesso aos modelos de terceiros (Anthropic etc.) no catálogo do Bedrock, se
  ainda não estiver habilitado (a Anthropic pede um formulário de uso).
- Para cada modelo aprovado, anote: ID a chamar (direto ou perfil `us.`), se suporta **prompt cache**
  (Claude e Nova sim) e o **preço na região** (página de preços do Bedrock).

✅ Os modelos aprovados aparecem e as quotas de tokens por dia são > 0.
↩️ Nada a desfazer.

---

## Fase 2 — Imagem no ECR

O repositório **não publica imagem** (sem CI). Construa a partir de uma tag/commit conhecido:

```bash
git -C nautilm rev-parse --short HEAD                      # versão = commit
aws ecr create-repository --region <região> --repository-name nautilm \
  --image-scanning-configuration scanOnPush=true --image-tag-mutability IMMUTABLE      # 🧑 HUMANO (cria recurso)
aws ecr get-login-password --region <região> | docker login --username AWS --password-stdin <ACCOUNT_ID>.dkr.ecr.<região>.amazonaws.com
docker buildx build --platform linux/amd64 -t <ACCOUNT_ID>.dkr.ecr.<região>.amazonaws.com/nautilm:<commit> --push nautilm
#   nós Graviton: --platform linux/arm64 (ou linux/amd64,linux/arm64)
```
Se a empresa tiver pipeline de imagens (scan, assinatura, SBOM), use-a em vez do build manual.

✅ `aws ecr describe-images --repository-name nautilm` mostra a tag; o scan não tem CRITICAL.
↩️ `aws ecr batch-delete-image` na tag.

---

## Fase 3 — Role IAM com Pod Identity

**Entradas**: `<ACCOUNT_ID>`, `<CLUSTER_NAME>`, região, modelos (ARNs).

**Passos**
1. Copie `deployments/aws/nautilm-policy.json` e `nautilm-pod-identity-trust.json` para fora do repo.
2. Preencha os placeholders e **ajuste a região** (o exemplo usa us-east-2) e a lista de modelos:
   - modelo direto: `arn:aws:bedrock:<região>::foundation-model/<model-id>`;
   - perfil `us.`: o ARN do perfil + os ARNs do modelo nas regiões do perfil, com a condição
     `bedrock:InferenceProfileArn` (já modelado no arquivo para o Claude);
   - se a empresa não permite cross-Region, **remova** os statements de perfil.
3. Valide (sem efeito colateral):
   ```bash
   aws accessanalyzer validate-policy --policy-type IDENTITY_POLICY --policy-document file://nautilm-policy.json
   aws accessanalyzer validate-policy --policy-type RESOURCE_POLICY \
     --validate-policy-resource-type AWS::IAM::AssumeRolePolicyDocument --policy-document file://nautilm-pod-identity-trust.json
   ```
   Corrija **todo** achado (ERROR, SECURITY_WARNING, WARNING, SUGGESTION) antes de seguir.
4. 🧑 HUMANO aplica:
   ```bash
   aws iam create-role --role-name nautilm --assume-role-policy-document file://nautilm-pod-identity-trust.json
   aws iam put-role-policy --role-name nautilm --policy-name nautilm --policy-document file://nautilm-policy.json
   aws eks create-pod-identity-association --cluster-name <CLUSTER_NAME> --region <região> \
     --namespace llm-gateway --service-account nautilm --role-arn arn:aws:iam::<ACCOUNT_ID>:role/nautilm
   ```

✅ `aws eks list-pod-identity-associations --cluster-name <CLUSTER_NAME> --namespace llm-gateway` lista a associação.
↩️ `aws eks delete-pod-identity-association …`; `aws iam delete-role-policy` + `delete-role`.

Os agentes **não** recebem role de Bedrock. Se já tinham (IRSA própria), ela sai **depois** da migração.

---

## Fase 4 — Dados: Valkey e PostgreSQL

### 4a. Valkey (ElastiCache)
- 🧑 HUMANO cria (ou indica) um ElastiCache **Valkey** com **TLS em trânsito** e **AUTH**, security group
  liberando 6379 **só** a partir dos nós/pods do nautilm. Não usa IAM.
- URL: `rediss://:<auth-token>@<endpoint>:6379` → vai para a Secret `nautilm-secrets` (fase 6).

### 4b. PostgreSQL em pod (`deployments/k8s/postgres.yaml`)
1. `kubectl create namespace llm-gateway` (se ainda não existir).
2. 🧑 HUMANO gera a senha e grava nos **dois** lugares, sem mostrar na tela:
   ```bash
   PG=$(openssl rand -hex 24)
   kubectl -n llm-gateway create secret generic postgres-auth --from-literal=password="$PG"
   aws secretsmanager create-secret --region <região> --name nautilm/database \
     --secret-string "postgres://nautilm:$PG@postgres.llm-gateway:5432/nautilm?sslmode=disable"
   unset PG
   ```
   Anote o **ARN completo** da secret (termina em `-XXXXXX`).
3. `kubectl apply -f deployments/k8s/postgres.yaml`.

✅ `kubectl -n llm-gateway rollout status statefulset/postgres`; o PVC está `Bound`;
`kubectl -n llm-gateway create job --from=cronjob/postgres-backup backup-teste` termina com "backup ok".
↩️ `kubectl delete -f deployments/k8s/postgres.yaml` (**apaga os dados**: só antes de entrar em produção) e
`aws secretsmanager delete-secret`.

Alternativa (trade-off): RDS/Aurora PostgreSQL. Mais caro, backup e alta disponibilidade gerenciados. Para o
nautilm basta trocar a URL na secret `nautilm/database`.

---

## Fase 5 — Microsoft Entra ID

🧑 HUMANO (admin do tenant), no **Entra admin center**:
1. **App registrations → New registration**: nome `nautilm`, *Single tenant*, Redirect URI **Web**
   `https://<host-do-painel>/auth/callback` (o mesmo host do `public_url`).
2. Copie **Application (client) ID** e **Directory (tenant) ID** (não são segredos).
3. **Certificates & secrets → New client secret** (validade conforme política; **anote a data de
   expiração**: expirado = ninguém entra no painel).
4. **Token configuration → Add groups claim**:
   - P1/P2: **Groups assigned to the application**, ID token, *Group ID*; depois **Enterprise
     applications → nautilm → Users and groups** e atribua os grupos (e *Assignment required = Yes*).
   - Free: **Security groups** (o Free não atribui grupos a apps).
5. Grupos de segurança: admin (time de gestão do nautilm) e viewer (quem acompanha custo). Anote os
   **Object IDs**.
6. **Authentication**: implicit grant (access/ID tokens) **desmarcado**. API permissions: `User.Read` basta.

✅ Os IDs anotados; o client secret guardado pelo humano (não no chat).
↩️ Apagar a App Registration.

---

## Fase 6 — Config e segredos do nautilm

1. Copie `deployments/k8s/nautilm.yaml` para o repositório de deploy do ambiente e preencha:
   - `image:` → `<ACCOUNT_ID>.dkr.ecr.<região>.amazonaws.com/nautilm:<commit>`;
   - `providers:` → região real; `models:` → modelos aprovados (fase 1), com `prompt_cache: explicit`
     nos que suportam cache e serão usados por agentes com CAG;
   - `pricing:` → preços **da região**, com `effective_from`; deixe `strict_pricing: true`;
   - `agents:` → cada agente piloto (`namespace/serviceaccount`), `models`, `team`, `cost_center`; o
     orquestrador com `on_behalf_of: true`; `"*"` com o mínimo;
   - `dashboard.public_url` → `https://<host-do-painel>`;
   - `dashboard.entra` → `tenant_id`, `client_id`, `admin_groups`, `viewer_groups` (**sem** `issuer_url`);
   - `database.aws_secret_arn` → ARN completo da fase 4b;
   - `quotas:` → comece **folgado** (um 429 no meio de um troubleshooting é pior que um gasto alto em
     piloto); ajuste depois com os números do painel.
2. 🧑 HUMANO cria a Secret (valores sem eco):
   ```bash
   read -rs VK; read -rs ES
   kubectl -n llm-gateway create secret generic nautilm-secrets \
     --from-literal=VALKEY_URL="$VK" \
     --from-literal=NAUTILM_ENTRA_CLIENT_SECRET="$ES" \
     --from-literal=NAUTILM_DASHBOARD_TOKEN="$(openssl rand -hex 24)"
   unset VK ES
   ```
3. Valide o config **antes** de aplicar, com as mesmas regras da subida (sem rede, sem banco). Salve o
   `config.yaml` do ConfigMap preenchido num arquivo e rode com valores fictícios nas `${VAR}`:
   ```bash
   VALKEY_URL=x NAUTILM_DASHBOARD_TOKEN=x NAUTILM_ENTRA_CLIENT_SECRET=x go run ./cmd/nautilm -check -config config.yaml
   # config ok: config.yaml   (ou o erro, com a chave e a linha)
   ```
   Com `strict_pricing: true`, um apelido sem preço aparece aqui.

✅ `kubectl apply --dry-run=server -f nautilm.yaml` sem erro.

---

## Fase 7 — Subir, expor e isolar

1. `kubectl apply -f nautilm.yaml` (Namespace, SA, ClusterRoleBinding `system:auth-delegator`,
   ConfigMap, Deployment, PDB, Service).
2. **Ingress HTTPS** (o repositório não traz um, depende da empresa). Exemplo com AWS Load Balancer Controller:
   ```yaml
   apiVersion: networking.k8s.io/v1
   kind: Ingress
   metadata:
     name: nautilm
     namespace: llm-gateway
     annotations:
       alb.ingress.kubernetes.io/scheme: internal
       alb.ingress.kubernetes.io/target-type: ip
       alb.ingress.kubernetes.io/listen-ports: '[{"HTTPS":443}]'
       alb.ingress.kubernetes.io/certificate-arn: <ACM_CERT_ARN>
       alb.ingress.kubernetes.io/ssl-policy: ELBSecurityPolicy-TLS13-1-2-2021-06
       # respostas de LLM (e SSE) passam de 60 s: aumente o idle timeout
       alb.ingress.kubernetes.io/load-balancer-attributes: idle_timeout.timeout_seconds=300
       alb.ingress.kubernetes.io/healthcheck-path: /healthz
   spec:
     ingressClassName: alb
     rules:
       - host: <host-do-painel>
         http: {paths: [{path: /, pathType: Prefix, backend: {service: {name: nautilm, port: {number: 80}}}}]}
   ```
   Considere expor só `/dashboard`, `/auth/*`, `/v1/usage*` e `/v1/admin/*` para pessoas, e
   `/v1/chat/completions`/`/v1/embeddings` só para quem precisa de fora (trade-off: mais regras × menos superfície).
3. **NetworkPolicy de entrada** do nautilm (recomendado): liberar 8080 só para os namespaces dos agentes,
   o ingress controller (ou o ALB, com target-type ip, pela faixa da VPC), o mirante e o Prometheus.

✅ Logs de **cada** réplica (`kubectl -n llm-gateway logs deploy/nautilm`), nesta ordem:
`database connected` (source `aws_secret`) → `dashboard sign-in` (issuer `login.microsoftonline.com/<tenant>`) →
`protections` → `billing` → `nautilm up`. Sem `models without a price`. Sem `config overrides … NOT applied`.
↩️ `kubectl delete -f nautilm.yaml` e o Ingress.

---

## Fase 8 — Validação ponta a ponta

| # | Teste | Esperado |
|---|---|---|
| 1 | `curl https://<host>/healthz` | `ok` |
| 2 | 🧑 Pessoa do grupo admin entra em `/dashboard` | nome + "admin"; abas Tokens, Audit, Config |
| 3 | 🧑 Pessoa só viewer entra | só Usage; `/v1/admin/*` = 403 |
| 4 | Pessoa fora dos grupos | "Access denied" listando os IDs dos grupos dela |
| 5 | Aba Config | "In effect: revision 0", provedores/apelidos do arquivo, sem ⚠ |
| 6 | Chamada de um pod de teste com ServiceAccount listada em `agents` | 200; `X-Nautilm-Provider/Model` corretos |
| 7 | A mesma com apelido não permitido | 403 `forbidden_model` |
| 8 | Criar token `naut_` no painel e chamar de fora | 200; aparece no Usage com o time do token |
| 9 | Revogar o token | 401 em até 15 s nas duas réplicas |
| 10 | Usage após alguns minutos | custo por agente; nenhuma chamada "unpriced" |
| 11 | Prompt cache (agente com CAG, apelido `prompt_cache: explicit`) | a partir da 2ª chamada, `usage.prompt_tokens_details.cached_tokens` > 0 |
| 12 | `/metrics` raspado pelo Prometheus; painel `deployments/grafana/nautilm.json` importado | séries `nautilm_*` |
| 13 | Auditoria | logins, tokens e mudanças de config registrados |

Pod de teste para o item 6 (token projetado):
```bash
kubectl -n <ns-do-agente> run nautilm-smoke --rm -it --restart=Never --image=curlimages/curl \
  --overrides='{"spec":{"serviceAccountName":"<sa-do-agente>","containers":[{"name":"c","image":"curlimages/curl","command":["sh","-c","T=$(cat /var/run/secrets/nautilm/token); curl -s -H \"Authorization: Bearer $T\" -H content-type:application/json http://nautilm.llm-gateway/v1/chat/completions -d \"{\\\"model\\\":\\\"fast\\\",\\\"messages\\\":[{\\\"role\\\":\\\"user\\\",\\\"content\\\":\\\"say ok\\\"}]}\""],"volumeMounts":[{"name":"t","mountPath":"/var/run/secrets/nautilm"}]}],"volumes":[{"name":"t","projected":{"sources":[{"serviceAccountToken":{"path":"token","audience":"nautilm","expirationSeconds":3600}}]}}]}}'
```

Depois: migrar os agentes (`docs/MIGRACAO-AGENTES.md`).

---

## Fase 9 — Operação

| Rotina | Como |
|---|---|
| Rotacionar client secret do Entra | novo secret → `kubectl patch secret nautilm-secrets` → `rollout restart` → apagar o antigo (antes de expirar) |
| Rotacionar tokens `naut_` | aba Tokens → Rotate (o antigo segue válido até Revoke) |
| Senha do Postgres | trocar no Postgres, na Secret `postgres-auth` e em `nautilm/database`; `rollout restart` |
| Backup | CronJob diário (7 cópias) + snapshot EBS (AWS Backup); teste de restore periódico |
| Atualizar versão | nova imagem → editar tag → `rollout`. Migrações rodam sozinhas e são só para frente: **faça backup antes** |
| Preço mudou | aba Config → Prices com novo `effective_from` (histórico mantém o antigo) |
| Trocar modelo de um apelido | aba Config → Aliases → Change (testa antes de salvar; rollback no History) |
| Novo provedor | aba Config → Add provider (assistente) |
| Conciliar custo | mensal: Usage (estimado) × Cost Explorer por modelo |
| Alertas sugeridos | `nautilm_usage_lost_total` > 0, `nautilm_unpriced_calls_total` > 0, taxa de 5xx, `provider_quota_day` nos logs |

---

## Problemas comuns

| Sintoma | Causa provável | O que fazer |
|---|---|---|
| Agente recebe 401 com token de ServiceAccount | audience diferente de `nautilm`, ou agente em **outro cluster** | `audience: nautilm` no token projetado; outro cluster → token `naut_` |
| 503 `auth_unavailable`, log "TokenReview HTTP 403" | falta ClusterRoleBinding `system:auth-delegator` | aplicar o do `nautilm.yaml` |
| 403 `forbidden_model` | apelido fora de `agents.<id>.models` (ou do perfil do token) | ajustar config/token |
| 429 `provider_quota_day` | quota diária **da conta** no Bedrock | Service Quotas / caso de suporte; não é cota do nautilm |
| 429 `quota_day`/`quota_minute` | cota do nautilm para o agente | ajustar `quotas`/`agents.<id>` |
| 4xx/5xx `AccessDeniedException` do Bedrock | ARN do modelo/perfil fora da política | acrescentar ARN (fase 3) |
| "Access denied … NO groups" | App Registration sem groups claim | fase 5, passo 4 |
| "too many groups (overage)" | usuário com > 200 grupos | "Groups assigned to the application" |
| "code exchange failed" (detalhe no log) | client secret errado/expirado (copiou *Secret ID* em vez de *Value*?) | novo secret |
| Login volta com erro de redirect no Entra | Redirect URI ≠ `<public_url>/auth/callback` | corrigir na App Registration |
| POST no painel = 403 `csrf` | `public_url` diferente do host usado no navegador | alinhar `public_url` |
| `database_unavailable` / pod não sobe ("database not ready") | Postgres fora, NetworkPolicy, senha divergente | conferir `postgres-auth` × `nautilm/database` |
| Aba Config: "Dashboard changes NOT applied" | revisão do painel inválida com o arquivo atual | ler o erro; corrigir no arquivo ou restaurar revisão no History |
| Provedor com ⚠ "secret unreadable" / 503 `provider_unavailable` | role sem `GetSecretValue` (prefixo) ou secret apagada | statement `ReadNautilmProviderSecrets` |
| Respostas longas cortadas no painel/ALB | idle timeout do ALB (60 s) | anotação `idle_timeout.timeout_seconds` |
| Custo "unpriced" | preço ausente para provedor+modelo | aba Config → Prices |
