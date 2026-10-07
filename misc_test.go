package engine

import (
	"context"
	"testing"
	"time"
)

func TestInvalidEventRejected(t *testing.T) {
	e := testEngine(t, "")
	defer e.Close()
	cases := []Event{
		{ID: "", PlayerID: "p", Amount: 1, OccurredAt: base},
		{ID: "x", PlayerID: "", Amount: 1, OccurredAt: base},
		{ID: "x", PlayerID: "p", Amount: -1, OccurredAt: base},
		{ID: "x", PlayerID: "p", Amount: 1, OccurredAt: time.Time{}},
	}
	for i, ev := range cases {
		if _, err := e.Ingest(context.Background(), ev); err != ErrEventInvalid {
			t.Fatalf("case %d: %v", i, err)
		}
	}
	// 时间不落在任何赛季：返回 season_not_found 结论而非错误
	r, err := e.Ingest(context.Background(), Event{ID: "x", PlayerID: "p", Amount: 1, OccurredAt: base.Add(-time.Hour)})
	if err != nil || r.Accepted || r.Reason != "season_not_found" {
		t.Fatalf("out of range: %+v err=%v", r, err)
	}
}

func TestOverlappingSeasonRejected(t *testing.T) {
	e := testEngine(t, "")
	defer e.Close()
	bad := SeasonConfig{ID: "sX", Start: base.Add(time.Hour), End: base.Add(2 * time.Hour),
		RuleVersion: "r1", LevelThresholds: []int64{1}}
	if err := e.StartSeason(bad); err == nil {
		t.Fatal("overlapping season accepted")
	}
}

func TestRuleVersionMustExist(t *testing.T) {
	e := testEngine(t, "")
	defer e.Close()
	bad := SeasonConfig{ID: "sX", Start: base.Add(48 * time.Hour), RuleVersion: "nope",
		LevelThresholds: []int64{1}}
	if err := e.StartSeason(bad); err != ErrRuleNotFound {
		t.Fatalf("got %v", err)
	}
}

func TestClaimAfterEngineClose(t *testing.T) {
	e := testEngine(t, "")
	if err := e.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := e.Ingest(context.Background(), Event{ID: "x", PlayerID: "p", Amount: 1, OccurredAt: base}); err != ErrEngineClosed {
		t.Fatal("ingest after close")
	}
	if _, err := e.ClaimLevel(context.Background(), "p", "s1", 1, "c"); err != ErrEngineClosed {
		t.Fatal("claim after close")
	}
	if err := e.Snapshot(); err != ErrEngineClosed {
		t.Fatal("snapshot after close")
	}
}

func TestNoCarrySeasonStartsFresh(t *testing.T) {
	e, err := New(Config{Dir: "", ShardCount: 2})
	if err != nil {
		t.Fatal(err)
	}
	defer e.Close()
	if err := e.RegisterRules(RuleSet{Version: "r1", Rewards: map[int][]Reward{1: {{Code: "c", Amount: 1}}}}); err != nil {
		t.Fatal(err)
	}
	s1 := SeasonConfig{ID: "s1", Start: base, End: base.Add(24 * time.Hour), CarryRatio: 0,
		LevelThresholds: []int64{100}, RuleVersion: "r1"}
	if err := e.StartSeason(s1); err != nil {
		t.Fatal(err)
	}
	ingest(t, e, Event{ID: "e", PlayerID: "p", Amount: 600, OccurredAt: base.Add(time.Hour)})
	s2 := SeasonConfig{ID: "s2", Start: base.Add(24 * time.Hour), CarryRatio: 0,
		LevelThresholds: []int64{100}, RuleVersion: "r1"}
	set, err := e.SwitchSeason(context.Background(), s2)
	if err != nil {
		t.Fatal(err)
	}
	if set.PlayerSummaries[0].CarriedOver != 0 {
		t.Fatalf("carry=%d", set.PlayerSummaries[0].CarriedOver)
	}
	v, _ := e.View("p")
	if v.EffectiveProgress != 0 || v.Level != 0 {
		t.Fatalf("fresh season: %+v", v)
	}
}
