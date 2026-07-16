package imp

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
)

// BoundaryDecision 是模型对单个 owned range 的边界判断（RFC §8.2）。
type BoundaryDecision struct {
	UnitID    string `json:"unit_id"`
	Anchor    string `json:"anchor,omitempty"`
	Kind      string `json:"kind"` // chapter / group / front_matter / back_matter
	Title     string `json:"title,omitempty"`
	Uncertain bool   `json:"uncertain,omitempty"`
	Reason    string `json:"reason,omitempty"`
}

const (
	kindChapter     = "chapter"
	kindGroup       = "group"
	kindFrontMatter = "front_matter"
	kindBackMatter  = "back_matter"
)

// boundaryBatch 是一次分段调用的结构化返回。
type boundaryBatch struct {
	Boundaries []BoundaryDecision `json:"boundaries"`
}

// ChapterSpan 是切分确认后的一个可提交章节：标题 + 归一化文本字节范围（含标题行）。
type ChapterSpan struct {
	Number int    `json:"number"`
	Title  string `json:"title"`
	Start  int    `json:"start_byte"`
	End    int    `json:"end_byte"`
}

// MatterSpan 是卷/篇标题或明确的附属区域。
type MatterSpan struct {
	Kind  string `json:"kind"`
	Title string `json:"title,omitempty"`
	Start int    `json:"start_byte"`
	End   int    `json:"end_byte"`
}

// Segmentation 是全文覆盖校验通过的切分结果（confirmation 与逐章分析的上游）。
type Segmentation struct {
	Chapters  []ChapterSpan `json:"chapters"`
	Matter    []MatterSpan  `json:"matter,omitempty"`    // group / front / back
	Uncertain []int         `json:"uncertain,omitempty"` // 标记 uncertain 的章节号，供预览提示
}

// Content 返回第 i 个章节的归一化正文（含标题行）。
func (s *Segmentation) Content(normalized []byte, i int) string {
	c := s.Chapters[i]
	return string(normalized[c.Start:c.End])
}

// resolveSegmentation 把有序边界决策映射为经全文覆盖校验的 Segmentation（RFC §8.3）。
// 纯函数：模型输出与代码校验分离，"某行是不是章标题"不由 Go 复判，但覆盖不变量必须成立。
func resolveSegmentation(normalized []byte, units []SourceUnit, decisions []BoundaryDecision) (*Segmentation, error) {
	if len(decisions) == 0 {
		return nil, fmt.Errorf("未识别到任何边界")
	}
	// 前置契约：units 必须按 (Line,Part) 数值序排列（禁止 ID 字典序）。
	for i := 1; i < len(units); i++ {
		if !unitLess(units[i-1], units[i]) {
			return nil, fmt.Errorf("SourceUnit 未按 (Line,Part) 数值序排列：%s 后接 %s", units[i-1].ID, units[i].ID)
		}
	}
	unitByID := make(map[string]SourceUnit, len(units))
	for _, u := range units {
		unitByID[u.ID] = u
	}

	type point struct {
		byte int
		d    BoundaryDecision
	}
	points := make([]point, 0, len(decisions))
	prevByte := -1
	for i, d := range decisions {
		switch d.Kind {
		case kindChapter, kindGroup, kindFrontMatter, kindBackMatter:
		default:
			return nil, fmt.Errorf("边界[%d] kind 非法：%q", i, d.Kind)
		}
		b, err := resolveBoundaryByte(unitByID, d.UnitID, d.Anchor)
		if err != nil {
			return nil, err
		}
		if b <= prevByte {
			return nil, fmt.Errorf("边界[%d] 未按 (Line,Part) 数值序严格递增（byte %d <= %d）", i, b, prevByte)
		}
		prevByte = b
		points = append(points, point{byte: b, d: d})
	}
	// 首个边界必须落在文本起点，否则起始存在未归属文本（RFC §8.3.5）。
	if points[0].byte != 0 {
		if strings.TrimSpace(string(normalized[:points[0].byte])) != "" {
			return nil, fmt.Errorf("首个边界前存在未归属的非空文本（byte 0..%d）", points[0].byte)
		}
	}

	seg := &Segmentation{}
	chapterNo := 0
	for i, p := range points {
		start := p.byte
		if i == 0 {
			start = 0 // 首段吸收起始处的空白
		}
		end := len(normalized)
		if i+1 < len(points) {
			end = points[i+1].byte
		}
		title := strings.TrimSpace(p.d.Title)
		if title == "" {
			title = firstLine(normalized, p.byte, end)
		}
		switch p.d.Kind {
		case kindChapter:
			if strings.TrimSpace(bodyAfterTitle(normalized, p.byte, end)) == "" {
				return nil, fmt.Errorf("章节 %q 正文范围为空（byte %d..%d）", title, start, end)
			}
			chapterNo++
			seg.Chapters = append(seg.Chapters, ChapterSpan{Number: chapterNo, Title: title, Start: start, End: end})
			if p.d.Uncertain {
				seg.Uncertain = append(seg.Uncertain, chapterNo)
			}
		default:
			seg.Matter = append(seg.Matter, MatterSpan{Kind: p.d.Kind, Title: title, Start: start, End: end})
		}
	}
	if chapterNo == 0 {
		return nil, fmt.Errorf("切分未产出任何章节（group 不计入章节）")
	}
	return seg, nil
}

// firstLine 返回 [start,end) 内首行去空白后的文本。
func firstLine(normalized []byte, start, end int) string {
	s := string(normalized[start:end])
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[:i]
	}
	return strings.TrimSpace(s)
}

// bodyAfterTitle 返回 [start,end) 去掉首行（标题）后的正文。
// 多行章节标题独占首行，正文在其后；无换行的单行段（锚点切分场景）整段即正文，
// 此时返回全段而非空串——否则合法的单行/单行多章小说会被误判"正文为空"拒绝（RFC §8.3）。
func bodyAfterTitle(normalized []byte, start, end int) string {
	s := string(normalized[start:end])
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[i+1:]
	}
	return s
}

// planChunks 按字节预算把 units 切成互不重叠、完整覆盖的 owned 索引区间 [start,end)。
// 分块大小由上下文预算计算，不按固定行数或章节数（RFC §8.1）。
func planChunks(units []SourceUnit, budgetBytes int) [][2]int {
	if len(units) == 0 {
		return nil
	}
	if budgetBytes <= 0 {
		return [][2]int{{0, len(units)}}
	}
	var chunks [][2]int
	start := 0
	acc := 0
	for i, u := range units {
		size := u.EndByte - u.StartByte
		if acc > 0 && acc+size > budgetBytes {
			chunks = append(chunks, [2]int{start, i})
			start = i
			acc = 0
		}
		acc += size
	}
	chunks = append(chunks, [2]int{start, len(units)})
	return chunks
}

// buildProjection 组装一个 owned 区间的结构投影 payload（含少量上下文），模型只为 owned 返回边界。
func buildProjection(units []SourceUnit, owned [2]int, contextMargin int, guidance string) string {
	lo := owned[0] - contextMargin
	if lo < 0 {
		lo = 0
	}
	hi := owned[1] + contextMargin
	if hi > len(units) {
		hi = len(units)
	}
	type projUnit struct {
		ID   string `json:"id"`
		Text string `json:"text"`
	}
	proj := struct {
		OwnedStart   string     `json:"owned_start"`
		OwnedEnd     string     `json:"owned_end"`
		Units        []projUnit `json:"units"`
		UserGuidance string     `json:"user_guidance,omitempty"`
	}{
		OwnedStart:   units[owned[0]].ID,
		OwnedEnd:     units[owned[1]-1].ID,
		UserGuidance: guidance,
	}
	for i := lo; i < hi; i++ {
		proj.Units = append(proj.Units, projUnit{ID: units[i].ID, Text: units[i].Text})
	}
	data, _ := json.MarshalIndent(proj, "", "  ")
	return string(data)
}

// segmentInputDigest 覆盖分段动作实际消费的语义输入：归一化源、用户指导、prompt 版本（RFC §6.3）。
func segmentInputDigest(normalizedDigest, guidance, promptVersion string) string {
	return Digest([]byte(strings.Join([]string{"segment", promptVersion, normalizedDigest, guidance}, "\x00")))
}

// Segment 对整份归一化文本做语义切分：逐 owned 区间调用模型识别边界，再全文覆盖校验。
// contextMargin 上下文单元数，chunkBytes owned 区间字节预算，maxTokens 单次输出预算。
func Segment(ctx context.Context, m callModel, systemPrompt string, normalized []byte, units []SourceUnit, guidance string, chunkBytes, contextMargin, maxTokens int, prof callProfile) (*Segmentation, error) {
	chunks := planChunks(units, chunkBytes)
	var decisions []BoundaryDecision
	for _, owned := range chunks {
		payload := buildProjection(units, owned, contextMargin, guidance)
		lo, hi := units[owned[0]], units[owned[1]-1]
		ownedIDs := make(map[string]bool, owned[1]-owned[0])
		for i := owned[0]; i < owned[1]; i++ {
			ownedIDs[units[i].ID] = true
		}
		batch, err := callStructured[boundaryBatch](ctx, m, systemPrompt, payload, maxTokens, prof, func(b *boundaryBatch) error {
			return validateOwnedBoundaries(b.Boundaries, ownedIDs)
		})
		if err != nil {
			return nil, fmt.Errorf("切分区间 %s..%s：%w", lo.ID, hi.ID, err)
		}
		decisions = append(decisions, batch.Boundaries...)
	}
	return resolveSegmentation(normalized, units, decisions)
}

// validateOwnedBoundaries 校验单区间返回的边界都落在本次 owned range 内（RFC §8.1/§8.3.1）。
// 上下文区（context margin）只供模型判断语境，不得在此返回边界；越界即反馈重问，
// 避免相邻块各自为对方 owned 返回边界造成的跨块顺序冲突。
func validateOwnedBoundaries(bs []BoundaryDecision, ownedIDs map[string]bool) error {
	for _, b := range bs {
		if b.UnitID == "" {
			return fmt.Errorf("边界缺 unit_id")
		}
		if !ownedIDs[b.UnitID] {
			return fmt.Errorf("边界 unit_id %q 不在本次 owned 区间内（不得为上下文区返回边界）", b.UnitID)
		}
	}
	return nil
}
