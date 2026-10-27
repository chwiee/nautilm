package gateway

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"

	"github.com/aws/smithy-go"
)

const (
	arnOK     = "arn:aws:secretsmanager:us-east-2:111122223333:secret:nautilm/providers/remote-AbC123"
	arnDenied = "arn:aws:secretsmanager:us-east-2:111122223333:secret:nautilm/providers/denied-AbC123"
	arnKMS    = "arn:aws:secretsmanager:us-east-2:111122223333:secret:nautilm/providers/kms-AbC123"
	arnShape  = "arn:aws:secretsmanager:us-east-2:111122223333:secret:nautilm/providers/shape-AbC123"
	arnWrong  = "arn:aws:secretsmanager:us-east-2:111122223333:secret:nautilm/providers/wrong-AbC123"
	arnOther  = "arn:aws:secretsmanager:us-east-2:111122223333:secret:team-x/llm-key-AbC123"
)

// upstream is an OpenAI-compatible provider that only accepts "good-key".
// Model "broken" fails with 500 (to test that a bad alias is not saved).
func upstream(t *testing.T) *httptest.Server {
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer good-key" {
			http.Error(w, `{"error":{"message":"invalid api key"}}`, http.StatusUnauthorized)
			return
		}
		switch r.URL.Path {
		case "/v1/models":
			io.WriteString(w, `{"object":"list","data":[{"id":"m1"},{"id":"m2"}]}`)
		case "/v1/chat/completions":
			var req struct {
				Model    string
				Messages []any
			}
			json.NewDecoder(r.Body).Decode(&req)
			if len(req.Messages) == 0 { // what real servers do (Ollama, OpenAI)
				http.Error(w, `{"error":{"message":"[] is too short - 'messages'"}}`, http.StatusBadRequest)
				return
			}
			if req.Model == "broken" {
				http.Error(w, `{"error":{"message":"model is down"}}`, http.StatusInternalServerError)
				return
			}
			io.WriteString(w, `{"id":"x","object":"chat.completion","model":"`+req.Model+`","choices":[{"index":0,"message":{"role":"assistant","content":"OK"},"finish_reason":"stop"}],"usage":{"prompt_tokens":12,"completion_tokens":1,"total_tokens":13}}`)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(s.Close)
	return s
}

func revOf(t *testing.T, e adminEnv, c *http.Client, kind string) float64 {
	t.Helper()
	_, v := send(t, c, "GET", e.srv.URL+"/v1/admin/config", "", nil)
	revs, _ := v["revisions"].(map[string]any)
	n, _ := revs[kind].(float64)
	return n
}

func lastStep(out map[string]any) map[string]any {
	test, _ := out["test"].(map[string]any)
	steps, _ := test["steps"].([]any)
	if len(steps) == 0 {
		return nil
	}
	return steps[len(steps)-1].(map[string]any)
}

// addRemote saves the provider "remote" through the wizard.
func addRemote(t *testing.T, e adminEnv, admin *http.Client, up *httptest.Server) {
	t.Helper()
	e.secrets[arnOK] = `{"api_key":"good-key","base_url":"` + up.URL + `/v1"}`
	resp, out := send(t, admin, "POST", e.srv.URL+"/v1/admin/config/providers", e.srv.URL,
		map[string]any{"name": "remote", "def": map[string]any{"type": "openai", "secret_arn": arnOK}, "parent": revOf(t, e, admin, "providers")})
	if resp.StatusCode != http.StatusCreated || out["saved"] != true {
		t.Fatalf("add remote: %d %v", resp.StatusCode, out)
	}
}

func TestConfigProviderWizard(t *testing.T) {
	e := setupAdmin(t)
	up := upstream(t)
	admin, viewer := browser(t, e, "ana"), browser(t, e, "bruno")
	origin := e.srv.URL

	if resp, _ := send(t, viewer, "GET", e.srv.URL+"/v1/admin/config", "", nil); resp.StatusCode != http.StatusForbidden {
		t.Errorf("viewer must not see the Config tab: %d", resp.StatusCode)
	}

	// Step 2: the ARN is checked locally and the IAM statements are built.
	_, info := send(t, admin, "POST", e.srv.URL+"/v1/admin/config/arn", origin, map[string]string{"arn": arnOK})
	if info["valid"] != true || info["matches_prefix"] != true || info["partial"] != false ||
		!strings.Contains(info["policy_prefix"].(string), "secret:nautilm/providers/*") || info["region"] != "us-east-2" {
		t.Errorf("arn info: %v", info)
	}
	_, info = send(t, admin, "POST", e.srv.URL+"/v1/admin/config/arn", origin, map[string]string{"arn": arnOther})
	if info["matches_prefix"] != false || !strings.Contains(info["policy_exact"].(string), arnOther) {
		t.Errorf("arn outside the prefix: %v", info)
	}
	if _, info = send(t, admin, "POST", e.srv.URL+"/v1/admin/config/arn", origin, map[string]string{"arn": "nautilm/x"}); info["valid"] != false {
		t.Errorf("not an ARN: %v", info)
	}

	add := func(name, arn string) (int, map[string]any) {
		resp, out := send(t, admin, "POST", e.srv.URL+"/v1/admin/config/providers", origin,
			map[string]any{"name": name, "def": map[string]any{"type": "openai", "secret_arn": arn}, "parent": revOf(t, e, admin, "providers")})
		return resp.StatusCode, out
	}

	// The role cannot read the secret: the policy to add comes back.
	e.secrets[arnDenied] = &smithy.GenericAPIError{Code: "AccessDeniedException",
		Message: "User: arn:aws:sts::111122223333:assumed-role/nautilm is not authorized to perform: secretsmanager:GetSecretValue"}
	code, out := add("denied", arnDenied)
	s := lastStep(out)
	if code != http.StatusUnprocessableEntity || out["saved"] != false || s["name"] != "read_secret" ||
		!strings.Contains(s["policy"].(string), "secretsmanager:GetSecretValue") || !strings.Contains(s["hint"].(string), "IAM role") {
		t.Errorf("access denied: %d %v", code, out)
	}
	// A customer managed KMS key without kms:Decrypt.
	e.secrets[arnKMS] = &smithy.GenericAPIError{Code: "AccessDeniedException", Message: "Access to KMS is not allowed"}
	if _, out = add("kms", arnKMS); !strings.Contains(lastStep(out)["hint"].(string), "kms:Decrypt") {
		t.Errorf("kms: %v", out)
	}
	// A secret not found.
	if _, out = add("ghost", "arn:aws:secretsmanager:us-east-2:111122223333:secret:nautilm/providers/ghost-AbC123"); !strings.Contains(lastStep(out)["hint"].(string), "not found") {
		t.Errorf("not found: %v", out)
	}
	// Wrong shape: names the keys found, never the values.
	e.secrets[arnShape] = `{"token":"super-secret-value"}`
	_, out = add("shape", arnShape)
	if s = lastStep(out); s["name"] != "format" || !strings.Contains(s["detail"].(string), "token") ||
		strings.Contains(s["detail"].(string), "super-secret-value") {
		t.Errorf("format: %v", out)
	}
	// The secret is read but the provider refuses the key.
	e.secrets[arnWrong] = `{"api_key":"nope","base_url":"` + up.URL + `/v1"}`
	_, out = add("wrong", arnWrong)
	if s = lastStep(out); s["name"] != "provider" || !strings.Contains(s["hint"].(string), "rejected the key") {
		t.Errorf("wrong key: %v", out)
	}
	// A name of the config file is read-only.
	if _, out = add("a", arnOK); lastStep(out)["name"] != "definition" {
		t.Errorf("config provider name: %v", out)
	}
	if rev := revOf(t, e, admin, "providers"); rev != 0 {
		t.Fatalf("failed tests must save nothing, revision %v", rev)
	}

	// Everything right: tested and saved, with no alias yet.
	addRemote(t, e, admin, up)
	if code, _ := add("remote", arnOK); code != http.StatusConflict {
		t.Errorf("same name twice: %d", code)
	}
	_, view := send(t, admin, "GET", e.srv.URL+"/v1/admin/config", "", nil)
	var found map[string]any
	for _, p := range view["providers"].([]any) {
		if p.(map[string]any)["name"] == "remote" {
			found = p.(map[string]any)
		}
	}
	if found == nil || found["source"] != "dashboard" || found["base_url"] != "(from the secret)" || len(found["used_by"].([]any)) != 0 {
		t.Errorf("view: %v", found)
	}
	_, audit := send(t, admin, "GET", e.srv.URL+"/v1/admin/audit", "", nil)
	if !strings.Contains(toJSON(audit), `"config.providers"`) || !strings.Contains(toJSON(audit), `"config.provider.test"`) {
		t.Errorf("audit: %s", toJSON(audit))
	}
	if strings.Contains(toJSON(audit), "good-key") {
		t.Error("the API key must never reach the audit log")
	}
}

func TestConfigAliasTestSaveAndRollback(t *testing.T) {
	e := setupAdmin(t)
	up := upstream(t)
	admin := browser(t, e, "ana")
	origin := e.srv.URL
	addRemote(t, e, admin, up)

	served := func() string {
		resp := post(t, e.srv, "tok-mid", `{"model":"fast","messages":[{"role":"user","content":"hi"}]}`)
		resp.Body.Close()
		if resp.StatusCode != 200 {
			t.Fatalf("mid calling fast: %d", resp.StatusCode)
		}
		return resp.Header.Get("X-Nautilm-Provider") + "/" + resp.Header.Get("X-Nautilm-Model")
	}
	if got := served(); got != "b/small-model" {
		t.Fatalf("before: %s", got)
	}
	saveModel := func(alias string, def map[string]any) (int, map[string]any) {
		resp, out := send(t, admin, "POST", e.srv.URL+"/v1/admin/config/models", origin,
			map[string]any{"alias": alias, "def": def, "parent": revOf(t, e, admin, "models")})
		return resp.StatusCode, out
	}

	// Move "fast" to the new provider: tested with a real call, then saved.
	code, out := saveModel("fast", map[string]any{"provider": "remote", "model": "m1"})
	test, _ := out["test"].(map[string]any)
	if code != http.StatusCreated || test["ok"] != true || test["reply"] != "OK" {
		t.Fatalf("save fast: %d %v", code, out)
	}
	rev1 := revOf(t, e, admin, "models")
	if got := served(); got != "remote/m1" {
		t.Errorf("after the change mid must be served by remote: %s", got)
	}

	// A model that does not answer is never saved.
	code, out = saveModel("fast", map[string]any{"provider": "remote", "model": "broken"})
	if code != http.StatusUnprocessableEntity || out["saved"] != false {
		t.Errorf("broken model: %d %v", code, out)
	}
	if got := served(); got != "remote/m1" || revOf(t, e, admin, "models") != rev1 {
		t.Errorf("a failed test must not change anything: %s", got)
	}
	// Invalid config (fallback to an alias that does not exist).
	if code, out = saveModel("x2", map[string]any{"provider": "remote", "model": "m1", "fallback": []string{"nope"}}); code != http.StatusBadRequest ||
		!strings.Contains(toJSON(out), "fallback") {
		t.Errorf("invalid: %d %v", code, out)
	}
	// Two admins at once: the one with the stale view gets 409.
	resp, _ := send(t, admin, "POST", e.srv.URL+"/v1/admin/config/models", origin,
		map[string]any{"alias": "extra", "def": map[string]any{"provider": "remote", "model": "m2"}, "parent": 0})
	if resp.StatusCode != http.StatusConflict {
		t.Errorf("stale parent: %d", resp.StatusCode)
	}
	// The provider cannot be deleted while an alias uses it.
	if resp, _ = send(t, admin, "POST", e.srv.URL+"/v1/admin/config/providers/remote/delete", origin,
		map[string]any{"parent": revOf(t, e, admin, "providers")}); resp.StatusCode != http.StatusConflict {
		t.Errorf("delete provider in use: %d", resp.StatusCode)
	}

	// Removing the override restores the alias of the config file.
	if resp, _ = send(t, admin, "POST", e.srv.URL+"/v1/admin/config/models/fast/delete", origin,
		map[string]any{"parent": rev1}); resp.StatusCode != 200 {
		t.Fatalf("delete override: %d", resp.StatusCode)
	}
	if got := served(); got != "b/small-model" {
		t.Errorf("restored: %s", got)
	}
	// Rollback to revision 1 (a NEW revision, history kept).
	_, hist := send(t, admin, "GET", e.srv.URL+"/v1/admin/config/history?kind=models", "", nil)
	if n := len(hist["revisions"].([]any)); n != 2 {
		t.Fatalf("history: %d revisions", n)
	}
	if resp, _ = send(t, admin, "POST", e.srv.URL+"/v1/admin/config/rollback", origin,
		map[string]any{"revision_id": rev1, "parent": revOf(t, e, admin, "models")}); resp.StatusCode != 200 {
		t.Fatalf("rollback: %d", resp.StatusCode)
	}
	if got := served(); got != "remote/m1" {
		t.Errorf("after rollback: %s", got)
	}

	// The test calls are in the ledger, billed to nautilm-config-test.
	r := do(t, "GET", e.srv.URL+"/v1/usage?group_by=agent", "dash-secret", "")
	b, _ := io.ReadAll(r.Body)
	r.Body.Close()
	if !strings.Contains(string(b), TestAgent) {
		t.Errorf("test calls must be billed to %s: %s", TestAgent, b)
	}
}

func TestConfigPricingAndGroups(t *testing.T) {
	e := setupAdmin(t)
	admin := browser(t, e, "ana")
	origin := e.srv.URL

	missing := func() []any {
		_, v := send(t, admin, "GET", e.srv.URL+"/v1/admin/config", "", nil)
		m, _ := v["missing_prices"].([]any)
		return m
	}
	before := len(missing())
	if before == 0 {
		t.Fatal("the test config has an unpriced model (cheap)")
	}
	if resp, _ := send(t, admin, "POST", e.srv.URL+"/v1/admin/config/pricing", origin,
		map[string]any{"entries": []map[string]any{{"provider": "b", "model": "mystery-model", "input": -1}}}); resp.StatusCode != http.StatusBadRequest {
		t.Errorf("negative price: %d", resp.StatusCode)
	}
	if resp, out := send(t, admin, "POST", e.srv.URL+"/v1/admin/config/pricing", origin,
		map[string]any{"entries": []map[string]any{{"provider": "b", "model": "mystery-model", "input": 1, "output": 2, "effective_from": "2026-01-01"}}}); resp.StatusCode != 200 {
		t.Fatalf("pricing: %d %v", resp.StatusCode, out)
	}
	if after := len(missing()); after != before-1 {
		t.Errorf("missing prices %d → %d", before, after)
	}

	// Carla's group is not mapped: denied.
	carla := browser(t, e, "carla")
	if _, me := send(t, carla, "GET", e.srv.URL+"/auth/me", "", nil); me["user"] != nil {
		t.Fatal("carla must be denied before the mapping")
	}
	if resp, _ := send(t, admin, "POST", e.srv.URL+"/v1/admin/config/groups", origin,
		map[string]any{"viewer": []map[string]string{{"id": "unrelated", "name": "Ops team"}}}); resp.StatusCode != 200 {
		t.Fatalf("groups: %d", resp.StatusCode)
	}
	carla = browser(t, e, "carla")
	_, me := send(t, carla, "GET", e.srv.URL+"/auth/me", "", nil)
	u, _ := me["user"].(map[string]any)
	if u == nil || toJSON(u["roles"]) != `["viewer"]` {
		t.Fatalf("carla after the mapping: %v", me)
	}
	if resp, _ := send(t, carla, "GET", e.srv.URL+"/v1/usage", "", nil); resp.StatusCode != 200 {
		t.Errorf("carla reads usage: %d", resp.StatusCode)
	}
	// Removing the group cuts her EXISTING session at once.
	if resp, _ := send(t, admin, "POST", e.srv.URL+"/v1/admin/config/groups", origin,
		map[string]any{"viewer": []any{}, "parent": revOf(t, e, admin, "groups")}); resp.StatusCode != 200 {
		t.Fatal("remove group")
	}
	if resp, _ := send(t, carla, "GET", e.srv.URL+"/v1/usage", "", nil); resp.StatusCode == 200 {
		t.Error("carla's session must lose access when her group is removed")
	}
	// The groups of the config file always stay (no lock-out).
	_, view := send(t, admin, "GET", e.srv.URL+"/v1/admin/config", "", nil)
	groups := view["groups"].(map[string]any)
	if !slices.ContainsFunc(groups["admin"].([]any), func(g any) bool { return g.(map[string]any)["id"] == "grp-admin" }) {
		t.Errorf("config admin group must remain: %v", groups)
	}
	if resp, _ := send(t, admin, "GET", e.srv.URL+"/v1/admin/tokens", "", nil); resp.StatusCode != 200 {
		t.Error("ana is still admin")
	}
}

func toJSON(v any) string {
	b, _ := json.Marshal(v)
	return string(b)
}
