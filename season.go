package engine

import (
	"sort"
	"sync"
	"time"
)

// seasonTable 是不可变赛季配置表（只追加，不修改既有赛季）。
// 事件的赛季归属仅由 OccurredAt 决定：满足 Start <= OccurredAt < End；
// End 为零值时表示开放式赛季。处理事件的墙钟时间不参与归属判定，
// 因此切换窗口内到达的事件不会因处理时机不同而出现归属摇摆。
type seasonTable struct {
	mu      sync.RWMutex
	seasons []SeasonConfig // 按 Start 升序
	byID    map[SeasonID]*SeasonConfig
}

func newSeasonTable() *seasonTable {
	return &seasonTable{byID: map[SeasonID]*SeasonConfig{}}
}

func validateSeason(c SeasonConfig) error {
	if c.ID == "" {
		return ErrSeasonNotFound
	}
	if !c.End.IsZero() && !c.End.After(c.Start) {
		return ErrSeasonNotFound
	}
	if c.CarryRatio < 0 || c.CarryRatio > 1 {
		return ErrSeasonNotFound
	}
	for i := 1; i < len(c.LevelThresholds); i++ {
		if c.LevelThresholds[i] <= c.LevelThresholds[i-1] {
			return ErrSeasonNotFound
		}
	}
	if c.RuleVersion == "" {
		return ErrRuleNotFound
	}
	return nil
}

func (t *seasonTable) add(c SeasonConfig) error {
	if err := validateSeason(c); err != nil {
		return err
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if _, ok := t.byID[c.ID]; ok {
		return ErrSeasonNotFound
	}
	// 区间不得与既有赛季重叠（零值 End 视为无限延伸）。
	for _, s := range t.seasons {
		if rangesOverlap(s, c) {
			return ErrSeasonNotFound
		}
	}
	t.seasons = append(t.seasons, c)
	sort.Slice(t.seasons, func(i, j int) bool { return t.seasons[i].Start.Before(t.seasons[j].Start) })
	t.byID[c.ID] = &t.seasons[len(t.seasons)-1]
	// append 后重新回填指针（切片排序不改底层元素地址，但 append 可能换数组）。
	for i := range t.seasons {
		t.byID[t.seasons[i].ID] = &t.seasons[i]
	}
	return nil
}

func rangesOverlap(a, b SeasonConfig) bool {
	aEnd := a.End
	bEnd := b.End
	if aEnd.IsZero() {
		aEnd = time.Date(9999, 12, 31, 0, 0, 0, 0, time.UTC)
	}
	if bEnd.IsZero() {
		bEnd = time.Date(9999, 12, 31, 0, 0, 0, 0, time.UTC)
	}
	return a.Start.Before(bEnd) && b.Start.Before(aEnd)
}

// locate 依据事件发生时间找到归属赛季；已结束且未覆盖该时刻则返回 ErrSeasonNotFound。
func (t *seasonTable) locate(at time.Time) (*SeasonConfig, error) {
	t.mu.RLock()
	defer t.mu.RUnlock()
	var found *SeasonConfig
	for i := range t.seasons {
		s := &t.seasons[i]
		if at.Before(s.Start) {
			continue
		}
		if !s.End.IsZero() && !at.Before(s.End) {
			continue
		}
		found = s
	}
	if found == nil {
		return nil, ErrSeasonNotFound
	}
	// 返回拷贝，避免调用方持有内部切片元素
	cp := *found
	return &cp, nil
}

func (t *seasonTable) get(id SeasonID) (*SeasonConfig, bool) {
	t.mu.RLock()
	defer t.mu.RUnlock()
	s, ok := t.byID[id]
	if !ok {
		return nil, false
	}
	cp := *s
	return &cp, true
}

// activeID 返回某墙钟时刻处于开放区间的赛季 ID（用于新玩家视角/切换判断）。
func (t *seasonTable) activeID(at time.Time) (SeasonID, bool) {
	s, err := t.locate(at)
	if err != nil {
		return "", false
	}
	return s.ID, true
}

func (t *seasonTable) list() []SeasonID {
	t.mu.RLock()
	defer t.mu.RUnlock()
	ids := make([]SeasonID, 0, len(t.seasons))
	for _, s := range t.seasons {
		ids = append(ids, s.ID)
	}
	return ids
}
