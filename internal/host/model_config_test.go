package host

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/voocel/ainovel-cli/internal/bootstrap"
)

func newModelConfigTestHost(t *testing.T) (*Host, string) {
	t.Helper()
	pc := bootstrap.ProviderConfig{
		Type: "openai", APIKey: "old-secret", BaseURL: "https://example.com/v1",
		Models: []bootstrap.ModelConfig{{Name: "old", ContextWindow: 128000}, {Name: "writer-model"}},
	}
	cfg := bootstrap.Config{
		Provider: "proxy", ModelName: "old", Providers: map[string]bootstrap.ProviderConfig{"proxy": pc},
		Roles: map[string]bootstrap.RoleConfig{"writer": {Provider: "proxy", Model: "writer-model"}},
	}
	models, err := bootstrap.NewModelSet(cfg)
	if err != nil {
		t.Fatalf("new model set: %v", err)
	}
	// 落一份初始配置：生产中 configPath 必指向已存在的配置层，SaveProviderConfig
	// 只补 providers 段、保留其余，seed 后才能真实检验“顶层选择不被改动”。
	path := filepath.Join(t.TempDir(), "config.json")
	if err := bootstrap.SaveConfig(path, cfg); err != nil {
		t.Fatalf("seed config: %v", err)
	}
	return &Host{
		cfg: cfg, models: models, events: make(chan Event, 4),
		configPath: path,
	}, path
}

// 推理强度存储保留原始意图：显式设定后，切模型不得把它钳制降级写回。
func TestSetRoleThinkingPreservesIntentAcrossModelSwitch(t *testing.T) {
	h, _ := newModelConfigTestHost(t)
	if err := h.SetRoleThinking("writer", "high"); err != nil {
		t.Fatalf("set thinking: %v", err)
	}
	if got := h.cfg.Roles["writer"].ReasoningEffort; got != "high" {
		t.Fatalf("SetRoleThinking 应原样存 high，得到 %q", got)
	}
	// 换 writer 的模型：已存的强度意图必须保持 high，钳制只应发生在下发路径。
	if err := h.SwitchModel("writer", "proxy", "old"); err != nil {
		t.Fatalf("switch: %v", err)
	}
	if got := h.cfg.Roles["writer"].ReasoningEffort; got != "high" {
		t.Fatalf("切模型后 writer thinking 被改写为 %q，应仍是 high", got)
	}
}

func TestConfigureModelsRejectsDeletingReferencedModel(t *testing.T) {
	h, _ := newModelConfigTestHost(t)
	// 删掉被 writer 角色引用的 "writer-model"（保留顶层在用的 "old"）应被拒。
	err := h.ConfigureModels(ModelConfigurationDraft{
		Provider: "proxy", Type: "openai", BaseURL: "https://example.com/v1",
		Models:       []bootstrap.ModelConfig{{Name: "old"}, {Name: "new"}},
		APIKeyAction: APIKeyKeep,
	})
	if err == nil || !strings.Contains(err.Error(), "writer") {
		t.Fatalf("expected writer reference error, got %v", err)
	}
	provider, model, _ := h.models.CurrentSelection("default")
	if provider != "proxy" || model != "old" {
		t.Fatalf("runtime mutated after failure: %s/%s", provider, model)
	}
}

// /config 不再代切默认：删掉顶层正在用的模型必须被拒，让用户先去 /model 切走。
func TestConfigureModelsRejectsDeletingCurrentModel(t *testing.T) {
	h, _ := newModelConfigTestHost(t)
	err := h.ConfigureModels(ModelConfigurationDraft{
		Provider: "proxy", Type: "openai", BaseURL: "https://example.com/v1",
		Models:       []bootstrap.ModelConfig{{Name: "writer-model"}, {Name: "new"}},
		APIKeyAction: APIKeyKeep,
	})
	if err == nil || !strings.Contains(err.Error(), "default") {
		t.Fatalf("expected default reference error, got %v", err)
	}
}

func TestConfigureModelsPersistsAndHotApplies(t *testing.T) {
	h, path := newModelConfigTestHost(t)
	err := h.ConfigureModels(ModelConfigurationDraft{
		Provider: "proxy", Type: "openai", API: "responses", BaseURL: "https://new.example/v1",
		Models:       []bootstrap.ModelConfig{{Name: "old", ContextWindow: 640000}, {Name: "writer-model"}},
		APIKeyAction: APIKeyKeep,
	})
	if err != nil {
		t.Fatalf("configure: %v", err)
	}
	// 顶层选择不被 /config 改动：仍是 proxy/old。
	provider, model, _ := h.models.CurrentSelection("default")
	if provider != "proxy" || model != "old" {
		t.Fatalf("runtime selection mutated = %s/%s", provider, model)
	}
	// provider 段热应用：old 的窗口更新为 640000。
	if window, source := h.models.ResolveContextWindow("proxy", "old"); window != 640000 || source != bootstrap.CtxWindowModelConfig {
		t.Fatalf("runtime window = %d %s", window, source)
	}
	saved, err := bootstrap.LoadConfigFile(path)
	if err != nil {
		t.Fatalf("load saved: %v", err)
	}
	if saved.Provider != "proxy" || saved.ModelName != "old" || saved.Providers["proxy"].APIKey != "old-secret" {
		t.Fatalf("saved config = %#v", saved)
	}
	if saved.Providers["proxy"].API != "responses" || saved.Providers["proxy"].BaseURL != "https://new.example/v1" {
		t.Fatalf("saved provider not patched = %#v", saved.Providers["proxy"])
	}
	if len(saved.Providers["proxy"].Models) != 2 || saved.Providers["proxy"].Models[0].ContextWindow != 640000 {
		t.Fatalf("saved models = %#v", saved.Providers["proxy"].Models)
	}
}
