package engine

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"
)

func TestClaimIdempotentAndConcurrent(t *testing.T) {
	e := testEngine(t, "")
	defer e.Close()
	ingest(t, e, Event{ID: "e1", PlayerID: "p", Amount: 400, OccurredAt: base.Add(time.Hour)})

	// 同一 claimID 并发重复领取：恰好一个首次结果，其余全部 AlreadyClaimed。
	var wg sync.WaitGroup
	var mu sync.Mutex
	first, dup := 0, 0
	var firstRewards []Reward
	for i := 0; i < 32; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			res, err := e.ClaimLevel(context.Background(), "p", "", 2, "claim-1")
			if err != nil {
				t.Errorf("claim: %v", err)
				return
			}
			mu.Lock()
			defer mu.Unlock()
			if res.AlreadyClaimed {
				dup++
			} else {
				first++
				firstRewards = res.Record.Rewards
			}
		}()
	}
	wg.Wait()
	if first != 1 || dup != 31 {
		t.Fatalf("first=%d dup=%d", first, dup)
	}
	if len(firstRewards) != 1 || firstRewards[0].Code != "gem" || firstRewards[0].Amount != 10 {
		t.Fatalf("reward snapshot wrong: %+v", firstRewards)
	}

	// 不同 claimID 再领同一等级：返回首次结果，不重复发放。
	res, err := e.ClaimLevel(context.Background(), "p", "", 2, "claim-2")
	if err != nil || !res.AlreadyClaimed || res.ClaimID != "claim-1" {
		t.Fatalf("second claim id: %+v err=%v", res, err)
	}
	v, _ := e.View("p")
	if len(v.ClaimedLevels) != 1 || v.ClaimedLevels[0] != 2 {
		t.Fatalf("claimed levels: %v", v.ClaimedLevels)
	}

	// 未达到的等级不可领。
	if _, err := e.ClaimLevel(context.Background(), "p", "", 3, "c3"); err != ErrLevelNotReached {
		t.Fatalf("want ErrLevelNotReached, got %v", err)
	}
}

func TestClaimBeforeLevel(t *testing.T) {
	e := testEngine(t, "")
	defer e.Close()
	ingest(t, e, Event{ID: "e", PlayerID: "p", Amount: 10, OccurredAt: base.Add(time.Hour)})
	if _, err := e.ClaimLevel(context.Background(), "p", "", 1, "c"); err != ErrLevelNotReached {
		t.Fatalf("got %v", err)
	}
}

func TestClaimSettledSeasonStillClaimable(t *testing.T) {
	e := testEngine(t, "")
	defer e.Close()
	ingest(t, e, Event{ID: "e1", PlayerID: "p", Amount: 200, OccurredAt: base.Add(time.Hour)}) // 等级 1
	if err := e.RegisterRules(RuleSet{
		Version: "r2", EffectiveFrom: base.Add(24 * time.Hour),
		Rewards: map[int][]Reward{1: {{Code: "coin", Amount: 9999}}},
	}); err != nil {
		t.Fatal(err)
	}
	s2 := SeasonConfig{
		ID: "s2", Start: base.Add(24 * time.Hour), End: base.Add(48 * time.Hour),
		CarryRatio: 0, LevelThresholds: []int64{100}, RuleVersion: "r2",
	}
	res, err := e.SwitchSeason(context.Background(), s2)
	if err != nil {
		t.Fatal(err)
	}
	if res.PlayerCount != 1 || res.PlayerSummaries[0].CarriedOver != 100 {
		t.Fatalf("settlement: %+v", res.PlayerSummaries)
	}
	// 旧赛季等级 1 的奖励仍可按旧版本规则领取，不被 r2 追溯改写。
	cr, err := e.ClaimLevel(context.Background(), "p", "s1", 1, "old-c")
	if err != nil {
		t.Fatal(err)
	}
	if rws := cr.Record.Rewards; rws[0].Amount != 100 || cr.Record.RuleVersion != "r1" {
		t.Fatalf("old reward rewritten: %+v", rws)
	}
	// 迟到的 s1 事件必须被拒绝，不能改写已结算结论。
	late := ingest(t, e, Event{ID: "late1", PlayerID: "p", Amount: 5000, OccurredAt: base.Add(2 * time.Hour)})
	if late.Accepted || late.Reason != "season_settled" {
		t.Fatalf("late event: %+v", late)
	}
}

func TestSeasonCarryOverAndAttribution(t *testing.T) {
	e := testEngine(t, "")
	defer e.Close()
	ingest(t, e, Event{ID: "a", PlayerID: "p", Amount: 200, OccurredAt: base.Add(time.Hour)})

	s2 := SeasonConfig{
		ID: "s2", Start: base.Add(24 * time.Hour), End: base.Add(48 * time.Hour),
		CarryRatio: 0.5, LevelThresholds: []int64{50, 200}, RuleVersion: "r1",
	}
	set, err := e.SwitchSeason(context.Background(), s2)
	if err != nil {
		t.Fatal(err)
	}
	if set.PlayerSummaries[0].CarriedOver != 100 {
		t.Fatalf("carry=%d", set.PlayerSummaries[0].CarriedOver)
	}
	// 结转种子 50 => 新赛季初始即等级 1（threshold 50）。
	// 切换窗口附近事件：s1 事件与 s2 事件按 OccurredAt 严格归属，互不摇摆。
	rOld := ingest(t, e, Event{ID: "old", PlayerID: "p", Amount: 10, OccurredAt: base.Add(23*time.Hour + 59*time.Minute)})
	if rOld.Accepted || rOld.Reason != "season_settled" {
		t.Fatalf("old window event %+v", rOld)
	}
	rNew := ingest(t, e, Event{ID: "new", PlayerID: "p", Amount: 100, OccurredAt: base.Add(24*time.Hour + time.Second)})
	if !rNew.Accepted || rNew.SeasonID != "s2" {
		t.Fatalf("new window event %+v", rNew)
	}
	v, _ := e.View("p")
	if v.SeasonID != "s2" || v.EffectiveProgress != 200 || v.Level != 2 {
		t.Fatalf("s2 view: %+v", v)
	}
}

func TestRuleVersionIsolation(t *testing.T) {
	e := testEngine(t, "")
	defer e.Close()
	// s1 内升级到等级 1 并领取 r1 奖励
	ingest(t, e, Event{ID: "a", PlayerID: "p", Amount: 100, OccurredAt: base.Add(time.Hour)})
	cr, err := e.ClaimLevel(context.Background(), "p", "s1", 1, "c1")
	if err != nil || cr.Record.RuleVersion != "r1" || cr.Record.Rewards[0].Amount != 100 {
		t.Fatalf("r1 claim: %+v %v", cr, err)
	}
	// 注册 r2：等级 1 奖励改成 999，等级 2 奖励取消
	if err := e.RegisterRules(RuleSet{
		Version: "r2", EffectiveFrom: base.Add(24 * time.Hour),
		Rewards: map[int][]Reward{1: {{Code: "coin", Amount: 999}}},
	}); err != nil {
		t.Fatal(err)
	}
	s2 := SeasonConfig{
		ID: "s2", Start: base.Add(24 * time.Hour), CarryRatio: 1,
		LevelThresholds: []int64{50}, RuleVersion: "r2",
	}
	if _, err := e.SwitchSeason(context.Background(), s2); err != nil {
		t.Fatal(err)
	}
	// s2 再领等级 1：按 r2 口径；s1 已发的 100 不受影响
	cr2, err := e.ClaimLevel(context.Background(), "p", "s2", 1, "c1-s2")
	if err != nil {
		t.Fatal(err)
	}
	if cr2.Record.RuleVersion != "r2" || cr2.Record.Rewards[0].Amount != 999 {
		t.Fatalf("r2 claim: %+v", cr2)
	}
	if cr.Record.Rewards[0].Amount != 100 {
		t.Fatal("historical reward mutated by rule update")
	}
}

func TestHighConcurrencyNoCrossContamination(t *testing.T) {
	e := testEngine(t, "")
	defer e.Close()
	const players = 64
	var wg sync.WaitGroup
	for i := 0; i < players; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			p := PlayerID(fmt.Sprintf("p%03d", i))
			for j := 0; j < 20; j++ {
				_, _ = e.Ingest(context.Background(), Event{
					ID: fmt.Sprintf("%s-e%d", p, j), PlayerID: p, Amount: 10,
					OccurredAt: base.Add(time.Hour + time.Duration(j)*2*time.Minute),
				})
			}
		}(i)
	}
	wg.Wait()
	for i := 0; i < players; i++ {
		p := PlayerID(fmt.Sprintf("p%03d", i))
		v, err := e.View(p)
		if err != nil {
			t.Fatal(err)
		}
		if v.RawProgress != 200 || v.EffectiveProgress != 200 {
			t.Fatalf("%s contaminated: raw=%d eff=%d", p, v.RawProgress, v.EffectiveProgress)
		}
	}
}

func TestClaimIDConflictingLevelReturnsFirst(t *testing.T) {
	e := testEngine(t, "")
	defer e.Close()
	ingest(t, e, Event{ID: "e", PlayerID: "p", Amount: 400, OccurredAt: base.Add(time.Hour)})
	r1, err := e.ClaimLevel(context.Background(), "p", "s1", 1, "same-id")
	if err != nil {
		t.Fatal(err)
	}
	if r1.AlreadyClaimed {
		t.Fatal("first claim should not be already")
	}
	// 同一 claimID 改领等级 2：必须返回首次（等级1）结果，不能发第二份。
	r2, err := e.ClaimLevel(context.Background(), "p", "s1", 2, "same-id")
	if err != nil || r2.Level != 1 || !r2.AlreadyClaimed {
		t.Fatalf("conflict: %+v err=%v", r2, err)
	}
	v, _ := e.View("p")
	if len(v.ClaimedLevels) != 1 {
		t.Fatalf("double grant via conflicting id: %v", v.ClaimedLevels)
	}
}
