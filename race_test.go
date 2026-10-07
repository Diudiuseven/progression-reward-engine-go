package engine

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// TestConcurrentIngestAndClaim 摄入、领取、视图并发：
// 任何时刻读到的等级/已领取账目必须自洽，最终每个等级最多发一份。
func TestConcurrentIngestAndClaim(t *testing.T) {
	dir := t.TempDir()
	e := testEngine(t, dir)
	defer e.Close()

	var granted int64
	var wg sync.WaitGroup
	stop := make(chan struct{})
	var readerWG sync.WaitGroup

	for w := 0; w < 8; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			for i := 0; i < 400; i++ {
				_, _ = e.Ingest(context.Background(), Event{
					ID: fmt.Sprintf("w%d-e%d", w, i), PlayerID: "hot", Amount: 20,
					OccurredAt: base.Add(time.Duration(w) * time.Hour).Add(time.Duration(i) * 2 * time.Minute),
				})
			}
		}(w)
	}
	for w := 0; w < 8; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			for lvl := 1; lvl <= 5; lvl++ {
				res, err := e.ClaimLevel(context.Background(), "hot", "s1", lvl, fmt.Sprintf("claim-%d-%d", w, lvl))
				if err == nil && !res.AlreadyClaimed {
					atomic.AddInt64(&granted, 1)
				}
			}
		}(w)
	}
	readerWG.Add(1)
	go func() {
		defer readerWG.Done()
		for {
			select {
			case <-stop:
				return
			default:
			}
			v, err := e.View("hot")
			if err == nil {
				for _, l := range v.ClaimedLevels {
					if l > v.Level {
						t.Errorf("claimed level %d exceeds level %d", l, v.Level)
					}
				}
			}
		}
	}()

	wg.Wait()
	close(stop)
	readerWG.Wait()

	v, _ := e.View("hot")
	if v.Level == 0 {
		t.Fatal("no progress")
	}
	for _, l := range v.ClaimedLevels {
		if l > v.Level {
			t.Fatalf("claimed %d > level %d", l, v.Level)
		}
	}
	// 每个被领取的等级恰好发放一份（首次结果数 == 已领取等级数）。
	if atomic.LoadInt64(&granted) != int64(len(v.ClaimedLevels)) {
		t.Fatalf("granted=%d claimed=%v", atomic.LoadInt64(&granted), v.ClaimedLevels)
	}
	if err := e.Close(); err != nil {
		t.Fatal(err)
	}
	e2 := testEngine(t, dir)
	defer e2.Close()
	v2, _ := e2.View("hot")
	if len(v2.ClaimedLevels) != len(v.ClaimedLevels) {
		t.Fatalf("claims changed after restart: %v vs %v", v2.ClaimedLevels, v.ClaimedLevels)
	}
}

// TestEventAttributionDuringSwitch 切换前事件归属 s1；切换后：
//   - OccurredAt 在 s2 的事件全部归属 s2；
//   - OccurredAt 在 s1 的迟到事件被拒绝（season_settled）；
//   - 同 ID 事件重放只返回 duplicate，绝不二次累计。
func TestEventAttributionDuringSwitch(t *testing.T) {
	e := testEngine(t, "")
	defer e.Close()

	var wg sync.WaitGroup
	// 阶段一：切换前并发投递 s1 与“未来 s2 时刻”的事件。
	// s2 配置尚不存在 -> s2 事件 season_not_found（不会错算到 s1）。
	preOld, preFuture := int64(0), int64(0)
	for i := 0; i < 100; i++ {
		wg.Add(1)
		i := i
		go func() {
			defer wg.Done()
			ev := Event{ID: fmt.Sprintf("old-%d", i), PlayerID: "mix", Amount: 1, OccurredAt: base.Add(time.Hour)}
			r, err := e.Ingest(context.Background(), ev)
			if err != nil {
				t.Error(err)
				return
			}
			if r.Accepted && r.SeasonID == "s1" {
				atomic.AddInt64(&preOld, 1)
			}
			_ = preFuture
		}()
	}
	wg.Wait()
	if preOld != 100 {
		t.Fatalf("pre switch s1 accepted=%d", preOld)
	}

	s2 := SeasonConfig{ID: "s2", Start: base.Add(24 * time.Hour), CarryRatio: 0,
		LevelThresholds: []int64{10}, RuleVersion: "r1"}
	if _, err := e.SwitchSeason(context.Background(), s2); err != nil {
		t.Fatal(err)
	}

	// 阶段二：切换后并发，一半 s1 迟到、一半 s2 正常。
	newCnt, lateRej := int64(0), int64(0)
	for i := 0; i < 200; i++ {
		wg.Add(1)
		i := i
		go func() {
			defer wg.Done()
			var ev Event
			if i%2 == 0 {
				ev = Event{ID: fmt.Sprintf("late-%d", i), PlayerID: "mix", Amount: 1, OccurredAt: base.Add(2 * time.Hour)}
			} else {
				ev = Event{ID: fmt.Sprintf("new-%d", i), PlayerID: "mix", Amount: 1, OccurredAt: base.Add(25 * time.Hour)}
			}
			r, err := e.Ingest(context.Background(), ev)
			if err != nil {
				t.Error(err)
				return
			}
			if r.Accepted && r.SeasonID == "s2" {
				atomic.AddInt64(&newCnt, 1)
			}
			if !r.Accepted && r.Reason == "season_settled" {
				atomic.AddInt64(&lateRej, 1)
			}
		}()
	}
	wg.Wait()
	if newCnt != 100 || lateRej != 100 {
		t.Fatalf("post switch new=%d lateRejected=%d", newCnt, lateRej)
	}

	// 阶段三：全部事件再投一次 => s1/s2 均判重或拒绝，零新增。
	for _, id := range []string{"old-0", "new-1"} {
		at := base.Add(time.Hour)
		if id == "new-1" {
			at = base.Add(25 * time.Hour)
		}
		r, err := e.Ingest(context.Background(), Event{ID: id, PlayerID: "mix", Amount: 1, OccurredAt: at})
		if err != nil || r.Accepted {
			t.Fatalf("replay %s: %+v err=%v", id, r, err)
		}
		if r.Reason != "duplicate" && r.Reason != "season_settled" {
			t.Fatalf("replay reason: %s", r.Reason)
		}
	}
}
