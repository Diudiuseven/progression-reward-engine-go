package engine

import (
	"fmt"
	"testing"
	"time"
)

func idf(format string, args ...any) string { return fmt.Sprintf(format, args...) }

// newTestEngine 只创建引擎；首次调用后由 bootTestData 装载规则与赛季，
// 重启场景再次 newTestEngine 时 restore 已能还原，无需重复注册。
func newTestEngine(t *testing.T, dir string) *Engine {
	t.Helper()
	e, err := New(Config{Dir: dir, ShardCount: 4, Clock: func() time.Time {
		return base.Add(48 * time.Hour)
	}})
	if err != nil {
		t.Fatalf("new engine: %v", err)
	}
	return e
}

func bootTestData(t *testing.T, e *Engine) {
	t.Helper()
	if _, err := e.rules.get("r1"); err == nil {
		return // 已恢复
	}
	rules := RuleSet{
		Version: "r1", EffectiveFrom: base,
		Rewards: map[int][]Reward{
			1: {{Code: "coin", Amount: 100}},
			2: {{Code: "gem", Amount: 10}},
			3: {{Code: "gem", Amount: 50}},
		},
	}
	if err := e.RegisterRules(rules); err != nil {
		t.Fatalf("register rules: %v", err)
	}
	s1 := SeasonConfig{
		ID: "s1", Start: base, End: base.Add(24 * time.Hour),
		CarryRatio: 0.5, LevelThresholds: []int64{100, 300, 600},
		RuleVersion: "r1",
		Decay:       DecayConfig{Window: time.Minute, Gamma: 0.5, MinFactor: 0.25},
	}
	if err := e.StartSeason(s1); err != nil {
		t.Fatalf("start s1: %v", err)
	}
}

func testEngine(t *testing.T, dir string) *Engine {
	e := newTestEngine(t, dir)
	bootTestData(t, e)
	return e
}
