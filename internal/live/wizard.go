package live

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/aws/smithy-go"

	"github.com/chwiee/nautilm/internal/config"
	"github.com/chwiee/nautilm/internal/provider"
	"github.com/chwiee/nautilm/pkg/openai"
)

// DefaultSecretPrefix: if every provider secret lives under this name, the
// nautilm role needs ONE policy statement for all of them (written once).
const DefaultSecretPrefix = "nautilm/providers/"

// ProviderName: short, lowercase (it shows in logs, metrics and the ledger).
var ProviderName = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{1,39}$`)

var (
	secretARN   = regexp.MustCompile(`^arn:(aws|aws-cn|aws-us-gov):secretsmanager:([a-z0-9-]+):(\d{12}):secret:(.+)$`)
	arnSuffix   = regexp.MustCompile(`-[A-Za-z0-9]{6}$`)
	awsRegion   = regexp.MustCompile(`^[a-z]{2}(-[a-z]+)+-\d$`)
	secretShape = `{"api_key": "<the provider API key>", "base_url": "<optional: https://.../v1>"}`
)

// ParseKey reads a provider secret: JSON {"api_key", "base_url"} or the key
// as plain text. Errors name the keys found, NEVER the values.
func ParseKey(v string) (Key, error) {
	v = strings.TrimSpace(v)
	if v == "" {
		return Key{}, errors.New("the secret is empty")
	}
	if !strings.HasPrefix(v, "{") {
		return Key{APIKey: v}, nil
	}
	var m map[string]any
	if err := json.Unmarshal([]byte(v), &m); err != nil {
		return Key{}, errors.New("the secret starts with { but is not valid JSON")
	}
	k := Key{}
	k.APIKey, _ = m["api_key"].(string)
	k.BaseURL, _ = m["base_url"].(string)
	if k.APIKey == "" {
		keys := make([]string, 0, len(m))
		for name := range m {
			keys = append(keys, name)
		}
		slices.Sort(keys)
		return Key{}, fmt.Errorf("the secret has no \"api_key\" (keys found: %s); expected %s", strings.Join(keys, ", "), secretShape)
	}
	return k, nil
}

// ARNInfo is what the wizard shows after the ARN is typed (no AWS call).
type ARNInfo struct {
	Valid         bool   `json:"valid"`
	Error         string `json:"error,omitempty"`
	Region        string `json:"region,omitempty"`
	Account       string `json:"account,omitempty"`
	Name          string `json:"name,omitempty"`
	Partial       bool   `json:"partial"` // no -XXXXXX suffix: copy the full ARN from the console
	Prefix        string `json:"prefix"`
	MatchesPrefix bool   `json:"matches_prefix"`
	// PolicyPrefix covers every secret under the prefix (apply once);
	// PolicyExact covers only this secret.
	PolicyPrefix string `json:"policy_prefix,omitempty"`
	PolicyExact  string `json:"policy_exact,omitempty"`
	KMSNote      string `json:"kms_note"`
}

// InspectARN validates the ARN and builds the IAM statements to show.
func InspectARN(arn, prefix string) ARNInfo {
	if prefix == "" {
		prefix = DefaultSecretPrefix
	}
	info := ARNInfo{Prefix: prefix, KMSNote: "If the secret uses a customer managed KMS key, also allow kms:Decrypt on that key. " +
		"With the default key (aws/secretsmanager) nothing else is needed."}
	m := secretARN.FindStringSubmatch(strings.TrimSpace(arn))
	if m == nil {
		info.Error = "not a Secrets Manager ARN (arn:aws:secretsmanager:<region>:<account>:secret:<name>-XXXXXX)"
		return info
	}
	info.Valid, info.Region, info.Account, info.Name = true, m[2], m[3], m[4]
	info.Partial = !arnSuffix.MatchString(info.Name)
	base := strings.TrimSuffix(info.Name, arnSuffix.FindString(info.Name))
	info.MatchesPrefix = strings.HasPrefix(base, prefix)
	stmt := func(sid, res string) string {
		// A struct keeps the order people expect in IAM (a map would sort the keys).
		b, _ := json.MarshalIndent(struct{ Sid, Effect, Action, Resource string }{sid, "Allow", "secretsmanager:GetSecretValue", res}, "", "  ")
		return string(b)
	}
	info.PolicyPrefix = stmt("ReadNautilmProviderSecrets", fmt.Sprintf("arn:%s:secretsmanager:%s:%s:secret:%s*", m[1], m[2], m[3], prefix))
	info.PolicyExact = stmt("ReadThisProviderSecret", strings.TrimSpace(arn))
	return info
}

// Step is one line of the wizard's test result.
type Step struct {
	Name   string `json:"name"` // definition, arn, read_secret, format, provider
	OK     bool   `json:"ok"`
	Detail string `json:"detail,omitempty"`
	Hint   string `json:"hint,omitempty"`
	Policy string `json:"policy,omitempty"`
}

// Explain turns a Secrets Manager error into what the admin must do.
func Explain(err error, info ARNInfo) (detail, hint, policy string) {
	detail = err.Error()
	var api smithy.APIError
	code, msg := "", ""
	if errors.As(err, &api) {
		code, msg = api.ErrorCode(), api.ErrorMessage()
		detail = code + ": " + msg
	}
	policy = info.PolicyExact
	if info.MatchesPrefix {
		policy = info.PolicyPrefix
	}
	switch {
	case code == "AccessDeniedException" && strings.Contains(strings.ToLower(msg), "kms"):
		return detail, "The secret uses a KMS key the nautilm role cannot use: allow kms:Decrypt on that key (key policy or IAM).", ""
	case code == "AccessDeniedException":
		return detail, "The nautilm IAM role cannot read this secret. Add this statement to the role (or the prefix one, once for every provider).", policy
	case code == "ResourceNotFoundException":
		return detail, "Secret not found: check account, Region and the 6-character suffix (copy the full ARN from the console).", ""
	case code == "DecryptionFailure":
		return detail, "Secrets Manager could not decrypt the secret with its KMS key (key disabled or no kms:Decrypt).", ""
	case code == "InvalidRequestException":
		return detail, "The secret is scheduled for deletion or not usable: restore it or use another one.", ""
	case code == "UnrecognizedClientException" || code == "ExpiredTokenException" ||
		strings.Contains(detail, "credentials") || strings.Contains(detail, "IMDS"):
		return detail, "nautilm has no valid AWS credentials (Pod Identity association missing or expired).", ""
	}
	return detail, "", ""
}

// TestInput is what the wizard sends.
type TestInput struct {
	Name      string      `json:"name"`
	Def       ProviderDef `json:"def"`
	TestModel string      `json:"test_model"` // bedrock: a model id for a 1-token call
}

// TestResult is the wizard's answer.
type TestResult struct {
	OK     bool          `json:"ok"`
	Steps  []Step        `json:"steps"`
	Models []string      `json:"models,omitempty"` // models the provider lists (openai)
	Usage  *openai.Usage `json:"-"`                // bedrock test call (billed by the gateway)
}

// TestProvider runs the wizard checks in order and stops at the first failure.
func (m *Manager) TestProvider(ctx context.Context, in TestInput, prefix string) TestResult {
	var res TestResult
	add := func(s Step) bool { res.Steps = append(res.Steps, s); return s.OK }
	d := in.Def

	// 1. Definition.
	switch {
	case !ProviderName.MatchString(in.Name):
		add(Step{Name: "definition", Detail: "name: 2 to 40 characters, lowercase letters, numbers and -"})
		return res
	case m.Base.Providers[in.Name] != (config.Provider{}):
		add(Step{Name: "definition", Detail: fmt.Sprintf("provider %q is defined in the config file; pick another name", in.Name)})
		return res
	case d.Type == "bedrock" && !awsRegion.MatchString(d.Region):
		add(Step{Name: "definition", Detail: "bedrock needs a Region (e.g. us-east-2)"})
		return res
	case d.Type == "bedrock" && in.TestModel == "":
		add(Step{Name: "definition", Detail: "bedrock needs a model id to test (e.g. amazon.nova-lite-v1:0)"})
		return res
	case d.Type == "openai" && d.SecretARN == "" && d.BaseURL == "":
		add(Step{Name: "definition", Detail: "openai needs base_url (here or inside the secret)"})
		return res
	case d.Type != "bedrock" && d.Type != "openai":
		add(Step{Name: "definition", Detail: "type must be bedrock or openai"})
		return res
	}
	add(Step{Name: "definition", OK: true})

	key := Key{}
	if d.Type == "openai" && d.SecretARN != "" {
		// 2. ARN.
		info := InspectARN(d.SecretARN, prefix)
		if !info.Valid {
			add(Step{Name: "arn", Detail: info.Error})
			return res
		}
		note := ""
		if info.Partial {
			note = "partial ARN (no -XXXXXX suffix): prefer the full ARN"
		}
		add(Step{Name: "arn", OK: true, Detail: note})
		// 3. Read the secret (always fresh: no cache in the wizard).
		if m.Secrets == nil {
			add(Step{Name: "read_secret", Detail: "no secret reader configured"})
			return res
		}
		v, err := m.Secrets.Read(ctx, d.SecretARN)
		if err != nil {
			detail, hint, policy := Explain(err, info)
			add(Step{Name: "read_secret", Detail: detail, Hint: hint, Policy: policy})
			return res
		}
		add(Step{Name: "read_secret", OK: true})
		// 4. Format.
		if key, err = ParseKey(v); err != nil {
			add(Step{Name: "format", Detail: err.Error(), Hint: "put a new value in the same secret (the ARN does not change) and test again"})
			return res
		}
		if d.BaseURL == "" && key.BaseURL == "" {
			add(Step{Name: "format", Detail: "no base_url: set it in the form or inside the secret"})
			return res
		}
		add(Step{Name: "format", OK: true})
	}

	// 5. The provider accepts the credentials.
	if d.Type == "openai" {
		base := d.BaseURL
		if base == "" {
			base = key.BaseURL
		}
		models, err := listModels(ctx, base, key.APIKey)
		if err != nil {
			add(Step{Name: "provider", Detail: err.Error(), Hint: providerHint(err)})
			return res
		}
		res.Models = models
		add(Step{Name: "provider", OK: true, Detail: fmt.Sprintf("%d models listed", len(models))})
	} else {
		p, err := m.Build(ctx, in.Name, config.Provider{Type: "bedrock", Region: d.Region})
		if err == nil {
			max := 5
			var resp *openai.ChatResponse
			resp, err = p.Chat(ctx, provider.Call{Model: in.TestModel, DefaultMaxTokens: 5,
				Request: &openai.ChatRequest{Model: in.TestModel, MaxTokens: &max, Messages: []openai.Message{{Role: "user", Content: openai.Text("Reply with OK.")}}}})
			if err == nil {
				res.Usage = resp.Usage
			}
		}
		if err != nil {
			add(Step{Name: "provider", Detail: err.Error(), Hint: "check the nautilm role has bedrock:InvokeModel on this model ARN, the model is enabled and has quota in the Region"})
			return res
		}
		add(Step{Name: "provider", OK: true, Detail: "1-token call to " + in.TestModel + " answered"})
	}
	res.OK = true
	return res
}

type httpStatusError struct {
	status int
	body   string
}

func (e *httpStatusError) Error() string {
	return fmt.Sprintf("HTTP %d from the provider: %s", e.status, e.body)
}

func providerHint(err error) string {
	var he *httpStatusError
	if errors.As(err, &he) && (he.status == 401 || he.status == 403) {
		return "the secret was read, but the provider rejected the key: check api_key in the secret"
	}
	return "check base_url (it must end in /v1 for most OpenAI-compatible servers) and the network path from nautilm"
}

// listModels calls GET <base>/models: proves the URL and the key work
// without spending tokens.
func listModels(ctx context.Context, base, key string) ([]string, error) {
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimRight(base, "/")+"/models", nil)
	if err != nil {
		return nil, fmt.Errorf("base_url: %w", err)
	}
	if key != "" {
		req.Header.Set("Authorization", "Bearer "+key)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode != http.StatusOK {
		body := strings.TrimSpace(string(b))
		if len(body) > 200 {
			body = body[:200]
		}
		return nil, &httpStatusError{resp.StatusCode, body}
	}
	var list struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := json.Unmarshal(b, &list); err != nil {
		return nil, errors.New("GET /models did not return an OpenAI-style list: is base_url right?")
	}
	out := make([]string, 0, len(list.Data))
	for _, d := range list.Data {
		out = append(out, d.ID)
	}
	slices.Sort(out)
	return out, nil
}
