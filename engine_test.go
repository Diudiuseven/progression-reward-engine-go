package engine

import (
	"fmt"
	"sync"
	"testing"
	"time"
)

var t0 = time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

func approxEq(a, b float64) bool {
	d := a - b
	if d < 0 {
		d = -d
	}
	return d < 1e-6
}

func testRules() []RuleVersion {
	return []RuleVersion{
		{
			Version:       "v1",
			EffectiveFrom: t0,
			Levels:        []LevelThreshold{{Level: 1, Required: 100}, {Level: 2, Required: 300}, {Level: 3, Required: 600}},
			Rewards:       []Reward{{Level: 1, RewardID: "r1", Amount: 10}, {Level: 2, RewardID: "r2", Amount: 20}, {Level: 3, RewardID: "r3", Amount: 30}},
			DayCap:        100,
			DayKnee:       50,
			SeasonCap:     1000,
		},
		{
			Version:       "v2",
			EffectiveFrom: t0.Add(10 * 24 * time.Hour),
			Levels:        []LevelThreshold{{Level: 1, Required: 50}, {Level: 2, Required: 200}},
			Rewards:       []Reward{{Level: 1, RewardID: "s2r1", Amount: 11}, {Level: 2, RewardID: "s2r2", Amount: 22}},
			DayCap:        100,
			DayKnee:       50,
			SeasonCap:     1000,
		},
	}
}

func testSeasons() []SeasonConfig {
	return []SeasonConfig{
		{SeasonID: "S1", Start: t0, End: t0.Add(10 * 24 * time.Hour), CarryRatio: 0.5, RuleVersion: "v1"},
		{SeasonID: "S2", Start: t0.Add(10 * 24 * time.Hour), End: t0.Add(20 * 24 * time.Hour), CarryRatio: 0.5, RuleVersion: "v2"},
	}
}

func openTest(t *testing.T, dir string, now time.Time) *Engine {
	t.Helper()
	e, err := Open(Config{
		DataDir:          dir,
		Rules:            testRules(),
		Seasons:          testSeasons(),
		MaxCachedPlayers: 8,
		Now:              func() time.Time { return now },
	})
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	return e
}

func ev(id, player string, pts int64, at time.Time) Event {
	return Event{ID: id, PlayerID: player, Type: "play", Points: pts, EventTime: at}
}

// 重复事件不得重复累计；乱序到达当日总收益与顺序无关（可复现）。
func TestDuplicateAndOutOfOrder(t *testing.T) {
	dir := t.TempDir()
	e := openTest(t, dir, t0.Add(time.Hour))
	defer e.Close()

	r1, err := e.Ingest(ev("e1", "p1", 30, t0.Add(time.Hour)))
	if err != nil || r1.Reason != "ok" && r1.Reason != "decayed" {
		t.Fatalf("first ingest: %+v err=%v", r1, err)
	}
	r2, _ := e.Ingest(ev("e1", "p1", 30, t0.Add(time.Hour)))
	if r2.Reason != "duplicate" || r2.Effective != 0 {
		t.Fatalf("dup not ignored: %+v", r2)
	}
	v1, _ := e.View("p1")

	// 另一玩家以相反顺序摄入同样两笔，最终进度应一致
	e.Ingest(ev("a", "p2", 30, t0.Add(time.Hour)))
	e.Ingest(ev("b", "p2", 40, t0.Add(2*time.Hour)))
	e.Ingest(ev("b", "p3", 40, t0.Add(2*time.Hour)))
	e.Ingest(ev("a", "p3", 30, t0.Add(time.Hour)))
	v2, _ := e.View("p2")
	v3, _ := e.View("p3")
	if !approxEq(v2.SeasonProgress, v3.SeasonProgress) {
		t.Fatalf("order changed result: %v vs %v", v2.SeasonProgress, v3.SeasonProgress)
	}
	// p1 只计了一笔 30
	e.Ingest(ev("c", "p1", 40, t0.Add(2*time.Hour)))
	v1b, _ := e.View("p1")
	if !approxEq(v1b.SeasonProgress, v2.SeasonProgress) {
		t.Fatalf("duplicate polluted progress: p1=%v p2=%v (before dup check v1=%v)", v1b.SeasonProgress, v2.SeasonProgress, v1.SeasonProgress)
	}
	// 玩家之间互不污染
	if v1b.SeasonProgress != 0 && v2.SeasonProgress == 0 {
		t.Fatal("cross-player pollution")
	}
}

// 日收益递减并封顶：同日高频事件的有效收益单调不增，日总有效收益封顶 DayCap。
func TestDayCapAndDecay(t *testing.T) {
	dir := t.TempDir()
	e := openTest(t, dir, t0.Add(time.Hour))
	defer e.Close()

	var last = 1e18
	for i := 0; i < 50; i++ {
		r, err := e.Ingest(ev(fmt.Sprintf("d%d", i), "p1", 10, t0.Add(time.Hour)))
		if err != nil {
			t.Fatal(err)
		}
		if r.Effective > last {
			t.Fatalf("marginal gain increased: %v > %v", r.Effective, last)
		}
		last = r.Effective
	}
	v, _ := e.View("p1")
	if v.SeasonProgress > 100 {
		t.Fatalf("day cap exceeded: %v", v.SeasonProgress)
	}
	if v.SeasonProgress < 90 {
		t.Fatalf("expected to approach day cap, got %v", v.SeasonProgress)
	}
	// 第二天（事件时间）重新计数
	r, _ := e.Ingest(ev("next-day", "p1", 10, t0.Add(26*time.Hour)))
	if r.Effective <= last {
		t.Fatalf("new day should reset decay, got %v (last=%v)", r.Effective, last)
	}
}

// 赛季硬上限。
func TestSeasonCap(t *testing.T) {
	dir := t.TempDir()
	rules := testRules()
	rules[0].DayCap = 200 // 日上限 200，10 天可打满赛季上限 1000
	rules[1].DayCap = 200
	e, err := Open(Config{
		DataDir: dir, Rules: rules, Seasons: testSeasons(),
		Now: func() time.Time { return t0.Add(time.Hour) },
	})
	if err != nil {
		t.Fatal(err)
	}
	defer e.Close()
	for d := 0; d < 10; d++ {
		for i := 0; i < 30; i++ {
			e.Ingest(ev(fmt.Sprintf("s-%d-%d", d, i), "p1", 10, t0.Add(time.Duration(d)*24*time.Hour+time.Hour)))
		}
	}
	v, _ := e.View("p1")
	if !approxEq(v.SeasonProgress, 1000) {
		t.Fatalf("expected exactly season cap 1000, got %v", v.SeasonProgress)
	}
	r, _ := e.Ingest(ev("overflow", "p1", 10, t0.Add(9*24*time.Hour+2*time.Hour)))
	if r.Effective != 0 || r.Reason != "season_cap" {
		t.Fatalf("overflow should be rejected, got %+v", r)
	}
}

// 赛季切换：结转比例生效、迟到事件归属正确、新事件计入新赛季。
func TestSeasonSwitchCarryAndLate(t *testing.T) {
	dir := t.TempDir()
	now := t0.Add(time.Hour)
	e := openTest(t, dir, now)

	for d := 0; d < 4; d++ {
		for i := 0; i < 30; i++ {
			e.Ingest(ev(fmt.Sprintf("e-%d-%d", d, i), "p1", 10, t0.Add(time.Duration(d)*24*time.Hour+time.Hour)))
		}
	}
	v, _ := e.View("p1")
	// 每天 300 原始 -> D(300)=100*(1-e^-6)≈99.7521，4 天 ≈ 399.0083
	if !approxEq(v.SeasonProgress, 399.0084991293333) {
		t.Fatalf("pre-switch progress %v", v.SeasonProgress)
	}

	// 推进到 S2
	now = t0.Add(11 * 24 * time.Hour)
	if _, err := e.SwitchSeason(now); err != nil {
		t.Fatal(err)
	}
	v, _ = e.View("p1")
	if v.SeasonID != "S2" || !approxEq(v.SeasonProgress, 199.50424956466666) {
		t.Fatalf("carry wrong: %+v", v)
	}

	// 迟到事件（事件时间在 S1）不得改写已结算赛季，也不得计入 S2
	r, _ := e.Ingest(ev("late-1", "p1", 50, t0.Add(2*24*time.Hour)))
	if r.Reason != "late" || r.Effective != 0 {
		t.Fatalf("late event not rejected: %+v", r)
	}
	v, _ = e.View("p1")
	if !approxEq(v.SeasonProgress, 199.50424956466666) {
		t.Fatalf("late event rewrote state: %+v", v)
	}

	// 切换瞬间到达、但事件时间属于 S2 的事件正常计入 S2
	r, _ = e.Ingest(ev("new-1", "p1", 10, t0.Add(11*24*time.Hour+time.Minute)))
	if r.SeasonID != "S2" || r.Effective <= 0 {
		t.Fatalf("new season event misattributed: %+v", r)
	}
	e.Close()
}

// 规则版本化：赛季绑定规则版本，口径稳定；已发放奖励不被规则更新追溯。
func TestRuleVersioning(t *testing.T) {
	dir := t.TempDir()
	now := t0.Add(time.Hour)
	e := openTest(t, dir, now)

	// S1 内升到 1 级（阈值 100；单日有效收益渐近 100，需跨两天）
	for i := 0; i < 30; i++ {
		e.Ingest(ev(fmt.Sprintf("r-%d", i), "p1", 10, t0.Add(time.Hour)))
	}
	for i := 0; i < 30; i++ {
		e.Ingest(ev(fmt.Sprintf("r-d2-%d", i), "p1", 10, t0.Add(25*time.Hour)))
	}
	c1, err := e.Claim("p1")
	if err != nil {
		t.Fatal(err)
	}
	if len(c1.Granted) != 1 || c1.Granted[0].RewardID != "r1" {
		t.Fatalf("S1 claim: %+v", c1)
	}
	// 切到 S2（绑定 v2，奖励 ID 不同）
	now = t0.Add(11 * 24 * time.Hour)
	e.SwitchSeason(now)
	// 已发放的 r1 不得被追溯改写：再次领取不含 r1 的重复发放
	for i := 0; i < 30; i++ {
		e.Ingest(ev(fmt.Sprintf("r2-%d", i), "p1", 10, now.Add(time.Hour)))
	}
	c2, _ := e.Claim("p1")
	for _, g := range c2.Granted {
		if g.RewardID == "r1" {
			t.Fatalf("old season reward re-granted: %+v", c2)
		}
	}
	if len(c2.Granted) == 0 || c2.Granted[0].RewardID != "s2r1" {
		t.Fatalf("S2 claim should use v2 rules: %+v", c2)
	}
	// 同一赛季内规则口径稳定：重复视图等级一致
	v1, _ := e.View("p1")
	v2, _ := e.View("p1")
	if v1.Level != v2.Level || v1.RuleVersion != "v2" {
		t.Fatalf("unstable view: %+v vs %+v", v1, v2)
	}
	e.Close()
}

// 领取幂等与并发：并发重复领取，每个奖励只被发放一次；领取与结算并发时账目自洽。
func TestClaimIdempotentConcurrent(t *testing.T) {
	dir := t.TempDir()
	e := openTest(t, dir, t0.Add(time.Hour))
	defer e.Close()

	for d := 0; d < 7; d++ {
		for i := 0; i < 30; i++ {
			e.Ingest(ev(fmt.Sprintf("c-%d-%d", d, i), "p1", 10, t0.Add(time.Duration(d)*24*time.Hour+time.Hour)))
		}
	}
	const n = 32
	results := make([]ClaimResult, n)
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			r, err := e.Claim("p1")
			if err != nil {
				t.Error(err)
			}
			results[i] = r
		}(i)
	}
	wg.Wait()
	granted := map[string]int{}
	for _, r := range results {
		for _, g := range r.Granted {
			granted[g.RewardID]++
		}
	}
	if len(granted) == 0 {
		t.Fatal("nothing granted")
	}
	for id, cnt := range granted {
		if cnt != 1 {
			t.Fatalf("reward %s granted %d times", id, cnt)
		}
	}
	// 再次领取：Granted 为空，Already 为完整口径
	again, _ := e.Claim("p1")
	if len(again.Granted) != 0 || len(again.Already) != len(granted) {
		t.Fatalf("re-claim not idempotent: %+v", again)
	}

	// 领取与摄入并发：最终视图必须自洽（进度单调、等级与进度匹配）
	var wg2 sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg2.Add(1)
		go func(i int) {
			defer wg2.Done()
			for j := 0; j < 20; j++ {
				e.Ingest(ev(fmt.Sprintf("mix-%d-%d", i, j), "p2", 5, t0.Add(8*24*time.Hour+time.Hour)))
			}
		}(i)
	}
	for i := 0; i < 4; i++ {
		wg2.Add(1)
		go func() {
			defer wg2.Done()
			for j := 0; j < 10; j++ {
				e.Claim("p2")
				e.View("p2")
			}
		}()
	}
	wg2.Wait()
	v, _ := e.View("p2")
	rule, _ := e.rules.ByVersion("v1")
	if LevelFor(rule, v.SeasonProgress) != v.Level {
		t.Fatalf("inconsistent view: %+v", v)
	}
}

// 重启恢复：进度、等级、已发放奖励、去重集合全部保留；重复事件仍被去重。
func TestRestartRecovery(t *testing.T) {
	dir := t.TempDir()
	now := t0.Add(time.Hour)
	e := openTest(t, dir, now)
	for i := 0; i < 30; i++ {
		e.Ingest(ev(fmt.Sprintf("k-%d", i), "p1", 10, t0.Add(time.Hour)))
	}
	before, _ := e.View("p1")
	c1, _ := e.Claim("p1")
	e.Close()

	// 重启
	e2 := openTest(t, dir, now)
	defer e2.Close()
	after, _ := e2.View("p1")
	if !approxEq(after.SeasonProgress, before.SeasonProgress) || after.Level != before.Level {
		t.Fatalf("progress jumped after restart: before=%+v after=%+v", before, after)
	}
	// 重复事件仍被去重
	r, _ := e2.Ingest(ev("k-0", "p1", 10, t0.Add(time.Hour)))
	if r.Reason != "duplicate" {
		t.Fatalf("dedup lost after restart: %+v", r)
	}
	// 已发放奖励不重复发放
	c2, _ := e2.Claim("p1")
	if len(c2.Granted) != 0 || len(c2.Already) != len(c1.Granted) {
		t.Fatalf("re-grant after restart: %+v", c2)
	}
	// 新事件从已提交进度继续累计
	r, _ = e2.Ingest(ev("k-new", "p1", 10, t0.Add(2*time.Hour)))
	if r.Effective <= 0 || !approxEq(r.SeasonTotalAfter, after.SeasonProgress+r.Effective) {
		t.Fatalf("resume from committed progress failed: %+v view=%+v", r, after)
	}
}

// 崩溃在赛季切换两阶段提交之间：恢复时重做切换，赛季归属不摇摆。
func TestPendingSwitchRedo(t *testing.T) {
	dir := t.TempDir()
	now := t0.Add(time.Hour)
	e := openTest(t, dir, now)
	for i := 0; i < 30; i++ {
		e.Ingest(ev(fmt.Sprintf("w-%d", i), "p1", 10, t0.Add(time.Hour)))
	}
	e.Close()

	// 手工制造崩溃现场：写入 begin 但不写 commit、也不更新 season.json
	store, err := OpenStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	settled := []string{"S1"}
	store.AppendSwitch(SwitchRecord{Kind: switchBegin, SwitchID: "crash-1", To: "S2", Settled: settled})
	store.Close()

	now = t0.Add(11 * 24 * time.Hour)
	e2 := openTest(t, dir, now)
	defer e2.Close()
	v, _ := e2.View("p1")
	if v.SeasonID != "S2" {
		t.Fatalf("pending switch not redone: %+v", v)
	}
	// D(300)*0.5 ≈ 49.876 结转
	if !approxEq(v.SeasonProgress, 49.876062391166684) {
		t.Fatalf("carry after redo wrong: %+v", v)
	}
	// S1 已结算，迟到事件被拒绝
	r, _ := e2.Ingest(ev("late", "p1", 10, t0.Add(time.Hour)))
	if r.Reason != "late" {
		t.Fatalf("late after redo: %+v", r)
	}
}

// 多玩家高并发：互不进度的污染，且缓存有界（内存不随玩家数持续增长）。
func TestManyPlayersConcurrentAndBoundedCache(t *testing.T) {
	dir := t.TempDir()
	e := openTest(t, dir, t0.Add(time.Hour))
	defer e.Close()

	const players = 40
	var wg sync.WaitGroup
	for p := 0; p < players; p++ {
		wg.Add(1)
		go func(p int) {
			defer wg.Done()
			pid := fmt.Sprintf("u%d", p)
			for i := 0; i < 20; i++ {
				if _, err := e.Ingest(ev(fmt.Sprintf("m-%d-%d", p, i), pid, 5, t0.Add(time.Hour))); err != nil {
					t.Error(err)
				}
			}
		}(p)
	}
	wg.Wait()
	// 每个玩家进度一致且互不污染
	var expect = -1.0
	for p := 0; p < players; p++ {
		v, err := e.View(fmt.Sprintf("u%d", p))
		if err != nil {
			t.Fatal(err)
		}
		if expect < 0 {
			expect = v.SeasonProgress
		}
		if !approxEq(v.SeasonProgress, expect) {
			t.Fatalf("player %d progress %v != %v", p, v.SeasonProgress, expect)
		}
	}
	// LRU 缓存不超过上限
	e.cacheMu.Lock()
	n := e.lru.Len()
	e.cacheMu.Unlock()
	if n > 8 {
		t.Fatalf("cache grew beyond limit: %d", n)
	}
	// 被逐出的玩家状态仍可正确读回（从磁盘）
	v, _ := e.View("u0")
	if !approxEq(v.SeasonProgress, expect) {
		t.Fatalf("evicted player lost state: %+v", v)
	}
}

// 衰减曲线本身的性质：边际递减、接近日上限、确定性。
func TestDecayCurveProperties(t *testing.T) {
	rule := &RuleVersion{DayCap: 100, DayKnee: 50, SeasonCap: 10000}
	prev := 1e18
	var total float64
	for i := 0; i < 200; i++ {
		g, _ := computeGain(rule, float64(i)*10, total, 10)
		if g > prev+1e-9 { // 容忍浮点舍入抖动
			t.Fatalf("not diminishing at i=%d: %v > %v", i, g, prev)
		}
		prev = g
		total += g
	}
	if total > 100 {
		t.Fatalf("exceeded day cap: %v", total)
	}
	// 确定性：同样输入同样输出
	a, _ := computeGain(rule, 40, 0, 10)
	b, _ := computeGain(rule, 40, 0, 10)
	if a != b {
		t.Fatalf("non-deterministic: %v vs %v", a, b)
	}
}
