package engine

import "sort"

// ruleBook 保存全部已注册规则版本。规则只追加不改写：
//   - 赛季在开启时绑定固定 RuleVersion，赛季内结算与领取口径恒定；
//   - 新版本规则通过新赛季（或指定 EffectiveFrom）生效，不影响旧赛季；
//   - 已发放奖励以领取时冻结的快照为准，规则更新永远不会追溯改写。
type ruleBook struct {
	byVersion map[RuleVersion]*RuleSet
}

func newRuleBook() *ruleBook {
	return &ruleBook{byVersion: map[RuleVersion]*RuleSet{}}
}

func (b *ruleBook) register(rs RuleSet) error {
	if rs.Version == "" {
		return ErrRuleNotFound
	}
	if _, exists := b.byVersion[rs.Version]; exists {
		return ErrRuleNotFound
	}
	cp := RuleSet{
		Version:       rs.Version,
		EffectiveFrom: rs.EffectiveFrom,
		Rewards:       make(map[int][]Reward, len(rs.Rewards)),
	}
	// 深拷贝奖励切片，杜绝外部在注册后篡改口径。
	levels := make([]int, 0, len(rs.Rewards))
	for lvl, rws := range rs.Rewards {
		if lvl < 1 {
			return ErrRuleNotFound
		}
		cp.Rewards[lvl] = append([]Reward(nil), rws...)
		levels = append(levels, lvl)
	}
	sort.Ints(levels)
	b.byVersion[rs.Version] = &cp
	return nil
}

func (b *ruleBook) get(v RuleVersion) (*RuleSet, error) {
	rs, ok := b.byVersion[v]
	if !ok {
		return nil, ErrRuleNotFound
	}
	return rs, nil
}

// rewardsFor 返回某等级奖励的拷贝，供领取时冻结快照。
func (b *ruleBook) rewardsFor(v RuleVersion, level int) ([]Reward, error) {
	rs, err := b.get(v)
	if err != nil {
		return nil, err
	}
	rws := rs.Rewards[level]
	if len(rws) == 0 {
		return nil, nil
	}
	return append([]Reward(nil), rws...), nil
}
