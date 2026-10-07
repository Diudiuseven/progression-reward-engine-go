package engine

import "time"

// janitor 周期性淘汰已结束且超出保留数量的赛季账本与去重集合。
// 它只回收“已经结算且超过保留窗口”的赛季，因此既保证长期运行内存有界，
// 也不影响当前赛季与保留期内旧赛季的奖励领取。
func (e *Engine) janitor() {
	defer e.wg.Done()
	t := time.NewTicker(e.cfg.EvictInterval)
	defer t.Stop()
	for {
		select {
		case <-e.stopJanitor:
			return
		case now := <-t.C:
			e.evictOldSettled(now)
		}
	}
}

func (e *Engine) evictOldSettled(now time.Time) {
	if e.cfg.RetainSettled <= 0 {
		return
	}
	e.seasonsMu.RLock()
	var ended []SeasonConfig
	for _, id := range e.seasons.list() {
		s, ok := e.seasons.get(id)
		if !ok || s.End.IsZero() {
			continue
		}
		if now.Before(s.End) {
			continue
		}
		ended = append(ended, *s)
	}
	e.seasonsMu.RUnlock()
	// 按结束时间升序，仅保留最近 RetainSettled 个。
	for i := 0; i < len(ended); i++ {
		for j := i + 1; j < len(ended); j++ {
			if ended[j].End.Before(ended[i].End) {
				ended[i], ended[j] = ended[j], ended[i]
			}
		}
	}
	drop := len(ended) - e.cfg.RetainSettled
	for i := 0; i < drop; i++ {
		_ = e.EvictSettled(ended[i].ID)
	}
}
