package imp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/voocel/agentcore"
)

// import 专用 typed-call 内核（RFC §13）。小而专用，不建通用 LLM 工作流框架。
// 三类失败分离：请求层瞬时错误重试；输出层 JSON/校验失败带反馈重问；容量错误（长度截断）
// 不原样重试也不占用语义重试，交调用方决定失败或前缀打捞。
const (
	callMaxSemanticAttempts = 3
	callMaxRequestRetries   = 7
	callMaxRetryDelay       = 60 * time.Second
)

const callRetryHint = "上面的输出不是合法 JSON 或缺少必填字段。只输出一个符合约定 schema 的 JSON 对象，不要任何解释文字，不要 Markdown 代码围栏。"

// callModel 是内核对模型的最小依赖，便于测试注入 mock。
type callModel interface {
	Generate(ctx context.Context, messages []agentcore.Message, tools []agentcore.ToolSpec, opts ...agentcore.CallOption) (*agentcore.LLMResponse, error)
}

// errTruncated 表示模型因长度停止（容量错误）。携带原始文本供调用方决定失败或前缀打捞（§9.5）。
type errTruncated struct {
	Raw string
}

func (e *errTruncated) Error() string { return "模型输出被长度截断（stop=length）" }

// errSemantic 表示输出层语义失败（JSON/校验多次仍非法），携带最后一次原始响应，
// 供 runner 统一落 failures/ 失败工件（§14.2），所有语义函数共用。
type errSemantic struct {
	Raw string
	Err error
}

func (e *errSemantic) Error() string { return e.Err.Error() }
func (e *errSemantic) Unwrap() error { return e.Err }

func assistantMsg(text string) agentcore.Message {
	return agentcore.Message{Role: agentcore.RoleAssistant, Content: []agentcore.ContentBlock{agentcore.TextBlock(text)}}
}

// callProfile 承载一次结构化调用的能力相关选项（thinking 与结构化输出模式），
// 由 Host 探测的 ModelRuntime 派生；零值表示不发 thinking、走 prompt-only（与无能力信息时等价）。
//
// TODO(json-schema)：RFC §13.2 第 1 级——provider 明确支持 JSON Schema 时发送对应 schema 约束输出。
// 计划与仓库其它模型调用点（arbiter 裁定、engine 语义工具等）统一改造，届时在此增加 schema 位并在
// callOptions 组装；改造前统一走 JSON Object / prompt 契约 + extract/validate 兜底，行为正确只损失约束强度。
type callProfile struct {
	thinking   agentcore.ThinkingLevel
	jsonObject bool
}

// callOptions 组装本次调用的 CallOption：始终带输出上限；按能力可选 thinking 与 JSON Object 约束。
// thinking 仅在非 Auto 时发送——对不支持 thinking 的模型发任何等级（含 off）都是非法参数（与 arbiter 同策略）。
func (p callProfile) callOptions(maxTokens int) []agentcore.CallOption {
	opts := []agentcore.CallOption{agentcore.WithMaxTokens(maxTokens)}
	if p.thinking != agentcore.ThinkingAuto {
		opts = append(opts, agentcore.WithThinking(p.thinking))
	}
	if p.jsonObject {
		opts = append(opts, agentcore.WithJSONMode())
	}
	return opts
}

// callStructured 发起一次结构化调用：system + payload → 解析进 T → validate。
// 校验失败带反馈重问（最多 callMaxSemanticAttempts）；长度截断返回 *errTruncated。
// prof 决定是否发 thinking / JSON Object 约束；无论 provider 是否约束输出，都走同一套 extractJSONObject + validate 兜底。
func callStructured[T any](ctx context.Context, m callModel, systemPrompt, payload string, maxTokens int, prof callProfile, validate func(*T) error) (T, error) {
	var zero T
	if m == nil {
		return zero, fmt.Errorf("imp: model 未配置")
	}
	messages := []agentcore.Message{
		agentcore.SystemMsg(systemPrompt),
		agentcore.UserMsg(payload),
	}
	opts := prof.callOptions(maxTokens)
	var lastErr error
	var lastRaw string
	for attempt := 1; attempt <= callMaxSemanticAttempts; attempt++ {
		resp, err := generateWithRetry(ctx, m, messages, opts...)
		if err != nil {
			return zero, fmt.Errorf("imp: 模型调用失败：%w", err)
		}
		if resp == nil {
			lastErr = fmt.Errorf("模型返回空响应")
			break
		}
		raw := resp.Message.TextContent()
		lastRaw = raw
		if resp.Message.StopReason == agentcore.StopReasonLength {
			return zero, &errTruncated{Raw: raw}
		}
		out, verr := parseStructured[T](raw, validate)
		if verr == nil {
			return *out, nil
		}
		lastErr = verr
		messages = append(messages, assistantMsg(raw),
			agentcore.UserMsg(callRetryHint+"\n错误："+verr.Error()))
		if ctx.Err() != nil {
			break
		}
		slog.Warn("imp 结构化输出重试", "attempt", attempt, "err", verr)
	}
	return zero, &errSemantic{Raw: lastRaw,
		Err: fmt.Errorf("imp: 结构化输出失败（%d 次尝试）：%w", callMaxSemanticAttempts, lastErr)}
}

// parseStructured 从原始文本截取 JSON 对象、解析进 T 并 validate。
func parseStructured[T any](raw string, validate func(*T) error) (*T, error) {
	s := extractJSONObject(raw)
	if s == "" {
		return nil, fmt.Errorf("输出中未找到 JSON 对象")
	}
	var out T
	if err := json.Unmarshal([]byte(s), &out); err != nil {
		return nil, fmt.Errorf("解析 JSON：%w", err)
	}
	if validate != nil {
		if err := validate(&out); err != nil {
			return nil, err
		}
	}
	return &out, nil
}

// generateWithRetry 只重试适配器标记 retryable 的瞬时错误，遵守 Retry-After/指数退避。
// 鉴权/权限/模型不支持等终止错误立即返回（§13.3）。
func generateWithRetry(ctx context.Context, m callModel, messages []agentcore.Message, opts ...agentcore.CallOption) (*agentcore.LLMResponse, error) {
	var lastErr error
	for attempt := 0; attempt <= callMaxRequestRetries; attempt++ {
		resp, err := m.Generate(ctx, messages, nil, opts...)
		if err == nil {
			return resp, nil
		}
		lastErr = err
		if errors.Is(err, context.Canceled) || !isRetryable(err) || attempt == callMaxRequestRetries {
			return nil, err
		}
		delay := retryDelay(err, attempt)
		timer := time.NewTimer(delay)
		select {
		case <-ctx.Done():
			timer.Stop()
			return nil, ctx.Err()
		case <-timer.C:
		}
	}
	return nil, lastErr
}

func isRetryable(err error) bool {
	var retryable agentcore.RetryableError
	return errors.As(err, &retryable) && retryable.Retryable()
}

func retryDelay(err error, attempt int) time.Duration {
	var hinter agentcore.RetryHinter
	if errors.As(err, &hinter) {
		if d := hinter.RetryAfter(); d > 0 {
			if d > callMaxRetryDelay {
				return callMaxRetryDelay
			}
			return d
		}
	}
	d := time.Duration(1<<attempt) * time.Second
	if d > callMaxRetryDelay {
		return callMaxRetryDelay
	}
	return d
}

// extractJSONObject 截取首个平衡的 JSON 对象（容忍围栏与前后缀文本）。
func extractJSONObject(raw string) string {
	start := strings.IndexByte(raw, '{')
	if start < 0 {
		return ""
	}
	depth, inStr, escape := 0, false, false
	for i := start; i < len(raw); i++ {
		c := raw[i]
		if inStr {
			switch {
			case escape:
				escape = false
			case c == '\\':
				escape = true
			case c == '"':
				inStr = false
			}
			continue
		}
		switch c {
		case '"':
			inStr = true
		case '{':
			depth++
		case '}':
			depth--
			if depth == 0 {
				return raw[start : i+1]
			}
		}
	}
	return ""
}
