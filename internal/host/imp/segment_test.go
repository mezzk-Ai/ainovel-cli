package imp

import (
	"context"
	"strings"
	"testing"

	"github.com/voocel/agentcore"
)

func TestUnitLessNumericNotLexical(t *testing.T) {
	// 字典序会判 L900 > L1000、L1257.2 > L1800；数值序必须相反。
	if !unitLess(SourceUnit{Line: 900}, SourceUnit{Line: 1000}) {
		t.Fatal("L900 应 < L1000（数值序）")
	}
	if !unitLess(SourceUnit{Line: 1257, Part: 2}, SourceUnit{Line: 1800}) {
		t.Fatal("L1257.2 应 < L1800")
	}
	if !unitLess(SourceUnit{Line: 1257, Part: 1}, SourceUnit{Line: 1257, Part: 2}) {
		t.Fatal("同行 part 应按数值序")
	}
	if unitLess(SourceUnit{Line: 5}, SourceUnit{Line: 5}) {
		t.Fatal("相等不应 less")
	}
}

func TestBuildSourceUnitsRoundtrip(t *testing.T) {
	norm := []byte("第一章\n正文一\n\n第二章\n正文二")
	units := buildSourceUnits(norm, 0)
	// 拼回：每个 unit 文本 + 行间 '\n' 应还原归一化文本。
	var b strings.Builder
	for i, u := range units {
		if i > 0 {
			b.WriteByte('\n')
		}
		b.WriteString(u.Text)
		if u.Text != string(norm[u.StartByte:u.EndByte]) {
			t.Fatalf("unit %s 字节范围与文本不符", u.ID)
		}
	}
	if b.String() != string(norm) {
		t.Fatalf("拼回不符：%q", b.String())
	}
	if units[0].ID != "L1" || units[3].ID != "L4" {
		t.Fatalf("ID 不符：%s %s", units[0].ID, units[3].ID)
	}
}

func TestBuildSourceUnitsVirtualShard(t *testing.T) {
	// 一整行远超预算 → 拆多个虚拟 unit，边界在 UTF-8 字符边界。
	long := strings.Repeat("字", 100) // 每字 3 字节 = 300 字节
	units := buildSourceUnits([]byte(long), 30)
	if len(units) < 2 {
		t.Fatalf("超预算行应分片，得到 %d", len(units))
	}
	var b strings.Builder
	for _, u := range units {
		if u.Line != 1 || u.Part == 0 {
			t.Fatalf("虚拟分片应同 Line、Part>=1：%+v", u)
		}
		b.WriteString(u.Text) // 分片同一行，无换行分隔
	}
	if b.String() != long {
		t.Fatal("虚拟分片拼回丢字")
	}
}

func TestResolveBoundaryByteAnchor(t *testing.T) {
	units := []SourceUnit{{ID: "L1", Line: 1, StartByte: 0, EndByte: 10, Text: "楔子风起楔"}}
	m := map[string]SourceUnit{"L1": units[0]}
	if _, err := resolveBoundaryByte(m, "L1", "风起"); err != nil {
		t.Fatalf("唯一锚点应成功：%v", err)
	}
	if _, err := resolveBoundaryByte(m, "L1", "楔"); err == nil {
		t.Fatal("重复锚点应失败")
	}
	if _, err := resolveBoundaryByte(m, "L1", "缺失"); err == nil {
		t.Fatal("不存在锚点应失败")
	}
	if _, err := resolveBoundaryByte(m, "L9", ""); err == nil {
		t.Fatal("不存在 unit 应失败")
	}
}

func TestPlanChunksCoversWithoutGap(t *testing.T) {
	units := buildSourceUnits([]byte(strings.Repeat("行内容\n", 50)), 0)
	chunks := planChunks(units, 40)
	if len(chunks) < 2 {
		t.Fatalf("应分多块，得 %d", len(chunks))
	}
	// 无缝无重叠且完整覆盖。
	if chunks[0][0] != 0 || chunks[len(chunks)-1][1] != len(units) {
		t.Fatal("未完整覆盖")
	}
	for i := 1; i < len(chunks); i++ {
		if chunks[i][0] != chunks[i-1][1] {
			t.Fatalf("块 %d 与前块不相接：%v", i, chunks)
		}
	}
}

func segFixture() ([]byte, []SourceUnit) {
	norm := []byte("前言\n感谢阅读\n第一章 风起\n正文一\n卷二\n第二章 云涌\n正文二")
	return norm, buildSourceUnits(norm, 0)
}

func TestResolveSegmentationHappy(t *testing.T) {
	norm, units := segFixture()
	// L1 前言(front) / L3 第一章 / L5 卷二(group) / L6 第二章
	decisions := []BoundaryDecision{
		{UnitID: "L1", Kind: kindFrontMatter, Title: "前言"},
		{UnitID: "L3", Kind: kindChapter, Title: "第一章 风起"},
		{UnitID: "L5", Kind: kindGroup, Title: "卷二"},
		{UnitID: "L6", Kind: kindChapter, Title: "第二章 云涌"},
	}
	seg, err := resolveSegmentation(norm, units, decisions)
	if err != nil {
		t.Fatalf("覆盖校验应通过：%v", err)
	}
	if len(seg.Chapters) != 2 {
		t.Fatalf("章节数应为 2（group 不计），得 %d", len(seg.Chapters))
	}
	if seg.Chapters[0].Number != 1 || seg.Chapters[1].Number != 2 {
		t.Fatal("章节号应连续")
	}
	if !strings.Contains(seg.Content(norm, 0), "正文一") {
		t.Fatalf("章一正文不符：%q", seg.Content(norm, 0))
	}
	// 覆盖：首段(front_matter)从 0 起，末章覆盖到文本尾。
	if len(seg.Matter) == 0 || seg.Matter[0].Kind != kindFrontMatter || seg.Matter[0].Start != 0 {
		t.Fatalf("首段应为从 0 起的 front_matter：%+v", seg.Matter)
	}
	if seg.Chapters[len(seg.Chapters)-1].End != len(norm) {
		t.Fatal("末章应覆盖到文本尾")
	}
}

func TestResolveSegmentationRejections(t *testing.T) {
	norm, units := segFixture()
	cases := []struct {
		name string
		ds   []BoundaryDecision
	}{
		{"倒序", []BoundaryDecision{
			{UnitID: "L3", Kind: kindChapter}, {UnitID: "L1", Kind: kindChapter},
		}},
		{"空正文", []BoundaryDecision{
			{UnitID: "L1", Kind: kindChapter}, {UnitID: "L2", Kind: kindChapter},
			// L2 之后 L3.. 有正文，但构造一个末尾空章：
			{UnitID: "L7", Kind: kindChapter},
		}},
		{"起始未归属非空文本", []BoundaryDecision{
			{UnitID: "L3", Kind: kindChapter}, // L1/L2 非空却无归属
		}},
		{"无章节", []BoundaryDecision{
			{UnitID: "L1", Kind: kindFrontMatter},
		}},
		{"非法kind", []BoundaryDecision{
			{UnitID: "L1", Kind: "verse"},
		}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if _, err := resolveSegmentation(norm, units, c.ds); err == nil {
				t.Fatalf("应被拒绝：%s", c.name)
			}
		})
	}
}

// mockModel 顺序返回预设响应，供 typed-call 契约测试。
// stops 可为每次调用指定 stop reason；缺省用 stop 或 StopReasonStop。
type mockModel struct {
	responses []string
	stops     []agentcore.StopReason
	i         int
	stop      agentcore.StopReason
}

func (m *mockModel) Generate(_ context.Context, _ []agentcore.Message, _ []agentcore.ToolSpec, _ ...agentcore.CallOption) (*agentcore.LLMResponse, error) {
	idx := m.i
	r := m.responses[idx%len(m.responses)]
	sr := m.stop
	if idx < len(m.stops) {
		sr = m.stops[idx]
	}
	if sr == "" {
		sr = agentcore.StopReasonStop
	}
	m.i++
	return &agentcore.LLMResponse{Message: agentcore.Message{
		Role:       agentcore.RoleAssistant,
		Content:    []agentcore.ContentBlock{agentcore.TextBlock(r)},
		StopReason: sr,
	}}, nil
}

// TestResolveSegmentationSingleLineChapters 守护 #9：无换行的单行段（锚点切分场景）整段即正文，
// 单行/单行多章小说不应被误判"正文为空"拒绝。
func TestResolveSegmentationSingleLineChapters(t *testing.T) {
	normalized := []byte("第一章甲的故事第二章乙的故事") // 整篇一行，无换行
	units := buildSourceUnits(normalized, 0)
	decisions := []BoundaryDecision{
		{UnitID: "L1", Kind: kindChapter, Title: "第一章"},                // 无锚点 → byte 0
		{UnitID: "L1", Anchor: "第二章", Kind: kindChapter, Title: "第二章"}, // 行内锚点切出第二章
	}
	seg, err := resolveSegmentation(normalized, units, decisions)
	if err != nil {
		t.Fatalf("单行多章应被接受：%v", err)
	}
	if len(seg.Chapters) != 2 {
		t.Fatalf("应切出 2 章，得 %d", len(seg.Chapters))
	}
	if got := seg.Content(normalized, 0); got != "第一章甲的故事" {
		t.Fatalf("首章正文范围不对：%q", got)
	}
}

func TestSegmentWithMockModel(t *testing.T) {
	norm, units := segFixture()
	resp := `{"boundaries":[
		{"unit_id":"L1","kind":"front_matter","title":"前言"},
		{"unit_id":"L3","kind":"chapter","title":"第一章 风起"},
		{"unit_id":"L5","kind":"group","title":"卷二"},
		{"unit_id":"L6","kind":"chapter","title":"第二章 云涌"}
	]}`
	m := &mockModel{responses: []string{resp}}
	seg, err := Segment(context.Background(), m, "sys", norm, units, "", 0, 0, 4096, callProfile{})
	if err != nil {
		t.Fatalf("Segment: %v", err)
	}
	if len(seg.Chapters) != 2 {
		t.Fatalf("应得 2 章，得 %d", len(seg.Chapters))
	}
}

func TestCallStructuredTruncation(t *testing.T) {
	m := &mockModel{responses: []string{`{"boundaries":[]}`}, stop: agentcore.StopReasonLength}
	_, err := callStructured[boundaryBatch](context.Background(), m, "s", "p", 16, callProfile{}, nil)
	var trunc *errTruncated
	if err == nil || !asTruncated(err, &trunc) {
		t.Fatalf("长度截断应返回 *errTruncated，得 %v", err)
	}
}

func asTruncated(err error, target **errTruncated) bool {
	t, ok := err.(*errTruncated)
	if ok {
		*target = t
	}
	return ok
}
