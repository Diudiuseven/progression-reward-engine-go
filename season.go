package engine

import (
	"fmt"
	"sort"
	"time"
)

// SeasonManager 管理赛季列表与当前活跃赛季。
// 事件归属只由 EventTime 落在哪个赛季的 [Start, End) 区间决定，与到达时间无关，
// 因此切换过程中到达的事件归属确定，不会摇摆。
type SeasonManager struct {
	seasons   []SeasonConfig // 按 Start 升序
	activeIdx int            // 当前活跃赛季下标，-1 表示尚未开始
	settled   map[string]bool
}

func NewSeasonManager(cfgs []SeasonConfig, now time.Time) (*SeasonManager, error) {
	if len(cfgs) == 0 {
		return nil, fmt.Errorf("at least one season required")
	}
	seasons := make([]SeasonConfig, len(cfgs))
	copy(seasons, cfgs)
	sort.Slice(seasons, func(a, b int) bool { return seasons[a].Start.Before(seasons[b].Start) })
	for i := 1; i < len(seasons); i++ {
		if seasons[i].Start.Before(seasons[i-1].End) {
			return nil, fmt.Errorf("season %q overlaps %q", seasons[i].SeasonID, seasons[i-1].SeasonID)
		}
	}
	m := &SeasonManager{seasons: seasons, activeIdx: -1, settled: map[string]bool{}}
	// 初始定位：落在哪个赛季区间即激活哪个；之前的赛季视为已结算。
	for i := range m.seasons {
		if !now.Before(m.seasons[i].Start) && now.Before(m.seasons[i].End) {
			m.activeIdx = i
			for j := 0; j < i; j++ {
				m.settled[m.seasons[j].SeasonID] = true
			}
			break
		}
	}
	return m, nil
}

// SeasonFor 返回 EventTime 所属的赛季。
func (m *SeasonManager) SeasonFor(t time.Time) *SeasonConfig {
	for i := range m.seasons {
		if !t.Before(m.seasons[i].Start) && t.Before(m.seasons[i].End) {
			return &m.seasons[i]
		}
	}
	return nil
}

// Active 返回当前活跃赛季。
func (m *SeasonManager) Active() *SeasonConfig {
	if m.activeIdx < 0 {
		return nil
	}
	return &m.seasons[m.activeIdx]
}

// IsSettled 赛季是否已结算（结算后迟到事件不得改写其结论）。
func (m *SeasonManager) IsSettled(seasonID string) bool {
	return m.settled[seasonID]
}

// Advance 结算当前活跃赛季并推进到包含 now 的下一赛季。
// 返回被结算的旧赛季与新激活的赛季。
func (m *SeasonManager) Advance(now time.Time) (old *SeasonConfig, new *SeasonConfig, err error) {
	target := -1
	for i := range m.seasons {
		if !now.Before(m.seasons[i].Start) && now.Before(m.seasons[i].End) {
			target = i
			break
		}
	}
	if target < 0 {
		return nil, nil, fmt.Errorf("no season covers %s", now)
	}
	if target <= m.activeIdx {
		return nil, nil, fmt.Errorf("season covering %s is not ahead of active season", now)
	}
	if m.activeIdx >= 0 {
		old = &m.seasons[m.activeIdx]
	}
	// 中间跳过的赛季一并结算
	for j := 0; j < target; j++ {
		m.settled[m.seasons[j].SeasonID] = true
	}
	m.activeIdx = target
	return old, &m.seasons[target], nil
}

// SettledIDs 返回已结算赛季列表（用于持久化恢复）。
func (m *SeasonManager) SettledIDs() []string {
	out := make([]string, 0, len(m.settled))
	for id := range m.settled {
		out = append(out, id)
	}
	sort.Strings(out)
	return out
}

// Restore 从持久化状态恢复活跃赛季与已结算集合。
func (m *SeasonManager) Restore(activeSeasonID string, settledIDs []string) error {
	for _, id := range settledIDs {
		m.settled[id] = true
	}
	for i := range m.seasons {
		if m.seasons[i].SeasonID == activeSeasonID {
			m.activeIdx = i
			return nil
		}
	}
	return fmt.Errorf("active season %q not found in configs", activeSeasonID)
}
