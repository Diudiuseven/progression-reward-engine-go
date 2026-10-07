package engine

import (
	"context"
	"testing"
	"time"
)

func TestEvictSettledReleasesMemory(t *testing.T) {
	e := testEngine(t, "")
	defer e.Close()
	ingest(t, e, Event{ID: "e1", PlayerID: "p", Amount: 100, OccurredAt: base.Add(time.Hour)})
	s2 := SeasonConfig{ID: "s2", Start: base.Add(24 * time.Hour), CarryRatio: 0,
		LevelThresholds: []int64{100}, RuleVersion: "r1"}
	if _, err := e.SwitchSeason(context.Background(), s2); err != nil {
		t.Fatal(err)
	}
	// 淘汰前：s1 账本仍可领取（旧赛季奖励保留）。
	if _, err := e.ClaimLevel(context.Background(), "p", "s1", 1, "c1"); err != nil {
		t.Fatalf("claim before evict: %v", err)
	}
	if err := e.EvictSettled("s1"); err != nil {
		t.Fatal(err)
	}
	for _, sh := range e.shards {
		sh.mu.RLock()
		if p := sh.get("p", "s1"); p != nil {
			t.Fatal("s1 ledger survived eviction")
		}
		if len(sh.dedup.sets) > 0 {
			t.Fatal("dedup set survived eviction")
		}
		sh.mu.RUnlock()
	}
}

func TestJanitorKeepsBoundedSeasons(t *testing.T) {
	// RetainSettled=2：保留最近两个已结束赛季；更早的 s1 被自动回收。
	e, err := New(Config{Dir: "", ShardCount: 2, RetainSettled: 2,
		EvictInterval: 20 * time.Millisecond,
		Clock:         func() time.Time { return base.Add(100 * 24 * time.Hour) }})
	if err != nil {
		t.Fatal(err)
	}
	defer e.Close()
	if err := e.RegisterRules(RuleSet{Version: "r1", Rewards: map[int][]Reward{1: {{Code: "c", Amount: 1}}}}); err != nil {
		t.Fatal(err)
	}
	mk := func(id string, day int) SeasonConfig {
		return SeasonConfig{ID: SeasonID(id), Start: base.Add(time.Duration(day) * 24 * time.Hour),
			End: base.Add(time.Duration(day+1) * 24 * time.Hour), CarryRatio: 0,
			LevelThresholds: []int64{100}, RuleVersion: "r1"}
	}
	if err := e.StartSeason(mk("s1", 0)); err != nil {
		t.Fatal(err)
	}
	ingest(t, e, Event{ID: "e", PlayerID: "p", Amount: 100, OccurredAt: base.Add(time.Hour)})
	if _, err := e.SwitchSeason(context.Background(), mk("s2", 1)); err != nil {
		t.Fatal(err)
	}
	ingest(t, e, Event{ID: "e2", PlayerID: "p", Amount: 100, OccurredAt: base.Add(25 * time.Hour)})
	if _, err := e.SwitchSeason(context.Background(), mk("s3", 2)); err != nil {
		t.Fatal(err)
	}
	ingest(t, e, Event{ID: "e3", PlayerID: "p", Amount: 100, OccurredAt: base.Add(49 * time.Hour)})

	// 等待 janitor 多轮。
	time.Sleep(150 * time.Millisecond)
	var s1, s2, s3 bool
	for _, sh := range e.shards {
		sh.mu.RLock()
		if p := sh.get("p", "s1"); p != nil {
			s1 = true
		}
		if p := sh.get("p", "s2"); p != nil {
			s2 = true
		}
		if p := sh.get("p", "s3"); p != nil {
			s3 = true
		}
		sh.mu.RUnlock()
	}
	if s1 {
		t.Fatal("s1 should have been evicted (beyond retain window)")
	}
	if !s2 {
		t.Fatal("s2 should be retained")
	}
	if !s3 {
		t.Fatal("s3 current ledgers missing")
	}
}
