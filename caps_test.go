package engine

import (
	"context"
	"testing"
	"time"
)

func cappedEngine(t *testing.T) *Engine {
	t.Helper()
	e, err := New(Config{Dir: "", ShardCount: 2, Clock: func() time.Time { return base.Add(time.Hour) }})
	if err != nil {
		t.Fatal(err)
	}
	if err := e.RegisterRules(RuleSet{Version: "r1", Rewards: map[int][]Reward{1: {{Code: "c", Amount: 1}}}}); err != nil {
		t.Fatal(err)
	}
	cfg := SeasonConfig{
		ID: "s1", Start: base, End: base.Add(7 * 24 * time.Hour),
		CarryRatio: 0, LevelThresholds: []int64{1000}, RuleVersion: "r1",
		Decay:    DecayConfig{Window: 10 * time.Second, Gamma: 0.5, MinFactor: 0.25},
		DailyCap: 100, SeasonCap: 150,
	}
	if err := e.StartSeason(cfg); err != nil {
		t.Fatal(err)
	}
	return e
}

func TestFrequencyDecaySmooth(t *testing.T) {
	e := noCapEngine(t)
	defer e.Close()
	// 同一玩家 0..3 秒内连发 4 条 Amount=100：因子 1,.5,.25,.25 => 100,50,25,25
	want := []int64{100, 50, 25, 25}
	var total int64
	for i := 0; i < 4; i++ {
		r, err := e.Ingest(context.Background(), Event{
			ID: idf("f%d", i), PlayerID: "p", Amount: 100,
			OccurredAt: base.Add(time.Duration(i) * time.Second),
		})
		if err != nil {
			t.Fatal(err)
		}
		if r.Breakdown.Granted != want[i] {
			t.Fatalf("event %d granted=%d want %d, factor=%v", i, r.Breakdown.Granted, want[i], r.Breakdown.FrequencyFactor)
		}
		total += r.Breakdown.Granted
	}
	if total != 200 {
		t.Fatalf("total=%d want 200", total)
	}
}

func TestDailyAndSeasonCap(t *testing.T) {
	e := cappedEngine(t)
	defer e.Close()
	send := func(id string, dayOff time.Duration, amt int64) *IngestResult {
		r, err := e.Ingest(context.Background(), Event{
			ID: id, PlayerID: "p", Amount: amt, OccurredAt: base.Add(dayOff),
		})
		if err != nil {
			t.Fatal(err)
		}
		return r
	}
	// 事件间隔超过 10s 窗口 => 无递减，单独考察封顶链路。
	// 第 0 天：80 入账（日限余 20）；再来 50 => 只入账 20，DailyCut=30
	r := send("d1a", 0, 80)
	if r.Breakdown.Granted != 80 {
		t.Fatalf("d1a %d", r.Breakdown.Granted)
	}
	r = send("d1b", 30*time.Second, 50)
	if r.Breakdown.Granted != 20 || r.Breakdown.DailyCut != 30 {
		t.Fatalf("d1b granted=%d cut=%d", r.Breakdown.Granted, r.Breakdown.DailyCut)
	}
	// 同日再来 10：日限已满 => 0
	r = send("d1c", 60*time.Second, 10)
	if r.Breakdown.Granted != 0 || r.Breakdown.DailyCut != 10 {
		t.Fatalf("d1c granted=%d dailycut=%d", r.Breakdown.Granted, r.Breakdown.DailyCut)
	}
	// 次日：日限重置；赛季累计 100，赛季上限 150 => 仅可再入 50
	r = send("d2a", 24*time.Hour+time.Second, 80)
	if r.Breakdown.Granted != 50 || r.Breakdown.SeasonCut != 30 || r.Breakdown.DailyCut != 0 {
		t.Fatalf("d2a granted=%d seasoncut=%d dailycut=%d", r.Breakdown.Granted, r.Breakdown.SeasonCut, r.Breakdown.DailyCut)
	}
	// 第三天：赛季已封顶 => 0
	r = send("d3a", 48*time.Hour+time.Second, 40)
	if r.Breakdown.Granted != 0 || r.Breakdown.SeasonCut != 40 {
		t.Fatalf("d3a granted=%d seasoncut=%d", r.Breakdown.Granted, r.Breakdown.SeasonCut)
	}
	v, _ := e.View("p")
	if v.SeasonGranted != 150 {
		t.Fatalf("season granted=%d want 150", v.SeasonGranted)
	}
}

func noCapEngine(t *testing.T) *Engine {
	e, err := New(Config{Dir: "", ShardCount: 2})
	if err != nil {
		t.Fatal(err)
	}
	if err := e.RegisterRules(RuleSet{Version: "r1", Rewards: map[int][]Reward{1: {{Code: "c", Amount: 1}}}}); err != nil {
		t.Fatal(err)
	}
	cfg := SeasonConfig{
		ID: "s1", Start: base, End: base.Add(30 * 24 * time.Hour),
		LevelThresholds: []int64{100000}, RuleVersion: "r1",
		Decay: DecayConfig{Window: 10 * time.Second, Gamma: 0.5, MinFactor: 0.25},
	}
	if err := e.StartSeason(cfg); err != nil {
		t.Fatal(err)
	}
	return e
}

func TestDecayDeterminismNoCap(t *testing.T) {
	// 高频递减定义在“事件到达流”上：相同到达顺序必须产出逐字段一致、可复现的结果。
	events := []Event{
		{ID: "x0", PlayerID: "p", Amount: 100, OccurredAt: base.Add(0 * time.Second)},
		{ID: "x1", PlayerID: "p", Amount: 100, OccurredAt: base.Add(3 * time.Second)},
		{ID: "x2", PlayerID: "p", Amount: 100, OccurredAt: base.Add(6 * time.Second)},
	}
	run := func() []GainBreakdown {
		e := noCapEngine(t)
		defer e.Close()
		var bs []GainBreakdown
		for _, ev := range events {
			r, err := e.Ingest(context.Background(), ev)
			if err != nil {
				t.Fatal(err)
			}
			bs = append(bs, *r.Breakdown)
		}
		return bs
	}
	a, b := run(), run()
	if len(a) != len(b) {
		t.Fatal("breakdown count differs")
	}
	for i := range a {
		if a[i] != b[i] {
			t.Fatalf("non-reproducible breakdown at %d: %+v vs %+v", i, a[i], b[i])
		}
	}
	// 乱序到达不改变“事件去重/归属/最终原始累计”这些不变量。
	e := noCapEngine(t)
	defer e.Close()
	for i := 2; i >= 0; i-- {
		ingest(t, e, events[i])
	}
	v, _ := e.View("p")
	if v.RawProgress != 300 {
		t.Fatalf("raw progress after out-of-order ingest = %d", v.RawProgress)
	}
	// 三个事件全部已入账，无丢失
	if len(v.DailyGranted) == 0 {
		t.Fatal("daily buckets missing")
	}
}
