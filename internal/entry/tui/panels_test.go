package tui

import (
	"strings"
	"testing"

	"github.com/charmbracelet/lipgloss"
	"github.com/voocel/ainovel-cli/internal/host"
)

func TestRenderTopBarShowsVersion(t *testing.T) {
	out := renderTopBar(host.UISnapshot{
		Provider:  "openrouter",
		ModelName: "test-model",
		NovelName: "测试小说",
	}, 120, "", "v1.2.3")
	if !strings.Contains(out, "ainovel-cli v1.2.3") {
		t.Fatalf("top bar missing version: %q", out)
	}
}

func TestBuildRightInfoShowsThinkingLevelAfterModel(t *testing.T) {
	out := buildRightInfo(host.UISnapshot{
		Provider:           "openrouter",
		ModelName:          "test-model",
		ModelContextWindow: 200000,
		ThinkingLevel:      "medium",
	}, "/tmp/output")
	if !strings.Contains(out, "test-model(200K,med)") {
		t.Fatalf("right info missing compact thinking level: %q", out)
	}
}

func TestBuildRightInfoShowsAutoThinkingWhenUnset(t *testing.T) {
	out := buildRightInfo(host.UISnapshot{
		ModelName:          "test-model",
		ModelContextWindow: 128000,
	}, "")
	if !strings.Contains(out, "test-model(128K,auto)") {
		t.Fatalf("right info missing auto thinking level: %q", out)
	}
}

func TestRenderUsageLineSeparatesFullWidthNameAndTokens(t *testing.T) {
	out := renderUsageLine("gpt-5.6-sol", bodyTextColor, 5300, 0, 0.23, 32)
	if !strings.Contains(out, "gpt-5.6-sol 5.3k") {
		t.Fatalf("model name and tokens should have a visible gap: %q", out)
	}
}

func TestTruncateByDisplayWidth(t *testing.T) {
	// 纯中文按视觉宽度截：10 列预算 = 3 个汉字(6列) + "..."(3列)，按 rune 截会溢出到 17 列
	got := truncate("临港市公共算法伦理审计员", 10)
	if w := lipgloss.Width(got); w > 10 {
		t.Errorf("truncate 溢出列宽: %d > 10 (%q)", w, got)
	}
	if !strings.HasSuffix(got, "...") {
		t.Errorf("超宽截断应带省略号: %q", got)
	}
	// ASCII 行为与旧实现一致
	if got := truncate("abcdef", 6); got != "abcdef" {
		t.Errorf("未超宽不应截断: %q", got)
	}
	if got := truncate("abcdefgh", 6); got != "abc..." {
		t.Errorf("ASCII 截断: got %q want %q", got, "abc...")
	}
}

func TestRenderDetailContentWrapsCJK(t *testing.T) {
	long := "沈砚（主角；临港市公共算法伦理审计员，台风夜事故的调查负责人，坚持程序正义）"
	const contentW = 40
	out := renderDetailContent(host.UISnapshot{
		Characters:       []string{long},
		SupportingCount:  1,
		RecentSupporting: []string{long},
		RecentSummaries:  []string{"第6章：" + long},
	}, contentW)
	for line := range strings.SplitSeq(out, "\n") {
		if w := lipgloss.Width(line); w > contentW {
			t.Errorf("行溢出面板宽度: %d > %d (%q)", w, contentW, line)
		}
	}
	// 长描述应折成多行（悬挂缩进续行），而不是截断丢信息
	joined := strings.ReplaceAll(strings.ReplaceAll(out, "\n", ""), " ", "")
	if !strings.Contains(joined, "坚持程序正义") {
		t.Errorf("折行后应保留完整描述，实际输出:\n%s", out)
	}
}
