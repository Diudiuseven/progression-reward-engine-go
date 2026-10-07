package engine

import (
	"fmt"
	"sort"
	"time"
)

// RuleSet 持有全部规则版本，提供按版本号与按生效时间两种查询。
// 规则不可变：注册后不再修改，保证历史赛季口径不被追溯改写。
type RuleSet struct {
	byVersion map[string]*RuleVersion
	ordered   []*RuleVersion // 按 EffectiveFrom 升序
}

func NewRuleSet(versions []RuleVersion) (*RuleSet, error) {
	rs := &RuleSet{byVersion: make(map[string]*RuleVersion, len(versions))}
	for i := range versions {
		v := versions[i] // 拷贝，避免外部后续修改
		if v.Version == "" {
			return nil, fmt.Errorf("rule version must not be empty")
		}
		if _, dup := rs.byVersion[v.Version]; dup {
			return nil, fmt.Errorf("duplicate rule version %q", v.Version)
		}
		if v.DayCap <= 0 || v.SeasonCap <= 0 {
			return nil, fmt.Errorf("rule %q: caps must be positive", v.Version)
		}
		// 阈值按 Required 升序排列
		sort.Slice(v.Levels, func(a, b int) bool { return v.Levels[a].Required < v.Levels[b].Required })
		rs.byVersion[v.Version] = &v
		rs.ordered = append(rs.ordered, &v)
	}
	sort.Slice(rs.ordered, func(a, b int) bool {
		return rs.ordered[a].EffectiveFrom.Before(rs.ordered[b].EffectiveFrom)
	})
	return rs, nil
}

// ByVersion 按版本号取规则。
func (rs *RuleSet) ByVersion(version string) (*RuleVersion, error) {
	v, ok := rs.byVersion[version]
	if !ok {
		return nil, fmt.Errorf("unknown rule version %q", version)
	}
	return v, nil
}

// EffectiveAt 取某时刻生效的最新版本（用于按指定时刻生效的新规则）。
func (rs *RuleSet) EffectiveAt(t time.Time) (*RuleVersion, error) {
	var best *RuleVersion
	for _, v := range rs.ordered {
		if !v.EffectiveFrom.After(t) {
			best = v
		} else {
			break
		}
	}
	if best == nil {
		return nil, fmt.Errorf("no rule effective at %s", t)
	}
	return best, nil
}

// LevelFor 依据阈值计算累计进度对应等级。
func LevelFor(rule *RuleVersion, progress float64) int {
	level := 0
	for _, th := range rule.Levels {
		if progress >= float64(th.Required) {
			level = th.Level
		} else {
			break
		}
	}
	return level
}

// RewardsUpTo 返回等级 <= level 的全部奖励（升序，结果确定）。
func RewardsUpTo(rule *RuleVersion, level int) []Reward {
	var out []Reward
	for _, r := range rule.Rewards {
		if r.Level <= level {
			out = append(out, r)
		}
	}
	sort.Slice(out, func(a, b int) bool {
		if out[a].Level != out[b].Level {
			return out[a].Level < out[b].Level
		}
		return out[a].RewardID < out[b].RewardID
	})
	return out
}
