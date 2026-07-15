// Package arbiter 是语义裁定层:按需唤醒的 LLM-as-function。
//
// 两平面对称(docs/engine-arbiter.md §二):
//
//	确定性平面:  flow.LoadState   → flow.Route     → Instruction
//	语义平面:    arbiter.Collect* → arbiter.Decide* → XxxDecision
//
// 纪律:Collect 集中 IO(从 store 读齐事实);Decide 除一次 LLM 调用外无 IO,
// 可用历史 facts 离线重放;执行归 Engine。每场景一对函数 + 专属 Decision 类型,
// 场景不匹配的动作在类型上不可表达;剩余合法性由各类型的 Validate 拒绝——
// Arbiter 输出与一切 LLM 输出同样不可信,事实校验是最后一道门。
package arbiter

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"strings"
	"time"

	"github.com/voocel/agentcore"
)

// decideMaxTokens 单次裁定的输出上限;裁定 JSON 很小,大头留给推理模型的思考预算
// (与 userrules.normalizeMaxTokens 同理)。
const decideMaxTokens = 8192

// decideMaxAttempts 总尝试次数:解析失败带反馈重问,最多 3 次后返回错误,
// 由调用方回显真实失败原因并保证不执行未裁定的动作。
const decideMaxAttempts = 3

// modelMaxRetries 与 Worker 的 subagentMaxRetries 保持一致。这里只处理请求层的
// 瞬时错误；JSON/字段校验失败仍由 decideMaxAttempts 独立限制，二者不能混为一类。
const modelMaxRetries = 7

const maxRetryDelay = 60 * time.Second

const retryHint = "上面的输出不是合法的 JSON 或缺少必填字段。请只输出一个符合约定 schema 的 JSON 对象，不要任何解释文字、不要 Markdown 代码围栏。"

// decide 是所有场景共用的 LLM 调用内核:system 提示词 + 用户负载 → 解析进 T →
// Validate;非法输出带反馈重问。除模型调用外无任何 IO。
func decide[T any](ctx context.Context, model agentcore.ChatModel, systemPrompt, payload string, validate func(*T) error) (T, error) {
	var zero T
	if model == nil {
		return zero, fmt.Errorf("arbiter: model 未配置")
	}

	messages := []agentcore.Message{
		{Role: agentcore.RoleSystem, Content: []agentcore.ContentBlock{agentcore.TextBlock(systemPrompt)}},
		{Role: agentcore.RoleUser, Content: []agentcore.ContentBlock{agentcore.TextBlock(payload)}},
	}

	var lastErr error
	for attempt := 1; attempt <= decideMaxAttempts; attempt++ {
		// 不覆盖 thinking：off 也是 provider/model 专属参数，不是普通 chat
		// 模型的通用 no-op。裁定沿用模型默认，只约束结构化输出预算。
		resp, err := generateWithRetry(ctx, model, messages,
			agentcore.WithMaxTokens(decideMaxTokens))
		if err != nil {
			return zero, fmt.Errorf("arbiter: 模型调用失败: %w", err)
		}
		var raw string
		switch {
		case resp == nil:
			lastErr = fmt.Errorf("模型返回空响应")
		default:
			raw = resp.Message.TextContent()
			var out T
			if s := extractJSON(raw); s != "" {
				if uerr := json.Unmarshal([]byte(s), &out); uerr == nil {
					if verr := validate(&out); verr == nil {
						return out, nil
					} else {
						lastErr = verr
					}
				} else {
					lastErr = uerr
				}
			} else {
				lastErr = fmt.Errorf("输出中未找到 JSON")
			}
			// 反馈式重试:把非法输出与纠正提示并入对话(仅格式/校验失败有反馈可给)。
			messages = append(messages,
				agentcore.Message{Role: agentcore.RoleAssistant, Content: []agentcore.ContentBlock{agentcore.TextBlock(raw)}},
				agentcore.Message{Role: agentcore.RoleUser, Content: []agentcore.ContentBlock{agentcore.TextBlock(retryHint + "\n错误：" + lastErr.Error())}},
			)
		}
		slog.Warn("裁定尝试失败", "module", "arbiter", "attempt", attempt, "err", lastErr)
		if ctx.Err() != nil {
			break
		}
	}
	return zero, fmt.Errorf("arbiter: 裁定失败（%d 次尝试）: %w", decideMaxAttempts, lastErr)
}

// generateWithRetry 为 Arbiter 接上与 Worker 相同的请求层重试语义：仅重试模型适配器
// 明确标记为 retryable 的错误，遵守 Retry-After/指数退避，并经 ToolProgress 把进度
// 送入既有工作台观察链。账户、鉴权、权限等终止错误会立即返回。
func generateWithRetry(ctx context.Context, model agentcore.ChatModel, messages []agentcore.Message, opts ...agentcore.CallOption) (*agentcore.LLMResponse, error) {
	var lastErr error
	for attempt := 0; attempt <= modelMaxRetries; attempt++ {
		resp, err := model.Generate(ctx, messages, nil, opts...)
		if err == nil {
			return resp, nil
		}
		lastErr = err
		if errors.Is(err, context.Canceled) || !isRetryable(err) || attempt == modelMaxRetries {
			return nil, err
		}

		delay := retryDelay(err, attempt)
		meta, _ := json.Marshal(struct {
			DelayMS int64 `json:"retry_delay_ms"`
		}{DelayMS: delay.Milliseconds()})
		agentcore.ReportToolProgress(ctx, agentcore.ProgressPayload{
			Kind:       agentcore.ProgressRetry,
			Agent:      "arbiter",
			Attempt:    attempt + 1,
			MaxRetries: modelMaxRetries,
			Message:    err.Error(),
			Meta:       meta,
		})
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
		if delay := hinter.RetryAfter(); delay > 0 {
			if delay > maxRetryDelay {
				return maxRetryDelay
			}
			return delay
		}
	}
	delay := time.Duration(math.Pow(2, float64(attempt))) * time.Second
	if delay > maxRetryDelay {
		return maxRetryDelay
	}
	return delay
}

// extractJSON 从模型输出中截取首个平衡的 JSON 对象(容忍围栏/前后缀文本)。
func extractJSON(raw string) string {
	start := strings.IndexByte(raw, '{')
	if start < 0 {
		return ""
	}
	depth := 0
	inStr := false
	escape := false
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

// DispatchOp 是各场景共享的派单动作。
type DispatchOp struct {
	Agent string `json:"agent"`
	Task  string `json:"task"`
}

// knownWorkers 是合法派单目标(与 agents.BuildWorkers 注册的一致)。
var knownWorkers = map[string]bool{
	"architect_long":  true,
	"architect_short": true,
	"writer":          true,
	"editor":          true,
}

func (d *DispatchOp) validate() error {
	if d == nil {
		return nil
	}
	if !knownWorkers[d.Agent] {
		return fmt.Errorf("dispatch.agent 非法: %q", d.Agent)
	}
	if strings.TrimSpace(d.Task) == "" {
		return fmt.Errorf("dispatch.task 不能为空")
	}
	return nil
}

func marshalPayload(v any) string {
	data, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return "{}"
	}
	return string(data)
}
