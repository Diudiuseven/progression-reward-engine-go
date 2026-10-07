package engine

import (
	"testing"
	"time"
)

// 规则按生效时间生效；同一版本规则不可变，口径稳定。
func TestRuleEffectiveAt(t *testing.T) {
	rs, err := NewRuleSet(testRules())
	if err != nil {
		t.Fatal(err)
	}
	// t0 时刻只有 v1 生效
	v, err := rs.EffectiveAt(t0.Add(time.Hour))
	if err != nil || v.Version != "v1" {
		t.Fatalf("at t0+1h: %v %v", v, err)
	}
	// 10 天后 v2 生效
	v, err = rs.EffectiveAt(t0.Add(10 * 24 * time.Hour))
	if err != nil || v.Version != "v2" {
		t.Fatalf("at t0+10d: %v %v", v, err)
	}
	// 生效时间之前无任何规则
	if _, err = rs.EffectiveAt(t0.Add(-time.Hour)); err == nil {
		t.Fatal("expected no rule before t0")
	}
	// 同一份进度在同一版本下等级口径一致
	rule, _ := rs.ByVersion("v1")
	if LevelFor(rule, 300) != 2 || LevelFor(rule, 299.9) != 1 {
		t.Fatal("level thresholds unstable")
	}
	rw := RewardsUpTo(rule, 2)
	if len(rw) != 2 || rw[0].RewardID != "r1" || rw[1].RewardID != "r2" {
		t.Fatalf("rewards up to 2: %+v", rw)
	}
	// 重复版本号拒绝
	if _, err := NewRuleSet([]RuleVersion{testRules()[0], testRules()[0]}); err == nil {
		t.Fatal("duplicate version accepted")
	}
}
