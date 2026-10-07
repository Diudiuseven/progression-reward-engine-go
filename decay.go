package engine

import "math"

// computeGain 依据规则对一次原始进度计算有效进度。
// 模型：日内累计采用 D(x) = DayCap * (1 - exp(-x / DayKnee)) 的平滑递减曲线，
// 单次事件的有效进度 = D(dayBefore+raw) - D(dayBefore)，随当日累计边际递减并渐近封顶于 DayCap；
// 再受赛季硬上限 SeasonCap 截断。全部为确定性浮点运算（Go 浮点遵循 IEEE754，同输入同输出），
// 账本以 float64 落盘，可复现、可解释。
// 返回 (effective, reason)。
func computeGain(rule *RuleVersion, dayBefore, seasonBefore float64, raw int64) (float64, string) {
	if raw <= 0 {
		return 0, "ok"
	}
	// 赛季硬上限
	seasonRemaining := float64(rule.SeasonCap) - seasonBefore
	if seasonRemaining <= 0 {
		return 0, "season_cap"
	}

	eff := marginalDayGain(rule, dayBefore, float64(raw))
	if eff > seasonRemaining {
		eff = seasonRemaining
	}
	if eff <= 0 {
		return 0, "day_cap"
	}
	reason := "ok"
	if eff < float64(raw) {
		reason = "decayed"
	}
	if seasonBefore+eff >= float64(rule.SeasonCap) {
		reason = "season_cap"
	}
	return eff, reason
}

// dayAccrued 日内累计原始进度 x 对应的有效收益（连续递减曲线，渐近 DayCap）。
func dayAccrued(rule *RuleVersion, x float64) float64 {
	if x <= 0 {
		return 0
	}
	knee := float64(rule.DayKnee)
	if knee <= 0 {
		knee = 1
	}
	return float64(rule.DayCap) * (1 - math.Exp(-x/knee))
}

// marginalDayGain 当日已累计 dayBefore 时，追加 raw 带来的有效收益增量。
// 对固定 raw 关于 dayBefore 严格递减，保证高频收益平滑衰减。
func marginalDayGain(rule *RuleVersion, dayBefore, raw float64) float64 {
	return dayAccrued(rule, dayBefore+raw) - dayAccrued(rule, dayBefore)
}
