package bedrock

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/bedrockruntime"

	"github.com/chwiee/nautilm/internal/provider"
	"github.com/chwiee/nautilm/pkg/openai"
)

// Invoker is the SDK part used for embeddings (InvokeModel: Converse does
// not generate embeddings). Same IAM permission: bedrock:InvokeModel.
type Invoker interface {
	InvokeModel(ctx context.Context, in *bedrockruntime.InvokeModelInput, opts ...func(*bedrockruntime.Options)) (*bedrockruntime.InvokeModelOutput, error)
}

// Embed generates embeddings with Amazon Titan Text Embeddings (v2).
//
// Titan takes ONE text per call, so we make one call per item. For large
// batches it is worth parallelizing with a limit (Bedrock has a TPS quota).
//
// Titan v2 format:
//
//	request:  {"inputText": "...", "dimensions": 1024, "normalize": true}
//	response: {"embedding": [...], "inputTextTokenCount": 7}
func (p *Provider) Embed(ctx context.Context, c provider.EmbedCall) ([][]float32, *openai.Usage, error) {
	if p.invoke == nil {
		return nil, nil, provider.Errorf(http.StatusNotImplemented, "unsupported", "embeddings not configured in the bedrock provider")
	}
	if !strings.Contains(c.Model, "titan-embed") {
		return nil, nil, provider.Errorf(http.StatusBadRequest, "unsupported", "model %q: nautilm only translates Titan embeddings (amazon.titan-embed-text-v2:0)", c.Model)
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
			return nil, nil, mapError(err, c.Model)
		}
		var res struct {
			Embedding           []float32 `json:"embedding"`
			InputTextTokenCount int       `json:"inputTextTokenCount"`
		}
		if err := json.Unmarshal(out.Body, &res); err != nil || len(res.Embedding) == 0 {
			return nil, nil, provider.Errorf(http.StatusBadGateway, "upstream_invalid", "Titan response without embedding: %v", err)
		}
		vecs[i] = res.Embedding
		usage.PromptTokens += res.InputTextTokenCount
	}
	usage.TotalTokens = usage.PromptTokens
	return vecs, usage, nil
}
