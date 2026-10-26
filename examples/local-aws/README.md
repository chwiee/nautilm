# Teste local contra o Bedrock real (us-east-2)

nautilm rodando na sua máquina, chamando o **Bedrock de verdade** com o perfil
`lab` (`aws login --profile lab`) e caindo para o Ollama local quando o
Bedrock recusa. Quatro agentes simples ([../agents](../agents)) gastam tokens
até a cota acabar.

```powershell
aws login --profile lab                       # credencial vale ~12h
& .\examples\local-aws\run.ps1                # nautilm em :18090 (tokens só de teste)
go run ./examples/agents -url http://localhost:18090 -probes
go run ./examples/agents -url http://localhost:18090 -alias compare   # 2º modelo (Nova Lite)
```

Painel: http://localhost:18090/dashboard (token `local-dashboard`).
Pré-requisitos: Ollama com `qwen2.5:7b` e o Valkey do compose do
plataforma-agentes (`localhost:16392`; o teste usa o banco 1 para não misturar
com o livro-caixa do compose).

## Modelos escolhidos (tabela de preços AWS, us-east-2, 23/10/2026)

| Apelido | Modelo | Entrada / saída por 1M | Por quê |
|---|---|---|---|
| `cheap` | `google.gemma-3-4b-it` | US$ 0,04 / 0,08 | o mais barato com **ON_DEMAND** na região |
| `compare` | `amazon.nova-lite-v1:0` | US$ 0,06 / 0,24 | 2º modelo, para ver o valor mudar |
| `local` | `qwen2.5:7b` (Ollama) | grátis | fallback |

Nova Micro (mais barato na entrada) e Claude Haiku 5.5 só existem como
**inference profile cross-Region** (`us.`/`global.`), que o projeto de região
única não permite.

## Resultado do 1º teste (26/10/2026)

**A conta tem quota zero no Bedrock para esses modelos.** Toda chamada recebeu
`ThrottlingException: Too many tokens per day`, inclusive a primeira do dia:

| Quota (código) | Aplicada na conta | Padrão da AWS | Ajustável no console |
|---|---|---|---|
| Gemma 3 4B, tokens/minuto on-demand (`L-73FB8466`) | **0** | 100.000.000 | não |
| Gemma 3 4B, requests/minuto on-demand (`L-3056DF33`) | **0** | 10.000 | não |
| Nova Lite, tokens/dia (`L-45E0AD92`) | **0** | 5.760.000.000 | não |

É a restrição que a AWS aplica em contas novas; só sai por **caso no Support
Center** ("Service limit increase" → Amazon Bedrock → us-east-2), que o plano
Basic gratuito permite. Para o teste, valores pequenos bastam (ex: 100 mil
tokens/minuto e 10 requests/minuto).

O que o teste comprovou mesmo assim:

- **Erros conhecidos** (`-probes`): sem token e token errado → 401; apelido
  inexistente e ID cru de modelo → 404; apelido não liberado → 403; mensagem
  vazia → 400; agente tentando cobrar outro → header ignorado, cobrado dele mesmo.
- **Bedrock recusando (429)** → fallback automático para o local, em todas as
  497 chamadas; o agente nunca viu o erro.
- **Livro-caixa**: cada tentativa no Bedrock registrada como erro com custo
  zero; o custo foi para quem atendeu (local, grátis). Os totais do livro-caixa
  batem com os dos headers que os agentes receberam.
- **Cota do nautilm**: o `mid` (respostas longas) estourou os 25 mil tokens do
  dia → `429 quota_day`, `Retry-After` até a meia-noite UTC.
- **Custo na AWS: zero.** Chamada recusada por quota não é cobrada.

Achado para melhorar: cada chamada gasta em média **0,42 s** tentando o Bedrock
(o SDK da AWS faz 2 retries) antes do fallback. Com quota **diária** zerada,
insistir a cada chamada é desperdício: vale pausar o modelo por alguns minutos
quando o erro diz "per day".

## Atualização: erro claro de quota da conta (26/10/2026)

A pedido, sem pausa automática: quando a quota diária da **conta AWS** acaba,
o agente recebe um erro próprio, diferente da cota do nautilm. Os apelidos
`cheap` e `compare` ficaram **sem fallback** neste exemplo para o erro aparecer:

```json
HTTP 429
{"error": {"type": "rate_limit_error", "code": "provider_quota_day",
  "message": "the AWS account reached the daily token limit of model google.gemma-3-4b-it on Amazon Bedrock. This is a provider account limit, not a nautilm quota: it resets the next day, or ask AWS to raise it (Service Quotas / Support case). AWS said: Too many tokens per day, please wait before trying again."}}
```

O SDK da AWS não repete mais essa chamada: **0,24 s** em vez de ~0,42 s.

### Quais quotas pedir à AWS

Na tela do Service Quotas aparecem dezenas de quotas do Bedrock. Para este
teste só importam estas (região **US East (Ohio)**):

| Para | Nome no console | Código | Sugerido |
|---|---|---|---|
| Gemma 3 4B (`cheap`) | On-demand model inference tokens per minute for Gemma 3 4B | `L-73FB8466` | 100.000 |
| Gemma 3 4B (`cheap`) | On-demand model inference requests per minute for Gemma 3 4B | `L-3056DF33` | 10 |
| Nova Lite (`compare`) | Model invocation max tokens per day for Amazon Nova Lite | `L-45E0AD92` | 1.000.000 |

Ignore as **Cross-region** (o projeto de região única não pode usar), **batch**,
**custom** e **provisioned**. As três são **não ajustáveis** no Service Quotas
(o botão "Request increase" fica desabilitado): abra pelo **Support Center** →
Create case → **Service limit increase** → Bedrock → US East (Ohio), citando
os códigos.
