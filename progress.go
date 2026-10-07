package engine

import (
	"math"
	"sort"
	"time"
)

// playerProgress 单个玩家在单个赛季内的进度账本。
// 所有字段只在所属分片的互斥区内读写；领取/赛季切换需要跨分片时
// 由引擎先取得该分片锁，因此任何时刻对外可见的都是一个自洽版本。
type playerProgress struct {
	PlayerID          PlayerID
	SeasonID          SeasonID
	RuleVersion       RuleVersion
	RawProgress       int64 // 原始累计进度（仅观测，封顶/递减不改写它）
	EffectiveProgress int64 // 实际生效的累计进度，决定等级
	Level             int
	SeasonGranted     int64 // 本赛季已发放有效进度（= CarriedOver + 本赛季事件入账，用于赛季封顶）
	CarriedOver       int64 // 赛季切换时由上一赛季按比例结转的初始有效进度
	DailyGranted      map[string]int64
	EventTimes        []int64        // 已结算事件 OccurredAt(UnixNano)，保持升序，供高频窗口统计
	ClaimedLevels     map[int]string // level -> 首个成功提交的 claimID
	PendingClaim      *ClaimRecord   // 两阶段领取中尚未 commit/abort 的半成品
	Settled           bool
}

func newPlayerProgress(id PlayerID, season SeasonID, rv RuleVersion) *playerProgress {
	return &playerProgress{
		PlayerID:      id,
		SeasonID:      season,
		RuleVersion:   rv,
		DailyGranted:  map[string]int64{},
		ClaimedLevels: map[int]string{},
	}
}

func normalizeDecay(d DecayConfig) DecayConfig {
	if d.Window <= 0 {
		d.Window = 60 * time.Second
	}
	if d.Gamma <= 0 || d.Gamma > 1 {
		d.Gamma = 0.8
	}
	if d.MinFactor <= 0 || d.MinFactor > 1 {
		d.MinFactor = 0.1
	}
	if d.MinFactor > d.Gamma {
		d.MinFactor = d.Gamma
	}
	return d
}

func dayBucket(t time.Time) string {
	u := t.UTC()
	return u.Format("2006-01-02")
}

// gain 是纯函数：同样的 (事件, 既有账本, 赛季配置) 永远得到同样的明细，
// 不依赖处理顺序之外的任何外部状态，结算结果因此完全可解释、可复现。
//
// 计算顺序：
//  1. 高频边际递减：以 OccurredAt 为右边界的滑动窗口内，
//     此前每多一条已结算事件，因子 *= Gamma，最低 MinFactor；
//  2. 每日封顶：按 OccurredAt 的 UTC 日期分桶；
//  3. 赛季封顶：本赛季累计有效进度不得超过 SeasonCap。
//
// 被各环节截掉的量逐字段保留在 Breakdown 中。
func gain(ev Event, p *playerProgress, cfg *SeasonConfig) *GainBreakdown {
	decay := normalizeDecay(cfg.Decay)
	at := ev.OccurredAt
	day := dayBucket(at)
	windowStart := at.Add(-decay.Window).UnixNano()
	atNano := at.UnixNano()

	// 窗口内 [at-window, at] 的此前事件数；EventTimes 升序。
	lo := sort.Search(len(p.EventTimes), func(i int) bool { return p.EventTimes[i] >= windowStart })
	hi := sort.Search(len(p.EventTimes), func(i int) bool { return p.EventTimes[i] > atNano })
	prior := hi - lo
	if prior < 0 {
		prior = 0
	}
	factor := math.Pow(decay.Gamma, float64(prior))
	if factor < decay.MinFactor {
		factor = decay.MinFactor
	}

	gross := ev.Amount
	afterDecay := int64(math.Floor(float64(gross) * factor))

	b := &GainBreakdown{
		EventID:         ev.ID,
		SeasonID:        cfg.ID,
		DayBucket:       day,
		Gross:           gross,
		PriorInWindow:   prior,
		FrequencyFactor: factor,
		AfterDecay:      afterDecay,
		DailyCap:        cfg.DailyCap,
		SeasonCap:       cfg.SeasonCap,
	}

	grant := afterDecay
	if cfg.DailyCap > 0 {
		used := p.DailyGranted[day]
		if used >= cfg.DailyCap {
			b.DailyCut = grant
			grant = 0
		} else if grant > cfg.DailyCap-used {
			b.DailyCut = grant - (cfg.DailyCap - used)
			grant = cfg.DailyCap - used
		}
	}
	if cfg.SeasonCap > 0 && grant > 0 {
		if p.SeasonGranted >= cfg.SeasonCap {
			b.SeasonCut = grant
			grant = 0
		} else if grant > cfg.SeasonCap-p.SeasonGranted {
			b.SeasonCut = grant - (cfg.SeasonCap - p.SeasonGranted)
			grant = cfg.SeasonCap - p.SeasonGranted
		}
	}
	b.Granted = grant
	return b
}

// applyGain 把已接受事件的结算明细并入账本，并推进等级。
// 等级只能单调前进；迟到事件只影响账本中未被外部领取固化的部分，
// 已发放的等级奖励由 ClaimedLevels 固化，不会被回退。
func (p *playerProgress) applyGain(b *GainBreakdown, thresholds []int64, at time.Time) {
	p.RawProgress += b.Gross
	p.EffectiveProgress += b.Granted
	p.SeasonGranted += b.Granted
	p.DailyGranted[b.DayBucket] += b.Granted
	n := at.UnixNano()
	idx := sort.Search(len(p.EventTimes), func(i int) bool { return p.EventTimes[i] > n })
	p.EventTimes = append(p.EventTimes, 0)
	copy(p.EventTimes[idx+1:], p.EventTimes[idx:])
	p.EventTimes[idx] = n
	if lvl := levelFor(p.EffectiveProgress, thresholds); lvl > p.Level {
		p.Level = lvl
	}
}

// levelFor 返回累计有效进度对应的等级：未达第一个阈值为 0，
// 否则为满足 thresholds[i] <= progress 的最大下标 +1。
func levelFor(progress int64, thresholds []int64) int {
	idx := sort.Search(len(thresholds), func(i int) bool { return thresholds[i] > progress })
	return idx
}
