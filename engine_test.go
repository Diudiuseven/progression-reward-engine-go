package engine

import (
	"context"
	"testing"
	"time"
)

var base = time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)

func ingest(t *testing.T, e *Engine, ev Event) *IngestResult {
	t.Helper()
	res, err := e.Ingest(context.Background(), ev)
	if err != nil {
		t.Fatalf("ingest %s: %v", ev.ID, err)
	}
	return res
}

func TestBasicLevelProgressAndDedup(t *testing.T) {
	dir := t.TempDir()
	e := testEngine(t, dir)
	defer e.Close()

	ev := func(id string, off time.Duration, amt int64) Event {
		return Event{ID: id, PlayerID: "p1", Type: "play", Amount: amt, OccurredAt: base.Add(off)}
	}
	// 三条事件间隔超过窗口 => 不触发递减，累计 120 -> 等级 1
	r := ingest(t, e, ev("e1", time.Hour, 40))
	if !r.Accepted || r.LevelAfter != 0 {
		t.Fatalf("e1: %+v", r)
	}
	r = ingest(t, e, ev("e2", 2*time.Hour, 40))
	r = ingest(t, e, ev("e3", 3*time.Hour, 40))
	if r.LevelAfter != 1 || r.ProgressAfter != 120 {
		t.Fatalf("after e3 level=%d prog=%d", r.LevelAfter, r.ProgressAfter)
	}

	// 重复事件：乱序重投 / 同 ID 再来一次，均不重复累计
	r = ingest(t, e, ev("e2", 2*time.Hour, 40))
	if r.Accepted || r.Reason != "duplicate" {
		t.Fatalf("dup e2: %+v", r)
	}
	// 迟到一条旧事件（赛季内，时间更早）：不影响既有结论以外的既定等级事实
	v, err := e.View("p1")
	if err != nil {
		t.Fatal(err)
	}
	if v.EffectiveProgress != 120 {
		t.Fatalf("progress changed by duplicate: %d", v.EffectiveProgress)
	}
}

func TestOutOfOrderEvents(t *testing.T) {
	e := testEngine(t, "")
	defer e.Close()
	mk := func(id string, off time.Duration) Event {
		return Event{ID: id, PlayerID: "p9", Amount: 100, OccurredAt: base.Add(off)}
	}
	// 先到晚事件，再到早事件；递减窗口按 OccurredAt 计算，结果稳定。
	ingest(t, e, mk("late", 2*time.Hour))
	ingest(t, e, mk("early", time.Hour))
	// 不同 ID 但同一时刻：窗口内已有 1 条，因子 0.5 => 50
	r3 := ingest(t, e, mk("burst", time.Hour))
	if r3.Breakdown == nil || r3.Breakdown.Granted != 50 || r3.Breakdown.FrequencyFactor != 0.5 {
		t.Fatalf("burst breakdown: %+v", r3.Breakdown)
	}
	// 同 ID 再来：判重，不累计
	r := ingest(t, e, mk("early", time.Hour))
	if r.Reason != "duplicate" {
		t.Fatalf("expected duplicate, got %+v", r)
	}
	v, _ := e.View("p9")
	// late 100 + early 100 + burst 50
	if v.EffectiveProgress != 250 || v.Level != 1 {
		t.Fatalf("out of order prog=%d level=%d", v.EffectiveProgress, v.Level)
	}
}

func TestPlayerIsolation(t *testing.T) {
	e := testEngine(t, "")
	defer e.Close()
	for i, p := range []PlayerID{"a", "b", "c"} {
		ev := Event{ID: "same-id", PlayerID: p, Amount: 400, OccurredAt: base.Add(time.Duration(i) * time.Hour)}
		r := ingest(t, e, ev)
		if !r.Accepted {
			t.Fatalf("player %s rejected: %+v", p, r)
		}
	}
	for _, p := range []PlayerID{"a", "b", "c"} {
		v, _ := e.View(p)
		if v.EffectiveProgress != 400 || v.Level != 2 {
			t.Fatalf("player %s contaminated: %+v", p, v)
		}
	}
}
