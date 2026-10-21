package bedrock

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/bedrockruntime"

	"github.com/chwiee/pedagio/internal/provider"
)

type fakeInvoker struct{ bodies []map[string]any }

func (f *fakeInvoker) InvokeModel(_ context.Context, in *bedrockruntime.InvokeModelInput, _ ...func(*bedrockruntime.Options)) (*bedrockruntime.InvokeModelOutput, error) {
	var b map[string]any
	json.Unmarshal(in.Body, &b)
	f.bodies = append(f.bodies, b)
	if aws.ToString(in.ModelId) != "amazon.titan-embed-text-v2:0" {
		panic("modelo errado")
	}
	return &bedrockruntime.InvokeModelOutput{Body: []byte(`{"embedding":[0.1,0.2,0.3],"inputTextTokenCount":4}`)}, nil
}

func TestTitanEmbeddings(t *testing.T) {
	inv := &fakeInvoker{}
	p := NewWithAPI(nil, nil).WithInvoker(inv)
	vecs, u, err := p.Embed(context.Background(), provider.EmbedCall{Model: "amazon.titan-embed-text-v2:0", Input: []string{"a", "b"}, Dimensions: 256})
	if err != nil {
		t.Fatal(err)
	}
	if len(vecs) != 2 || len(vecs[0]) != 3 || u.PromptTokens != 8 {
		t.Errorf("vecs=%v usage=%+v", vecs, u)
	}
	if inv.bodies[0]["inputText"] != "a" || inv.bodies[0]["dimensions"] != 256.0 || inv.bodies[0]["normalize"] != true {
		t.Errorf("corpo enviado ao Titan: %v", inv.bodies[0])
	}
}

func TestEmbeddingSoTitan(t *testing.T) {
	p := NewWithAPI(nil, nil).WithInvoker(&fakeInvoker{})
	if _, _, err := p.Embed(context.Background(), provider.EmbedCall{Model: "cohere.embed-v4", Input: []string{"a"}}); err == nil {
		t.Error("modelo não-Titan deveria dar erro claro")
	}
}
