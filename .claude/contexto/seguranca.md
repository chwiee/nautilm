# Modelo de segurança do nautilm

## Identidades

| Quem | Como prova quem é | Onde é validado |
|---|---|---|
| Agente no **mesmo** EKS | token projetado da ServiceAccount, `audience: nautilm`, expira em 1h | `TokenReview` (ClusterRole `system:auth-delegator`), cache 1 min |
| Agente fora do cluster (ou em outro cluster) | token `naut_...` criado no painel | SHA-256 no Postgres, cache 15 s; validade 1 a 366 dias (padrão 90) |
| Automação de leitura (Grafana, scripts) | `dashboard.token` | só lê `/v1/usage` |
| Pessoa no painel | Microsoft Entra ID (OIDC Authorization Code + PKCE) | grupos do ID token → admin/viewer |

- Token do painel **nunca** pode usar `on_behalf_of`, e não pode reutilizar nome de agente do config (409).
- `auth.modes: [none]` aceita todo mundo: **só desenvolvimento**.

## Painel (Entra ID)

- `state` + `nonce` + verificador PKCE num cookie temporário (`/auth/`, 10 min); ID token validado
  (assinatura, emissor, audiência, nonce, `tid` = tenant).
- Sessão: cookie `nautilm_session` HttpOnly, SameSite=Lax, `Secure` quando `public_url` é https; no
  banco só o hash; duração `session_ttl`.
- **Papéis recalculados a cada requisição** a partir dos grupos guardados na sessão: tirar um grupo do
  mapa corta o acesso na próxima requisição. Admin inclui viewer.
- **CSRF**: todo POST de admin exige `Origin` igual ao `public_url`.
- **Overage** (mais de 200 grupos): negado com dica (o nautilm não chama o Microsoft Graph de propósito).
  Na App Registration use "Groups assigned to the application" (Entra ID P1+) ou "Security groups" (Free).
- Acesso negado mostra os IDs de grupo do próprio login (para mapear) e vai para a auditoria.

## Segredos

| Segredo | Onde | Nunca |
|---|---|---|
| URL/senha do Postgres | Secrets Manager `nautilm/database` + Secret `postgres-auth` | no ConfigMap, no log (a URL nunca é logada) |
| Chaves de provedores extras | Secrets Manager `nautilm/providers/<nome>` | no banco (só o ARN), na auditoria, nas mensagens de erro (só nomes de chaves) |
| Client secret do Entra, token do painel, `VALKEY_URL` | Secret `nautilm-secrets` | no Git |
| Tokens `naut_` | mostrados **uma vez** ao criar | em claro no banco |

Prefixo `naut_` facilita achar token vazado com scanners (Gitleaks etc.).

## IAM (role do nautilm via EKS Pod Identity)

Arquivos: `deployments/aws/nautilm-policy.json` e `nautilm-pod-identity-trust.json` (placeholders
`<ACCOUNT_ID>`, `<CLUSTER_NAME>`; região us-east-2 no exemplo, ajustar).

- Invocação **por ARN de modelo** (`bedrock:InvokeModel` + `InvokeModelWithResponseStream`); perfis
  `us.` com condição `bedrock:InferenceProfileArn`. Modelo novo = acrescentar o ARN.
- `secretsmanager:GetSecretValue` só em `nautilm/database-??????` e `nautilm/providers/*`.
  `kms:Decrypt` apenas se a secret usar CMK.
- Trust restrito por `aws:SourceAccount` e `aws:SourceArn` (só o seu cluster assume).
- A versão com os statements de banco foi validada no IAM Access Analyzer sem achados; o statement
  `ReadNautilmProviderSecrets` foi acrescentado depois e **deve ser revalidado** antes de aplicar.
- Agentes **não** têm role de Bedrock.

## Rede

Ver tabela "3. Rede" do README. Resumo: nautilm → Bedrock Runtime e Secrets Manager via VPC
endpoints (sem NAT; o Pod Identity Agent do nó precisa alcançar o endpoint `eks-auth`); → API do Kubernetes; → ElastiCache 6379 TLS; → Postgres 5432 (NetworkPolicy do
`postgres.yaml`); → `login.microsoftonline.com` 443 (login). Navegador → nautilm só por **HTTPS**.

## Dados enviados ao modelo

- **Mascaramento antes de qualquer modelo** (mensagens, tool results e argumentos, embeddings):
  `private_key, jwt, aws_key, bearer, secret, cpf, cnpj, credit_card, email` + regras `extra`. O agente
  vê `X-Nautilm-Masked: cpf=1,…`; log/métrica guardam só o nome da regra.
- A marca de prompt cache passa pelo mesmo mascaramento (testado).
- Limite: regex não reconhece segredo sem formato.

## Contêiner

Distroless `static-debian12:nonroot`, UID 65532, `readOnlyRootFilesystem`, `drop: [ALL]`, sem shell.
