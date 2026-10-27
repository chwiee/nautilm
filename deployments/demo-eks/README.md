# Demo do nautilm num EKS existente (um namespace, sem disco)

Tudo num namespace (`nautilm-demo`): **nautilm** (2 réplicas), **Valkey** e **PostgreSQL** em pods
**sem volume**, Secrets do Kubernetes e acesso por **port-forward**. Feito para uma pessoa aprender,
testar e saturar sem gerar custo de infraestrutura.

> **Sem disco = sem memória.** Apagar ou reagendar o pod do Postgres perde tokens criados no painel,
> mudanças da aba Config, auditoria e sessões (é só logar de novo). Reiniciar o Valkey zera os totais
> de custo e as cotas. A config base (ConfigMap) e as Secrets ficam.

| | Demo (este diretório) | Produção (futuro) |
|---|---|---|
| Postgres | pod sem disco | RDS (troca `database.url` por `database.aws_secret_arn`) |
| Valkey | pod sem persistência | ElastiCache (troca `valkey_url`) |
| Segredos | Secret do Kubernetes | Secrets Manager + Secret |
| Acesso ao painel | `kubectl port-forward` | Ingress/ALB com HTTPS |
| Custo além do Bedrock | ~zero (nós já existentes + imagem no ECR) | RDS, ElastiCache, ALB |

O **único custo real é o Bedrock** (tokens). O teto é a cota do nautilm por agente
(`quotas.tokens_per_day: 1000000` no ConfigMap): com Nova Lite, 1M tokens ≈ US$ 0,10 a 0,25.

## Pré-requisitos

1. `kubectl` apontando para o EKS da demo; add-on **EKS Pod Identity Agent** instalado.
2. **Bedrock** na região: modelos padrão da demo liberados e com quota > 0 (inclusive tokens por dia):
   ```bash
   aws bedrock list-foundation-models --region <REGION> --by-inference-type ON_DEMAND \
     --query 'modelSummaries[?contains(modelId, `nova`) || contains(modelId, `titan-embed-text-v2`)].modelId'
   ```
   Padrão: `amazon.nova-lite-v1:0` (fast/cheap), `amazon.nova-pro-v1:0` (smart),
   `amazon.titan-embed-text-v2:0` (embed). Outros modelos: `FAST_MODEL`/`SMART_MODEL`/`EMBED_MODEL`
   no `up.sh` + ARN na política + preço na aba Config.
3. **Imagem** no ECR da conta (o repositório não publica imagem):
   ```bash
   aws ecr create-repository --region <REGION> --repository-name nautilm
   aws ecr get-login-password --region <REGION> | docker login --username AWS --password-stdin <ACCOUNT_ID>.dkr.ecr.<REGION>.amazonaws.com
   docker buildx build --platform linux/amd64 --push -t <ACCOUNT_ID>.dkr.ecr.<REGION>.amazonaws.com/nautilm:$(git rev-parse --short HEAD) .
   ```
   (nós Graviton: `--platform linux/arm64`)
4. **Role IAM + Pod Identity** (só Bedrock; a demo não usa Secrets Manager):
   ```bash
   # preencha <REGION>, <ACCOUNT_ID>, <CLUSTER_NAME> (e a região no ARN do cluster) em cópias fora do Git
   sed "s/<REGION>/$REGION/g" deployments/demo-eks/iam-policy.json > /tmp/demo-policy.json
   aws accessanalyzer validate-policy --policy-type IDENTITY_POLICY --policy-document file:///tmp/demo-policy.json
   aws iam create-role --role-name nautilm-demo --assume-role-policy-document file:///tmp/trust.json   # de deployments/aws/nautilm-pod-identity-trust.json
   aws iam put-role-policy --role-name nautilm-demo --policy-name bedrock --policy-document file:///tmp/demo-policy.json
   aws eks create-pod-identity-association --cluster-name <CLUSTER_NAME> --region <REGION> \
     --namespace nautilm-demo --service-account nautilm --role-arn arn:aws:iam::<ACCOUNT_ID>:role/nautilm-demo
   ```
5. **Entra ID**: App Registration com redirect `http://localhost:18095/auth/callback` (Web), groups claim e
   dois grupos (admin e viewer). O app do laboratório local já serve (mesmo redirect).

## Subir

```bash
IMAGE=<ACCOUNT_ID>.dkr.ecr.<REGION>.amazonaws.com/nautilm:<tag> REGION=<REGION> \
ENTRA_TENANT_ID=<tenant> ENTRA_CLIENT_ID=<client> ENTRA_ADMIN_GROUP=<grupo-admin> ENTRA_VIEWER_GROUP=<grupo-viewer> \
bash deployments/demo-eks/up.sh
```
Na primeira vez ele pede o **client secret** do Entra (digitação oculta) e gera as demais senhas e tokens
aleatórios, só no cluster. Rodar de novo atualiza config e imagem sem perder as Secrets.

✅ `kubectl -n nautilm-demo logs deploy/nautilm` mostra `database connected` → `dashboard sign-in` →
`nautilm up` nas duas réplicas.

## Usar

```bash
kubectl -n nautilm-demo port-forward svc/nautilm 18095:80     # painel: http://localhost:18095/dashboard
```
A porta 18095 é a mesma do laboratório kind: se ele estiver rodando, apague antes
(`kind delete cluster --name nautilm-lab`) ou o port-forward não sobe.

**Agente com ServiceAccount** (identidade `nautilm-demo/demo-agent`, sem segredo):
```bash
kubectl -n nautilm-demo run smoke --rm -i --restart=Never --image=curlimages/curl \
  --overrides='{"spec":{"serviceAccountName":"demo-agent","containers":[{"name":"smoke","image":"curlimages/curl","command":["sh","-c","curl -s -H \"Authorization: Bearer $(cat /t/token)\" -H content-type:application/json http://nautilm/v1/chat/completions -d {\\\"model\\\":\\\"fast\\\",\\\"messages\\\":[{\\\"role\\\":\\\"user\\\",\\\"content\\\":\\\"say_ok\\\"}]}"],"volumeMounts":[{"name":"t","mountPath":"/t"}]}],"volumes":[{"name":"t","projected":{"sources":[{"serviceAccountToken":{"path":"token","audience":"nautilm","expirationSeconds":3600}}]}}]}}'
```

**Saturar** com os 4 agentes de exemplo (k8s, mid, marcia, claudia), pelo port-forward:
```bash
for a in K8S MID MARCIA CLAUDIA; do
  export NAUTILM_TOKEN_$a="$(kubectl -n nautilm-demo get secret nautilm-secrets -o jsonpath="{.data.NAUTILM_TOKEN_$a}" | base64 -d)"
done
go run ./examples/agents -url http://localhost:18095 -probes            # até a cota acabar (429 quota_day)
go run ./examples/agents -url http://localhost:18095 -alias smart -only mid -rounds 20
```
Roteiro sugerido: cotas (429), fallback (`smart` → `fast`), mascaramento (`-probes`), custo por agente/time
no Usage, token `naut_` criado/rotacionado/revogado no painel, aba Config (trocar o modelo do `fast`,
preço, rollback no History), matar uma réplica do nautilm durante a carga, matar o Postgres e ver o que se perde.

Mudar cotas ou agentes: edite o ConfigMap (`kubectl -n nautilm-demo edit configmap nautilm`) e
`kubectl -n nautilm-demo rollout restart deployment/nautilm`. Apelidos e preços: aba Config, sem reiniciar.

## Problemas comuns

| Sintoma | Causa | O que fazer |
|---|---|---|
| Chamada responde `… get credentials … no EC2 IMDS role found` | falta a associação do **Pod Identity** (ou o add-on) | pré-requisito 4; depois `kubectl -n nautilm-demo rollout restart deployment/nautilm` |
| `AccessDeniedException` do Bedrock | modelo fora da política | acrescentar o ARN em `iam-policy.json` e reaplicar |
| 429 `provider_quota_day` | quota diária **da conta** no Bedrock (conta nova pode vir com 0) | Service Quotas / suporte |
| 429 `quota_day` | teto da demo (cota do nautilm) | era o esperado; aumente no ConfigMap se quiser |
| Port-forward não sobe (porta em uso) | laboratório kind usando a 18095 | `kind delete cluster --name nautilm-lab` |
| Login: "code exchange failed" | client secret errado (copiou o *Secret ID*?) | apague as Secrets (`kubectl -n nautilm-demo delete secret postgres-auth nautilm-secrets`) e rode o `up.sh` de novo |
| Painel pede login de novo / tokens sumiram | o pod do Postgres foi recriado (sem disco) | esperado na demo |

## Apagar

```bash
kubectl delete namespace nautilm-demo
kubectl delete clusterrolebinding nautilm-demo-auth-delegator
aws eks delete-pod-identity-association --cluster-name <CLUSTER_NAME> --region <REGION> --association-id <id>
aws iam delete-role-policy --role-name nautilm-demo --policy-name bedrock && aws iam delete-role --role-name nautilm-demo
aws ecr delete-repository --region <REGION> --repository-name nautilm --force     # se não for usar mais
```
