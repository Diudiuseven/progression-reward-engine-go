// Command demo 在本地用进程内状态 + 目录持久化，端到端演示：
// 事件摄入 -> 高频递减/每日与赛季封顶 -> 等级推进 -> 奖励领取（幂等）
// -> 赛季切换与结转 -> 重启恢复。运行后可检查 ./demo-data 下的 WAL 与快照文件。
package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"time"

	engine "github.com/Diudiuseven/progression-reward-engine-go"
)

func must(err error) {
	if err != nil {
		fmt.Fprintln(os.Stderr, "ERROR:", err)
		os.Exit(1)
	}
}

func main() {
	dir := filepath.Join(".", "demo-data")
	_ = os.RemoveAll(dir)
	t0 := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	clock := func() time.Time { return t0.Add(72 * time.Hour) }

	e, err := engine.New(engine.Config{Dir: dir, ShardCount: 8, Clock: clock})
	must(err)

	// 规则版本 r1：每个等级一次性里程碑奖励。
	must(e.RegisterRules(engine.RuleSet{
		Version: "r1", EffectiveFrom: t0,
		Rewards: map[int][]engine.Reward{
			1: {{Code: "coin", Amount: 100}},
			2: {{Code: "gem", Amount: 10}},
			3: {{Code: "gem", Amount: 50}},
		},
	}))
	must(e.StartSeason(engine.SeasonConfig{
		ID: "s1", Start: t0, End: t0.Add(24 * time.Hour),
		CarryRatio:      0.5,
		LevelThresholds: []int64{100, 300, 600},
		RuleVersion:     "r1",
		Decay: engine.DecayConfig{
			Window: 60 * time.Second, Gamma: 0.8, MinFactor: 0.2,
		},
		DailyCap:  900,
		SeasonCap: 1000,
	}))

	ctx := context.Background()
	send := func(id string, player engine.PlayerID, off time.Duration, amt int64) {
		r, err := e.Ingest(ctx, engine.Event{
			ID: id, PlayerID: player, Type: "play",
			Amount: amt, OccurredAt: t0.Add(off),
		})
		must(err)
		if r.Accepted {
			b := r.Breakdown
			fmt.Printf("ingest %-4s %s gross=%3d factor=%.2f afterDecay=%3d dailyCut=%3d seasonCut=%3d granted=%3d -> prog=%d level=%d (season %s)\n",
				id, player, b.Gross, b.FrequencyFactor, b.AfterDecay, b.DailyCut, b.SeasonCut, b.Granted,
				r.ProgressAfter, r.LevelAfter, r.SeasonID)
		} else {
			fmt.Printf("ingest %-4s %s rejected: %s\n", id, player, r.Reason)
		}
	}

	fmt.Println("== 高频平滑：同一玩家 0..3 秒连发 ==")
	send("e1", "alice", 0, 100)
	send("e2", "alice", 1*time.Second, 100)
	send("e3", "alice", 2*time.Second, 100)
	send("e4", "alice", 3*time.Second, 100)
	fmt.Println("== 重复事件（幂等，不重复累计）==")
	send("e2", "alice", 1*time.Second, 100)

	fmt.Println("== 多玩家互不污染 ==")
	send("b1", "bob", 10*time.Minute, 350)
	send("c1", "carol", 20*time.Minute, 600)

	fmt.Println("== 领取等级奖励（重复领取只返回首次结果）==")
	claim := func(player engine.PlayerID, season engine.SeasonID, level int, id string) {
		res, err := e.ClaimLevel(ctx, player, season, level, id)
		must(err)
		fmt.Printf("claim  %s lvl%d id=%s already=%v rewards=%v (rule %s)\n",
			player, level, res.ClaimID, res.AlreadyClaimed, res.Record.Rewards, res.Record.RuleVersion)
	}
	claim("alice", "s1", 1, "alice-l1")
	claim("alice", "s1", 1, "alice-l1")       // 同一 claimID
	claim("alice", "s1", 1, "alice-l1-again") // 不同 claimID -> 仍返回首次
	claim("bob", "s1", 2, "bob-l2")
	claim("carol", "s1", 3, "carol-l3")

	fmt.Println("== 赛季切换：50% 结转，规则升级 r2（旧奖励不追溯改写）==")
	must(e.RegisterRules(engine.RuleSet{
		Version: "r2", EffectiveFrom: t0.Add(24 * time.Hour),
		Rewards: map[int][]engine.Reward{
			1: {{Code: "coin", Amount: 1000}}, // 新版本口径变化
			2: {{Code: "gem", Amount: 88}},
		},
	}))
	set, err := e.SwitchSeason(ctx, engine.SeasonConfig{
		ID: "s2", Start: t0.Add(24 * time.Hour),
		CarryRatio:      0.5,
		LevelThresholds: []int64{100, 300},
		RuleVersion:     "r2",
		Decay:           engine.DecayConfig{Window: 60 * time.Second, Gamma: 0.8, MinFactor: 0.2},
	})
	must(err)
	for _, s := range set.PlayerSummaries {
		fmt.Printf("settle s1 -> s2: %s finalLevel=%d eff=%d carried=%d unclaimed=%v\n",
			s.PlayerID, s.FinalLevel, s.EffectiveProgress, s.CarriedOver, s.UnclaimedLevels)
	}

	fmt.Println("== 切换窗口事件：迟到 s1 事件被拒，s2 事件正常入账 ==")
	send("late", "alice", 2*time.Hour, 999)
	send("new", "alice", 25*time.Hour, 200)

	fmt.Println("== s1 旧奖励仍按 r1 口径领取，不被 r2 改写 ==")
	claim("carol", "s1", 3, "carol-l3-again")

	fmt.Println("== 快照压缩 + 关闭 ==")
	must(e.Snapshot())
	must(e.Close())

	fmt.Println("== 重启：从最近已提交状态恢复，奖励不重发 ==")
	e2, err := engine.New(engine.Config{Dir: dir, ShardCount: 8, Clock: clock})
	must(err)
	for _, p := range []engine.PlayerID{"alice", "bob", "carol"} {
		v, err := e2.View(p)
		must(err)
		fmt.Printf("view   %s season=%s status=%s raw=%d eff=%d level=%d claimed=%v daily=%v\n",
			p, v.SeasonID, v.Status, v.RawProgress, v.EffectiveProgress, v.Level, v.ClaimedLevels, v.DailyGranted)
	}
	claim2 := func(player engine.PlayerID, season engine.SeasonID, level int, id string) {
		res, err := e2.ClaimLevel(ctx, player, season, level, id)
		must(err)
		fmt.Printf("reclaim %s lvl%d already=%v (no double grant)\n", player, level, res.AlreadyClaimed)
	}
	claim2("bob", "s1", 2, "bob-l2")
	must(e2.Close())
	fmt.Println("\n持久化文件位于:", dir)
}
