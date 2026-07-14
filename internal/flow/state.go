package flow

import (
	"github.com/voocel/ainovel-cli/internal/domain"
	storepkg "github.com/voocel/ainovel-cli/internal/store"
)

// LoadState 从 Store 读取 Route 所需的全部事实。
// 这是路由的"IO 边界"：所有读取集中在这里，Route 保持纯。
// 读取失败按保守默认填充（has*=false, boundary=nil），让 Router 倾向重派而非跳过。
func LoadState(store *storepkg.Store) State {
	s := State{
		FoundationMissing: store.FoundationMissing(),
	}
	// 规划级别:save_foundation 落 scale 时写入 RunMeta,补齐分支据此推导规划师。
	// 读失败按未知处理(tier 空 → 补齐交 LLM 裁定),与其余事实的保守默认一致。
	if meta, err := store.RunMeta.Load(); err == nil && meta != nil {
		s.PlanningTier = meta.PlanningTier
	}
	progress, err := store.Progress.Load()
	if err != nil || progress == nil {
		return s
	}
	s.Progress = progress

	if n := len(progress.CompletedChapters); n > 0 {
		s.LastCompleted = progress.CompletedChapters[n-1]
	}

	// 弧边界仅在分层模式且有已完成章节时才计算
	if progress.Layered && s.LastCompleted > 0 {
		if boundary, berr := store.Outline.CheckArcBoundary(s.LastCompleted); berr == nil && boundary != nil {
			s.ArcBoundary = boundary
			if boundary.IsArcEnd {
				s.HasArcReview = store.World.HasArcReview(s.LastCompleted)
				s.HasArcSummary = store.Summaries.HasArcSummary(boundary.Volume, boundary.Arc)
				if boundary.IsVolumeEnd {
					s.HasVolumeSummary = store.Summaries.HasVolumeSummary(boundary.Volume)
				}
			}
		}
	}

	// 非分层全局审阅事实:仅在触发点读盘(其余组合 Route 不消费该字段)。
	if !progress.Layered && s.LastCompleted > 0 {
		if due, _ := domain.ShouldReview(len(progress.CompletedChapters)); due {
			s.HasGlobalReview = store.World.HasGlobalReview(s.LastCompleted)
		}
	}

	return s
}
