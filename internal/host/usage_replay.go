package host

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"strings"

	"github.com/voocel/agentcore"
)

// sessionRecord 是 meta/sessions/*.jsonl 单条记录的轻量解析形态——只取
// 累计 usage 需要的字段。Content 等大字段跳过解析，节省启动期 IO。
//
// Meta 字段（_meta）由 SessionStore 在写入时附加，含当时生效的 provider/model。
// 老版本 jsonl 没有这个字段 → Meta=nil，replay 时退回 ModelSet 当前查表，
// 这也是 "session 加 _meta 之前的历史 cost 仍为估算" 的兼容门。
type sessionRecord struct {
	Role  agentcore.Role     `json:"role"`
	Usage *agentcore.Usage   `json:"usage,omitempty"`
	Meta  *sessionRecordMeta `json:"_meta,omitempty"`
}

type sessionRecordMeta struct {
	Provider string `json:"provider,omitempty"`
	Model    string `json:"model,omitempty"`
}

// ReplaySessions 扫 meta/sessions/coordinator.jsonl 与 meta/sessions/agents/*.jsonl，
// 把每条 assistant 消息的 usage 重新累加到 tracker。返回回填条数。
//
// 调用约束：仅在 meta/usage.json 缺失（首次升级或 schema 变更）时调用一次，做
// 历史数据回填。日常持久化走 SaveNow / autoSaveLoop。
//
// 已知精度损失：session log 里没记"当时这条消息用的是哪个模型"。回放时只能拿
// 当前 ModelSet 给每个 role 反推单价；如果运行中切过模型，历史 cost 会和真实
// 账单有差。Token 数本身是精确的——回放后即便 cost 估算偏差，也胜过完全没有累计。
func (t *UsageTracker) ReplaySessions(rootDir string) (int, error) {
	if t == nil {
		return 0, nil
	}
	sessionsDir := filepath.Join(rootDir, "meta", "sessions")
	info, err := os.Stat(sessionsDir)
	if err != nil {
		if os.IsNotExist(err) {
			return 0, nil
		}
		return 0, err
	}
	if !info.IsDir() {
		return 0, nil
	}

	total := 0
	if n, err := t.replayFile(filepath.Join(sessionsDir, "coordinator.jsonl"), "coordinator"); err != nil {
		slog.Warn("replay coordinator session failed", "module", "usage", "err", err)
	} else {
		total += n
	}

	agentsDir := filepath.Join(sessionsDir, "agents")
	walkErr := filepath.WalkDir(agentsDir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			if os.IsNotExist(err) {
				return nil
			}
			return err
		}
		if d.IsDir() {
			return nil
		}
		name := d.Name()
		if !strings.HasSuffix(name, ".jsonl") {
			return nil
		}
		agentName := parseAgentNameFromFile(name)
		if agentName == "" {
			return nil
		}
		n, fileErr := t.replayFile(path, agentName)
		if fileErr != nil {
			slog.Warn("replay agent session failed", "module", "usage", "file", name, "err", fileErr)
			return nil
		}
		total += n
		return nil
	})
	if walkErr != nil && !os.IsNotExist(walkErr) {
		return total, walkErr
	}
	return total, nil
}

// replayFile 扫单个 jsonl 文件，把所有带 Usage 的 assistant 消息喂给 accumulate。
// agentName 由调用方传入（coordinator 或文件名解析的 sub-agent 名）。
func (t *UsageTracker) replayFile(path, agentName string) (int, error) {
	f, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			return 0, nil
		}
		return 0, err
	}
	defer f.Close()

	role := agentRoleName(agentName)
	count := 0
	scanner := bufio.NewScanner(f)
	// 单行可能很长（assistant 消息 + tool args 等都打平了），放宽到 4MB。
	scanner.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	for scanner.Scan() {
		line := scanner.Bytes()
		if len(line) == 0 {
			continue
		}
		var rec sessionRecord
		if err := json.Unmarshal(line, &rec); err != nil {
			continue
		}
		if rec.Role != agentcore.RoleAssistant || rec.Usage == nil {
			continue
		}
		modelName := ""
		if rec.Meta != nil {
			modelName = rec.Meta.Model
		}
		t.accumulate(role, modelName, *rec.Usage)
		count++
	}
	if err := scanner.Err(); err != nil {
		return count, fmt.Errorf("scan %s: %w", path, err)
	}
	return count, nil
}

// parseAgentNameFromFile 从 "writer-ch01.jsonl" / "architect_short-001.jsonl" 提取
// agent 名（"-" 之前部分）。命名约定见 store/session.go::subAgentPath：
// agentName 不含 dash，suffix 是 ch<n> 或递增序号。
func parseAgentNameFromFile(name string) string {
	base := strings.TrimSuffix(name, ".jsonl")
	if i := strings.Index(base, "-"); i > 0 {
		return base[:i]
	}
	return ""
}
