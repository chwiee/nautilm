package bedrock

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/bedrockruntime"

	"github.com/chwiee/pedagio/internal/provider"
	"github.com/chwiee/pedagio/pkg/openai"
)

// Invoker é a parte do SDK usada para embeddings (InvokeModel: a Converse
// não gera embeddings). Mesma permissão IAM: bedrock:InvokeModel.
type Invoker interface {
	InvokeModel(ctx context.Context, in *bedrockruntime.InvokeModelInput, opts ...func(*bedrockruntime.Options)) (*bedrockruntime.InvokeModelOutput, error)
}

// Embed gera embeddings com o Amazon Titan Text Embeddings (v2).
//
// O Titan recebe UM texto por chamada, então fazemos uma chamada por item.
// Para lotes grandes, vale paralelizar com limite (Bedrock tem cota de TPS).
//
// Formato do Titan v2:
//
//	pedido:   {"inputText": "...", "dimensions": 1024, "normalize": true}
//	resposta: {"embedding": [...], "inputTextTokenCount": 7}
func (p *Provider) Embed(ctx context.Context, c provider.EmbedCall) ([][]float32, *openai.Usage, error) {
	if p.invoke == nil {
		return nil, nil, provider.Errorf(http.StatusNotImplemented, "unsupported", "embeddings não configurados no provider bedrock")
	}
	if !strings.Contains(c.Model, "titan-embed") {
		return nil, nil, provider.Errorf(http.StatusBadRequest, "unsupported", "modelo %q: o pedagio só traduz embeddings do Titan (amazon.titan-embed-text-v2:0)", c.Model)
	}
	vecs := make([][]float32, len(c.Input))
	usage := &openai.Usage{}
	for i, text := range c.Input {
		body := map[string]any{"inputText": text, "normalize": true}
		if c.Dimensions > 0 {
			body["dimensions"] = c.Dimensions
		}
		b, _ := json.Marshal(body)
		out, err := p.invoke.InvokeModel(ctx, &bedrockruntime.InvokeModelInput{
			ModelId: aws.String(c.Model), Body: b,
			ContentType: aws.String("application/json"), Accept: aws.String("application/json"),
		})
		if err != nil {
			return nil, nil, mapError(err)
		}
		var res struct {
			Embedding           []float32 `json:"embedding"`
			InputTextTokenCount int       `json:"inputTextTokenCount"`
		}
		if err := json.Unmarshal(out.Body, &res); err != nil || len(res.Embedding) == 0 {
			return nil, nil, provider.Errorf(http.StatusBadGateway, "upstream_invalid", "resposta do Titan sem embedding: %v", err)
		}
		vecs[i] = res.Embedding
		usage.PromptTokens += res.InputTextTokenCount
	}
	usage.TotalTokens = usage.PromptTokens
	return vecs, usage, nil
}
