package engine

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestRestartContinuesFromCommitted(t *testing.T) {
	dir := t.TempDir()
	e := testEngine(t, dir)
	ingest(t, e, Event{ID: "e1", PlayerID: "p", Amount: 200, OccurredAt: base.Add(time.Hour)})
	ingest(t, e, Event{ID: "e2", PlayerID: "p", Amount: 100, OccurredAt: base.Add(2 * time.Hour)})
	cr, err := e.ClaimLevel(context.Background(), "p", "s1", 1, "c1")
	if err != nil {
		t.Fatal(err)
	}
	if cr.Record.Rewards[0].Amount != 100 {
		t.Fatalf("reward %+v", cr.Record)
	}
	if err := e.Close(); err != nil {
		t.Fatal(err)
	}

	// 重启：从最近一次已提交状态继续。
	e2 := testEngine(t, dir)
	defer e2.Close()
	v, err := e2.View("p")
	if err != nil {
		t.Fatal(err)
	}
	if v.EffectiveProgress != 300 || v.Level != 2 {
		t.Fatalf("after restart: %+v", v)
	}
	// 已发奖励不重复发放：同一 claimID 返回首次结果。
	cr2, err := e2.ClaimLevel(context.Background(), "p", "s1", 1, "c1")
	if err != nil || !cr2.AlreadyClaimed || cr2.ClaimID != "c1" {
		t.Fatalf("duplicate claim after restart: %+v err=%v", cr2, err)
	}
	// 已结算事件重投：仍然判重，不重复累计。
	r := ingest(t, e2, Event{ID: "e1", PlayerID: "p", Amount: 200, OccurredAt: base.Add(time.Hour)})
	if r.Accepted || r.Reason != "duplicate" {
		t.Fatalf("replay event: %+v", r)
	}
	v2, _ := e2.View("p")
	if v2.EffectiveProgress != 300 {
		t.Fatalf("progress mutated: %d", v2.EffectiveProgress)
	}
	// 新事件可继续累计。
	r = ingest(t, e2, Event{ID: "e3", PlayerID: "p", Amount: 300, OccurredAt: base.Add(3 * time.Hour)})
	if !r.Accepted || r.LevelAfter != 3 {
		t.Fatalf("post-restort ingest: %+v", r)
	}
}

func TestSnapshotThenRestart(t *testing.T) {
	dir := t.TempDir()
	e := testEngine(t, dir)
	for i := 0; i < 10; i++ {
		ingest(t, e, Event{
			ID: idf("e%d", i), PlayerID: "p", Amount: 30,
			OccurredAt: base.Add(time.Duration(i) * 2 * time.Minute),
		})
	}
	if _, err := e.ClaimLevel(context.Background(), "p", "s1", 1, "c1"); err != nil {
		t.Fatal(err)
	}
	if err := e.Snapshot(); err != nil {
		t.Fatal(err)
	}
	// 快照后关闭。
	if err := e.Close(); err != nil {
		t.Fatal(err)
	}

	e2 := testEngine(t, dir)
	defer e2.Close()
	v, _ := e2.View("p")
	if v.EffectiveProgress != 300 || v.Level != 2 || len(v.ClaimedLevels) != 1 {
		t.Fatalf("snapshot restart: %+v", v)
	}
}

func TestPreparedWithoutCommitIsAbortedOnRestart(t *testing.T) {
	dir := t.TempDir()
	e := testEngine(t, dir)
	ingest(t, e, Event{ID: "e1", PlayerID: "p", Amount: 100, OccurredAt: base.Add(time.Hour)})

	// 直接向分片 WAL 写入一条 prepared，但不写 committed，模拟“领到一半崩溃”。
	sh := e.shards[shardIndex("p", e.n)]
	rec := &ClaimRecord{
		ClaimID: "half", PlayerID: "p", SeasonID: "s1", Level: 1,
		RuleVersion: "r1", Rewards: []Reward{{Code: "coin", Amount: 100}},
		Status: ClaimPrepared,
	}
	if err := sh.store.append(walRecord{Kind: recClaimPrepared, Claim: rec}); err != nil {
		t.Fatal(err)
	}
	// 内存也置于半成品状态。
	sh.mu.Lock()
	sh.get("p", "s1").PendingClaim = rec
	sh.mu.Unlock()
	if err := e.Close(); err != nil {
		t.Fatal(err)
	}

	e2 := testEngine(t, dir)
	defer e2.Close()
	sh2 := e2.shards[shardIndex("p", e2.n)]
	sh2.mu.RLock()
	p := sh2.get("p", "s1")
	pending := p.PendingClaim
	claimed := len(p.ClaimedLevels)
	sh2.mu.RUnlock()
	if pending != nil {
		t.Fatal("half-finished claim survived restart")
	}
	if claimed != 0 {
		t.Fatal("prepared-only claim was treated as granted")
	}
	// WAL 中应补写一条 aborted 记录。
	recs, err := sh2.store.readRecords()
	if err != nil {
		t.Fatal(err)
	}
	hasAborted := false
	for _, r := range recs {
		if r.Kind == recClaimAborted && r.Claim.ClaimID == "half" {
			hasAborted = true
		}
	}
	if !hasAborted {
		t.Fatal("missing abort record for unfinished claim")
	}
	// 同一 claimID 可以重新领取（首次结果）。
	cr, err := e2.ClaimLevel(context.Background(), "p", "s1", 1, "half")
	if err != nil {
		t.Fatal(err)
	}
	if cr.AlreadyClaimed {
		t.Fatal("aborted claim should be retryable")
	}
}

func TestSeasonSwitchResumedAfterCrash(t *testing.T) {
	dir := t.TempDir()
	e := testEngine(t, dir)
	ingest(t, e, Event{ID: "e1", PlayerID: "p", Amount: 200, OccurredAt: base.Add(time.Hour)})
	// 登记 s2 配置后立即“崩溃”，不做结转。
	s2 := SeasonConfig{
		ID: "s2", Start: base.Add(24 * time.Hour), CarryRatio: 0.5,
		LevelThresholds: []int64{50}, RuleVersion: "r1",
	}
	if err := e.meta.append(walRecord{Kind: recSeasonAdded, Season: &s2}); err != nil {
		t.Fatal(err)
	}
	if err := e.seasons.add(s2); err != nil {
		t.Fatal(err)
	}
	if err := e.Close(); err != nil {
		t.Fatal(err)
	}

	e2 := testEngine(t, dir)
	defer e2.Close()
	v, _ := e2.View("p")
	// 时钟在 s2 区间内，应看到结转后的新账本：200*0.5=100
	if v.SeasonID != "s2" || v.EffectiveProgress != 100 || v.Level != 1 {
		t.Fatalf("resumed switch: %+v", v)
	}
	// s1 已冻结，迟到事件被拒。
	r := ingest(t, e2, Event{ID: "late", PlayerID: "p", Amount: 999, OccurredAt: base.Add(2 * time.Hour)})
	if r.Accepted || r.Reason != "season_settled" {
		t.Fatalf("late: %+v", r)
	}
}

// 确保持久化目录下产生了真实文件（证明本地持久化而非纯内存）。
func TestPersistentFilesExist(t *testing.T) {
	dir := t.TempDir()
	e := testEngine(t, dir)
	ingest(t, e, Event{ID: "e1", PlayerID: "p", Amount: 100, OccurredAt: base.Add(time.Hour)})
	if err := e.Close(); err != nil {
		t.Fatal(err)
	}
	var files []string
	_ = filepath.Walk(dir, func(path string, info os.FileInfo, err error) error {
		if err == nil && !info.IsDir() {
			files = append(files, path)
		}
		return nil
	})
	if len(files) == 0 {
		t.Fatal("no persisted files")
	}
}

func TestTornWALTailRecovered(t *testing.T) {
	dir := t.TempDir()
	e := testEngine(t, dir)
	ingest(t, e, Event{ID: "e1", PlayerID: "p", Amount: 100, OccurredAt: base.Add(time.Hour)})
	mustClose(t, e)

	// 找到包含该事件的分片日志，追加一段“写到一半”的 JSON（无换行）。
	var logFile string
	for i := 0; i < 4; i++ {
		p := filepath.Join(dir, fmt.Sprintf("shard-%03d", i), fmt.Sprintf("shard-%03d.log", i))
		data, err := os.ReadFile(p)
		if err == nil && len(data) > 0 {
			logFile = p
			_ = data
		}
	}
	if logFile == "" {
		t.Fatal("shard log not found")
	}
	f, err := os.OpenFile(logFile, os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString(`{"kind":"event_accepted","player_id":"p","season_id":"s1","breakdown":{"event_id":"ghost"`); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}

	e2 := testEngine(t, dir)
	defer e2.Close()
	v, _ := e2.View("p")
	if v.EffectiveProgress != 100 {
		t.Fatalf("torn tail changed committed state: %d", v.EffectiveProgress)
	}
	// 残行已在打开时被截断，新事件可正常追加。
	r := ingest(t, e2, Event{ID: "e2", PlayerID: "p", Amount: 100, OccurredAt: base.Add(2 * time.Hour)})
	if !r.Accepted || r.ProgressAfter != 200 {
		t.Fatalf("ingest after torn repair: %+v", r)
	}
}

func mustClose(t *testing.T, e *Engine) {
	t.Helper()
	if err := e.Close(); err != nil {
		t.Fatal(err)
	}
}
