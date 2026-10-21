// Package bedrock traduz o formato OpenAI para a Converse API do Amazon
// Bedrock e volta. Baseado no submódulo bedrock do gotoolkit.
//
// Credenciais: cadeia padrão da AWS (Pod Identity, IRSA, perfil local).
// SÓ o pedagio precisa de permissão no Bedrock — esse é o objetivo:
//
//	bedrock:InvokeModel                   (Converse)
//	bedrock:InvokeModelWithResponseStream (ConverseStream, quando o agente pede stream)
//
// Por que Converse e não InvokeModel? Um formato só de mensagens e de tool
// calling para todos os modelos (Claude, Nova, Llama...). Trocar de modelo é
// trocar o ID no config do pedagio.
package bedrock

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/bedrockruntime"
	"github.com/aws/aws-sdk-go-v2/service/bedrockruntime/document"
	"github.com/aws/aws-sdk-go-v2/service/bedrockruntime/types"
	"github.com/aws/smithy-go"

	"github.com/chwiee/pedagio/internal/provider"
	"github.com/chwiee/pedagio/pkg/openai"
)

type Config struct {
	Region string // vazio = AWS_REGION
}

// Converser é a parte do SDK que usamos — interface para dar para testar.
type Converser interface {
	Converse(ctx context.Context, in *bedrockruntime.ConverseInput, opts ...func(*bedrockruntime.Options)) (*bedrockruntime.ConverseOutput, error)
}

type Provider struct {
	api    Converser
	stream StreamAPI // nil = sem streaming real (o gateway simula)
}

// New carrega as credenciais padrão da AWS.
func New(ctx context.Context, cfg Config) (*Provider, error) {
	var opts []func(*config.LoadOptions) error
	if cfg.Region != "" {
		opts = append(opts, config.WithRegion(cfg.Region))
	}
	awsCfg, err := config.LoadDefaultConfig(ctx, opts...)
	if err != nil {
		return nil, fmt.Errorf("bedrock: carregando credenciais AWS: %w", err)
	}
	client := bedrockruntime.NewFromConfig(awsCfg)
	return NewWithAPI(client, sdkStream{client}), nil
}

// NewWithAPI injeta os clientes (testes). stream pode ser nil.
func NewWithAPI(api Converser, stream StreamAPI) *Provider {
	return &Provider{api: api, stream: stream}
}

func (p *Provider) Chat(ctx context.Context, c provider.Call) (*openai.ChatResponse, error) {
	in, err := buildInput(c)
	if err != nil {
		return nil, err
	}
	out, err := p.api.Converse(ctx, in)
	if err != nil {
		return nil, mapError(err)
	}
	return parseOutput(c.Model, out)
}

// buildInput faz a tradução de ida. As diferenças que importam:
//
//  1. System prompt NÃO é mensagem: vai no campo System.
//  2. Só existem user e assistant. Resultado de tool é um bloco toolResult
//     DENTRO de uma mensagem user.
//  3. Papéis precisam alternar. Mensagens seguidas do mesmo papel (ex: dois
//     resultados de tool) são fundidas numa só.
//  4. Argumentos de tool na OpenAI são string JSON; na Converse, documento.
func buildInput(c provider.Call) (*bedrockruntime.ConverseInput, error) {
	r := c.Request
	if r.ResponseFormat != nil && r.ResponseFormat.Type == "json_schema" {
		return nil, provider.Errorf(http.StatusBadRequest, "unsupported", "response_format json_schema ainda não é suportado no provider bedrock")
	}

	in := &bedrockruntime.ConverseInput{ModelId: aws.String(c.Model)}
	inf := &types.InferenceConfiguration{}
	if mt := r.EffectiveMaxTokens(); mt > 0 {
		inf.MaxTokens = aws.Int32(int32(mt))
	} else if c.DefaultMaxTokens > 0 {
		inf.MaxTokens = aws.Int32(int32(c.DefaultMaxTokens))
	}
	if r.Temperature != nil {
		inf.Temperature = aws.Float32(float32(*r.Temperature))
	}
	if r.TopP != nil {
		inf.TopP = aws.Float32(float32(*r.TopP))
	}
	if len(r.Stop) > 0 {
		inf.StopSequences = r.Stop
	}
	in.InferenceConfig = inf

	for i, m := range r.Messages {
		if m.Content.HasNonText {
			return nil, provider.Errorf(http.StatusBadRequest, "unsupported", "mensagem %d: só conteúdo de texto é suportado (imagens ainda não)", i)
		}
		var role types.ConversationRole
		var blocks []types.ContentBlock
		switch m.Role {
		case "system", "developer":
			if m.Content.Text != "" {
				in.System = append(in.System, &types.SystemContentBlockMemberText{Value: m.Content.Text})
			}
			continue
		case "user":
			role = types.ConversationRoleUser
			blocks = append(blocks, &types.ContentBlockMemberText{Value: nonEmpty(m.Content.Text)})
		case "assistant":
			role = types.ConversationRoleAssistant
			if m.Content.Text != "" {
				blocks = append(blocks, &types.ContentBlockMemberText{Value: m.Content.Text})
			}
			for _, tc := range m.ToolCalls {
				args, err := argsDoc(tc.Function.Arguments)
				if err != nil {
					return nil, provider.Errorf(http.StatusBadRequest, "invalid_request", "argumentos da tool %s não são JSON: %v", tc.Function.Name, err)
				}
				blocks = append(blocks, &types.ContentBlockMemberToolUse{Value: types.ToolUseBlock{
					ToolUseId: aws.String(tc.ID), Name: aws.String(tc.Function.Name), Input: document.NewLazyDocument(args),
				}})
			}
		case "tool":
			role = types.ConversationRoleUser
			blocks = append(blocks, &types.ContentBlockMemberToolResult{Value: types.ToolResultBlock{
				ToolUseId: aws.String(m.ToolCallID),
				Content:   []types.ToolResultContentBlock{&types.ToolResultContentBlockMemberText{Value: nonEmpty(m.Content.Text)}},
			}})
		default:
			return nil, provider.Errorf(http.StatusBadRequest, "invalid_request", "papel desconhecido %q", m.Role)
		}
		if len(blocks) == 0 {
			continue
		}
		if n := len(in.Messages); n > 0 && in.Messages[n-1].Role == role {
			in.Messages[n-1].Content = append(in.Messages[n-1].Content, blocks...)
			continue
		}
		in.Messages = append(in.Messages, types.Message{Role: role, Content: blocks})
	}
	if len(in.Messages) == 0 {
		return nil, provider.Errorf(http.StatusBadRequest, "invalid_request", "nenhuma mensagem user/assistant")
	}

	if len(r.Tools) > 0 {
		tc := &types.ToolConfiguration{}
		for _, t := range r.Tools {
			schema, err := schemaDoc(t.Function.Parameters)
			if err != nil {
				return nil, provider.Errorf(http.StatusBadRequest, "invalid_request", "schema da tool %s: %v", t.Function.Name, err)
			}
			tc.Tools = append(tc.Tools, &types.ToolMemberToolSpec{Value: types.ToolSpecification{
				Name:        aws.String(t.Function.Name),
				Description: aws.String(nonEmpty(t.Function.Description)),
				InputSchema: &types.ToolInputSchemaMemberJson{Value: document.NewLazyDocument(schema)},
			}})
		}
		tc.ToolChoice = toolChoice(r.ToolChoice)
		in.ToolConfig = tc
	}
	return in, nil
}

// toolChoice: "auto" → Auto, "required" → Any, {"function":{"name":X}} → Tool X.
// "none" não existe na Converse: fica como auto (as tools precisam continuar
// declaradas se o histórico tiver toolUse).
func toolChoice(v any) types.ToolChoice {
	switch x := v.(type) {
	case string:
		if x == "required" {
			return &types.ToolChoiceMemberAny{Value: types.AnyToolChoice{}}
		}
	case map[string]any:
		if fn, ok := x["function"].(map[string]any); ok {
			if name, ok := fn["name"].(string); ok && name != "" {
				return &types.ToolChoiceMemberTool{Value: types.SpecificToolChoice{Name: aws.String(name)}}
			}
		}
	}
	return nil // nil = auto (padrão da Converse)
}

// parseOutput faz a tradução de volta.
func parseOutput(model string, out *bedrockruntime.ConverseOutput) (*openai.ChatResponse, error) {
	msg, ok := out.Output.(*types.ConverseOutputMemberMessage)
	if !ok {
		return nil, provider.Errorf(http.StatusBadGateway, "upstream_invalid", "saída inesperada do Bedrock (stopReason=%s)", out.StopReason)
	}
	res := openai.ResponseMessage{Role: "assistant"}
	var text strings.Builder
	for _, b := range msg.Value.Content {
		switch v := b.(type) {
		case *types.ContentBlockMemberText:
			text.WriteString(v.Value)
		case *types.ContentBlockMemberToolUse:
			// O "document" do SDK já sabe se serializar em JSON — é exatamente
			// o que a OpenAI quer em arguments (string JSON).
			raw := []byte("{}")
			if v.Value.Input != nil {
				b, err := v.Value.Input.MarshalSmithyDocument()
				if err != nil {
					return nil, provider.Errorf(http.StatusBadGateway, "upstream_invalid", "argumentos de %s: %v", aws.ToString(v.Value.Name), err)
				}
				if len(b) > 0 && string(b) != "null" {
					raw = b
				}
			}
			res.ToolCalls = append(res.ToolCalls, openai.ToolCall{
				ID: aws.ToString(v.Value.ToolUseId), Type: "function",
				Function: openai.FunctionCall{Name: aws.ToString(v.Value.Name), Arguments: string(raw)},
			})
		}
	}
	if s := text.String(); s != "" || len(res.ToolCalls) == 0 {
		res.Content = &s
	}

	resp := &openai.ChatResponse{
		Object:  "chat.completion",
		Created: time.Now().Unix(),
		Model:   model,
		Choices: []openai.Choice{{Message: res, FinishReason: finishReason(out.StopReason)}},
	}
	if u := out.Usage; u != nil {
		in, outT := int(aws.ToInt32(u.InputTokens)), int(aws.ToInt32(u.OutputTokens))
		resp.Usage = &openai.Usage{PromptTokens: in, CompletionTokens: outT, TotalTokens: in + outT}
		if cached := int(aws.ToInt32(u.CacheReadInputTokens)); cached > 0 {
			resp.Usage.PromptTokensDetails = &openai.PromptTokensDetails{CachedTokens: cached}
		}
	}
	return resp, nil
}

func finishReason(r types.StopReason) string {
	switch r {
	case types.StopReasonToolUse:
		return "tool_calls"
	case types.StopReasonMaxTokens:
		return "length"
	case types.StopReasonGuardrailIntervened, types.StopReasonContentFiltered:
		return "content_filter"
	default:
		return "stop"
	}
}

// mapError converte erros da AWS em status HTTP para o gateway decidir
// entre devolver ao agente ou tentar o fallback.
func mapError(err error) error {
	var api smithy.APIError
	if !errors.As(err, &api) {
		if errors.Is(err, context.DeadlineExceeded) {
			return provider.Errorf(http.StatusGatewayTimeout, "timeout", "Bedrock não respondeu a tempo")
		}
		return provider.Errorf(http.StatusBadGateway, "upstream_error", "%v", err)
	}
	status := http.StatusBadGateway
	switch api.ErrorCode() {
	case "ValidationException":
		status = http.StatusBadRequest
	case "ThrottlingException", "ServiceQuotaExceededException":
		status = http.StatusTooManyRequests
	case "ModelTimeoutException":
		status = http.StatusGatewayTimeout
	case "ServiceUnavailableException", "ModelNotReadyException", "InternalServerException":
		status = http.StatusServiceUnavailable
	case "AccessDeniedException", "ResourceNotFoundException":
		// Erro de configuração do PEDAGIO (IAM ou modelo não habilitado),
		// não do agente: 502 e tenta o fallback.
		status = http.StatusBadGateway
	}
	return provider.Errorf(status, api.ErrorCode(), "%s", api.ErrorMessage())
}

// argsDoc converte a string JSON de argumentos em valor genérico. O
// "document" do SDK precisa de map/slice, não de bytes.
func argsDoc(s string) (any, error) {
	if strings.TrimSpace(s) == "" {
		return map[string]any{}, nil
	}
	var v any
	err := json.Unmarshal([]byte(s), &v)
	return v, err
}

func schemaDoc(raw json.RawMessage) (any, error) {
	if len(raw) == 0 || string(raw) == "null" {
		return map[string]any{"type": "object", "properties": map[string]any{}}, nil
	}
	var v any
	err := json.Unmarshal(raw, &v)
	return v, err
}

// A Converse rejeita blocos de texto vazios.
func nonEmpty(s string) string {
	if s == "" {
		return " "
	}
	return s
}
