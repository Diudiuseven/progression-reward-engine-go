package engine

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"
)

// Engine 串起事件摄入、进度结算、奖励领取与赛季切换。
//
// 并发模型：
//   - 玩家按 FNV 哈希固定到一个分片，摄入/领取只锁该分片，玩家间不互相阻塞；
//   - 赛季配置表与规则簿用独立读写锁；
//   - 赛季切换是唯一的跨分片操作，逐分片锁定处理，不做全局停顿，
//     切换期间到达的事件依然按 OccurredAt 归属：新赛季事件在旧赛季账本被
//     标记 Settled 之前还是之后处理，结论都是确定且不重复的。
//
// 持久化：每个分片独立 WAL（JSON-lines，每条 fsync）+ 原子快照；
// 元数据（规则/赛季配置）单独一个 WAL。Dir 为空时全部退化为内存模式。
type Engine struct {
	cfg    Config
	clock  func() time.Time
	n      int
	shards []*shard
	meta   *store

	rulesMu sync.RWMutex
	rules   *ruleBook

	seasonsMu sync.RWMutex
	seasons   *seasonTable

	switchMu sync.Mutex // 串行化赛季切换
	closedMu sync.RWMutex
	closed   bool

	stopJanitor chan struct{}
	wg          sync.WaitGroup
}

func defaultConfig(cfg Config) Config {
	if cfg.ShardCount <= 0 {
		cfg.ShardCount = 16
	}
	if cfg.Clock == nil {
		cfg.Clock = time.Now
	}
	if cfg.RetainSettled > 0 && cfg.EvictInterval <= 0 {
		cfg.EvictInterval = time.Minute
	}
	return cfg
}

func New(cfg Config) (*Engine, error) {
	cfg = defaultConfig(cfg)
	e := &Engine{
		cfg:         cfg,
		clock:       cfg.Clock,
		n:           cfg.ShardCount,
		rules:       newRuleBook(),
		seasons:     newSeasonTable(),
		stopJanitor: make(chan struct{}),
	}
	dir := cfg.Dir
	var err error
	if dir != "" {
		if err := mkdirAll(dir); err != nil {
			return nil, err
		}
		e.meta, err = openStore(filepath.Join(dir, "meta"), "meta")
		if err != nil {
			return nil, err
		}
	} else {
		e.meta, _ = openStore("", "meta")
	}
	e.shards = make([]*shard, e.n)
	for i := 0; i < e.n; i++ {
		var st *store
		if dir != "" {
			st, err = openStore(filepath.Join(dir, fmt.Sprintf("shard-%03d", i)), fmt.Sprintf("shard-%03d", i))
		} else {
			st, _ = openStore("", "")
		}
		if err != nil {
			return nil, err
		}
		e.shards[i] = newShard(uint32(i), st)
	}
	if err := e.restore(); err != nil {
		return nil, err
	}
	e.resumePendingSettlements()
	if cfg.RetainSettled > 0 {
		e.wg.Add(1)
		go e.janitor()
	}
	return e, nil
}

func mkdirAll(dir string) error { return os.MkdirAll(dir, 0o755) }

// ---------- 规则与赛季 ----------

func (e *Engine) RegisterRules(rs RuleSet) error {
	e.rulesMu.Lock()
	defer e.rulesMu.Unlock()
	if err := e.rules.register(rs); err != nil {
		return err
	}
	return e.meta.append(walRecord{Kind: recRuleRegistered, Rule: &rs})
}

// StartSeason 注册并开启一个赛季。赛季区间必须与既有赛季不重叠。
func (e *Engine) StartSeason(cfg SeasonConfig) error {
	e.rulesMu.RLock()
	_, err := e.rules.get(cfg.RuleVersion)
	e.rulesMu.RUnlock()
	if err != nil {
		return err
	}
	e.seasonsMu.Lock()
	err = e.seasons.add(cfg)
	e.seasonsMu.Unlock()
	if err != nil {
		return err
	}
	return e.meta.append(walRecord{Kind: recSeasonAdded, Season: &cfg})
}

func (e *Engine) ensureOpen() error {
	e.closedMu.RLock()
	defer e.closedMu.RUnlock()
	if e.closed {
		return ErrEngineClosed
	}
	return nil
}

// ---------- 事件摄入 ----------

func validateEvent(ev Event) error {
	if ev.ID == "" || ev.PlayerID == "" || ev.Amount < 0 || ev.OccurredAt.IsZero() {
		return ErrEventInvalid
	}
	return nil
}

// Ingest 摄入一条玩法事件。重复事件返回 Accepted=false/Reason=duplicate
// 且不会重复累计；迟到事件若目标赛季已结算则返回 season_settled，
// 已对外可见的等级与已领取奖励永不被改写。
func (e *Engine) Ingest(ctx context.Context, ev Event) (*IngestResult, error) {
	if err := e.ensureOpen(); err != nil {
		return nil, err
	}
	if err := validateEvent(ev); err != nil {
		return nil, err
	}
	e.seasonsMu.RLock()
	sc, err := e.seasons.locate(ev.OccurredAt)
	e.seasonsMu.RUnlock()
	if err != nil {
		return &IngestResult{
			EventID: ev.ID, PlayerID: ev.PlayerID, Accepted: false,
			Reason: "season_not_found",
		}, nil
	}

	sh := e.shards[shardIndex(ev.PlayerID, e.n)]
	sh.mu.Lock()
	defer sh.mu.Unlock()

	if sh.dedup.has(sc.ID, ev.PlayerID, ev.ID) {
		p := sh.get(ev.PlayerID, sc.ID)
		res := &IngestResult{
			EventID: ev.ID, PlayerID: ev.PlayerID, SeasonID: sc.ID,
			Accepted: false, Reason: "duplicate",
		}
		if p != nil {
			res.LevelBefore, res.LevelAfter = p.Level, p.Level
			res.ProgressBefore, res.ProgressAfter = p.EffectiveProgress, p.EffectiveProgress
		}
		return res, nil
	}
	if existing := sh.get(ev.PlayerID, sc.ID); existing != nil && existing.Settled {
		return &IngestResult{
			EventID: ev.ID, PlayerID: ev.PlayerID, Accepted: false,
			Reason: "season_settled", SeasonID: sc.ID,
			LevelBefore: existing.Level, LevelAfter: existing.Level,
			ProgressBefore: existing.EffectiveProgress, ProgressAfter: existing.EffectiveProgress,
		}, nil
	}

	p := sh.getOrCreate(ev.PlayerID, *sc)
	beforeLvl, beforeProg := p.Level, p.EffectiveProgress
	b := gain(ev, p, sc)
	p.applyGain(b, sc.LevelThresholds, ev.OccurredAt)
	sh.dedup.mark(sc.ID, ev.PlayerID, ev.ID)

	rec := walRecord{
		Kind: recEventAccepted, PlayerID: ev.PlayerID, SeasonID: sc.ID,
		OccurredAt: ev.OccurredAt, LevelAfter: p.Level, Breakdown: b,
	}
	if err := sh.store.append(rec); err != nil {
		// WAL 失败：内存状态回滚，且不标记去重，外部看不到任何结论，事件可安全重试。
		e.rollbackEvent(p, b, sc, ev.OccurredAt)
		return nil, err
	}

	return &IngestResult{
		EventID: ev.ID, PlayerID: ev.PlayerID, Accepted: true, SeasonID: sc.ID,
		LevelBefore: beforeLvl, LevelAfter: p.Level,
		ProgressBefore: beforeProg, ProgressAfter: p.EffectiveProgress,
		Breakdown: b,
	}, nil
}

// rollbackEvent 在 WAL append 失败时撤销内存中的本次结算。
func (e *Engine) rollbackEvent(p *playerProgress, b *GainBreakdown, sc *SeasonConfig, at time.Time) {
	p.RawProgress -= b.Gross
	p.EffectiveProgress -= b.Granted
	p.SeasonGranted -= b.Granted
	p.DailyGranted[b.DayBucket] -= b.Granted
	n := at.UnixNano()
	idx := sort.Search(len(p.EventTimes), func(i int) bool { return p.EventTimes[i] >= n })
	if idx < len(p.EventTimes) && p.EventTimes[idx] == n {
		p.EventTimes = append(p.EventTimes[:idx], p.EventTimes[idx+1:]...)
	}
	p.Level = levelFor(p.EffectiveProgress, sc.LevelThresholds)
}

// ---------- 奖励领取（两阶段 + 幂等） ----------

func (e *Engine) ClaimLevel(ctx context.Context, player PlayerID, season SeasonID, level int, claimID string) (*ClaimResult, error) {
	if err := e.ensureOpen(); err != nil {
		return nil, err
	}
	if level < 1 || claimID == "" || player == "" {
		return nil, ErrEventInvalid
	}
	sh := e.shards[shardIndex(player, e.n)]
	sh.mu.Lock()
	defer sh.mu.Unlock()

	ledgers := sh.listPlayerSeasons(player)
	// claimID 是全局幂等键：若它已绑定到其它等级，本次请求属冲突，
	// 不能让同一领取单号产生第二份奖励。
	for _, p := range ledgers {
		for lvl, id := range p.ClaimedLevels {
			if id == claimID && lvl != level {
				return e.claimResultFrom(p, lvl, true)
			}
		}
	}
	// 优先命中历史成功结论：重复领取只返回首次结果。
	for _, p := range ledgers {
		if season != "" && p.SeasonID != season {
			continue
		}
		if id, ok := p.ClaimedLevels[level]; ok && id == claimID {
			return e.claimResultFrom(p, level, true)
		}
	}
	// claimID 相同但此前是半成品（崩溃/失败）：允许复用同一 claimID 重试。
	var target *playerProgress
	for _, p := range ledgers {
		if pc := p.PendingClaim; pc != nil && pc.ClaimID == claimID && pc.Level == level {
			target = p
		}
	}
	if target == nil {
		if season != "" {
			if p := sh.get(player, season); p != nil && p.Level >= level {
				target = p
			}
		} else {
			target = e.pickClaimLedger(sh, player, level)
		}
	}
	if target == nil {
		return nil, ErrLevelNotReached
	}

	// 该等级已被其他 claimID 领取：返回首次结果，保证同等级不重复发放。
	if id, ok := target.ClaimedLevels[level]; ok && id != claimID {
		return e.claimResultFrom(target, level, true)
	}
	if target.Level < level {
		return nil, ErrLevelNotReached
	}

	// 阶段一：冻结奖励快照，写 prepared。
	var rec *ClaimRecord
	if pc := target.PendingClaim; pc != nil && pc.ClaimID == claimID && pc.Level == level {
		rec = pc // 复用上次半成品
	} else if pc != nil {
		return nil, ErrClaimIncomplete
	} else {
		rws, err := e.rewardSnapshot(target.RuleVersion, level)
		if err != nil {
			return nil, err
		}
		rec = &ClaimRecord{
			ClaimID: claimID, PlayerID: player, SeasonID: target.SeasonID,
			Level: level, RuleVersion: target.RuleVersion,
			Rewards: rws, Status: ClaimPrepared,
		}
		target.PendingClaim = rec
		if err := sh.store.append(walRecord{Kind: recClaimPrepared, Claim: rec}); err != nil {
			target.PendingClaim = nil
			return nil, err
		}
	}

	// 阶段二：原子提交（等级固化 + 清空半成品 + 已发结论不可改写）。
	rec.Status = ClaimCommitted
	rec.CommittedAt = e.clock()
	target.ClaimedLevels[level] = rec.ClaimID
	target.PendingClaim = nil
	commit := *rec
	if err := sh.store.append(walRecord{Kind: recClaimCommitted, Claim: &commit}); err != nil {
		// 提交未落盘则不产生对外可见结论，回滚内存，保留 prepared 供重试。
		delete(target.ClaimedLevels, level)
		target.PendingClaim = rec
		rec.Status = ClaimPrepared
		var zero time.Time
		rec.CommittedAt = zero
		return nil, err
	}
	return &ClaimResult{
		ClaimID: claimID, PlayerID: player, SeasonID: target.SeasonID,
		Level: level, AlreadyClaimed: false, Record: &commit,
	}, nil
}

func (e *Engine) rewardSnapshot(rv RuleVersion, level int) ([]Reward, error) {
	e.rulesMu.RLock()
	defer e.rulesMu.RUnlock()
	return e.rules.rewardsFor(rv, level)
}

// pickClaimLedger 选择可领取某等级的账本：优先未结算赛季，其次已结算但保留的赛季。
func (e *Engine) pickClaimLedger(sh *shard, player PlayerID, level int) *playerProgress {
	var settledCandidate *playerProgress
	for _, p := range sh.listPlayerSeasons(player) {
		if p.Level < level {
			continue
		}
		if !p.Settled {
			return p
		}
		if settledCandidate == nil {
			settledCandidate = p
		}
	}
	return settledCandidate
}

func (e *Engine) claimResultFrom(p *playerProgress, level int, already bool) (*ClaimResult, error) {
	rws, err := e.rewardSnapshot(p.RuleVersion, level)
	if err != nil {
		return nil, err
	}
	rec := &ClaimRecord{
		ClaimID: p.ClaimedLevels[level], PlayerID: p.PlayerID, SeasonID: p.SeasonID,
		Level: level, RuleVersion: p.RuleVersion, Rewards: rws, Status: ClaimCommitted,
	}
	return &ClaimResult{
		ClaimID: rec.ClaimID, PlayerID: p.PlayerID, SeasonID: p.SeasonID,
		Level: level, AlreadyClaimed: already, Record: rec,
	}, nil
}

// ---------- 赛季切换 ----------

// SwitchSeason 原子登记新赛季，并按旧赛季 CarryRatio 逐分片结转。
// 返回的结算汇总以实际完成的账本为准；过程可崩溃恢复（重启后自动续做）。
func (e *Engine) SwitchSeason(ctx context.Context, next SeasonConfig) (*SeasonSettlement, error) {
	if err := e.ensureOpen(); err != nil {
		return nil, err
	}
	e.switchMu.Lock()
	defer e.switchMu.Unlock()

	e.rulesMu.RLock()
	if _, err := e.rules.get(next.RuleVersion); err != nil {
		e.rulesMu.RUnlock()
		return nil, err
	}
	e.rulesMu.RUnlock()

	e.seasonsMu.RLock()
	prev := e.prevSeasonLocked(next.Start)
	e.seasonsMu.RUnlock()
	// 先持久化新赛季配置：切换窗口内到达的 OccurredAt>=新起点 的事件
	// 即使在结转完成前也能被正确路由到新赛季。
	e.seasonsMu.Lock()
	if err := e.seasons.add(next); err != nil {
		e.seasonsMu.Unlock()
		return nil, err
	}
	e.seasonsMu.Unlock()
	if err := e.meta.append(walRecord{Kind: recSeasonAdded, Season: &next}); err != nil {
		return nil, err
	}

	res := &SeasonSettlement{SettledAt: e.clock()}
	if prev == nil {
		return res, nil
	}
	res.SeasonID = prev.ID
	if err := e.settlePair(*prev, next, ctx, res); err != nil {
		return res, err
	}
	// 若此前有“已登记但未结转完”的更早赛季对（上次切换在中途失败），
	// 本次切换一并补做完，保证所有已提交配置都收敛到完整状态。
	e.seasonsMu.RLock()
	all := make([]SeasonConfig, 0)
	for _, id := range e.seasons.list() {
		if s, ok := e.seasons.get(id); ok {
			all = append(all, *s)
		}
	}
	e.seasonsMu.RUnlock()
	sort.Slice(all, func(i, j int) bool { return all[i].Start.Before(all[j].Start) })
	for i := 0; i+1 < len(all); i++ {
		if all[i].ID == prev.ID && all[i+1].ID == next.ID {
			continue
		}
		if all[i].End.IsZero() {
			continue
		}
		_ = e.settlePair(all[i], all[i+1], ctx, nil)
	}
	return res, nil
}

// settlePair 对一对相邻赛季执行全分片结转。
func (e *Engine) settlePair(prev, next SeasonConfig, ctx context.Context, res *SeasonSettlement) error {
	for _, sh := range e.shards {
		if err := ctx.Err(); err != nil {
			return err
		}
		sh.mu.Lock()
		sums, err := e.settleShard(sh, prev, next)
		sh.mu.Unlock()
		if err != nil {
			return err
		}
		if res != nil {
			res.PlayerSummaries = append(res.PlayerSummaries, sums...)
		}
	}
	if res != nil {
		res.PlayerCount = len(res.PlayerSummaries)
		sort.Slice(res.PlayerSummaries, func(i, j int) bool {
			return res.PlayerSummaries[i].PlayerID < res.PlayerSummaries[j].PlayerID
		})
	}
	return nil
}

// settleShard 在单个分片内完成旧赛季账本结转（调用方持该分片写锁）。
// 每个玩家一条原子 rollover 记录，同时完成：旧账本冻结、新账本种子、WAL 提交。
// 任一条记录落盘失败即返回错误，未提交的玩家在重试/重启时原样续做。
func (e *Engine) settleShard(sh *shard, prev, next SeasonConfig) ([]SettledPlayer, error) {
	var sums []SettledPlayer
	keys := make([]ppKey, 0)
	for k, p := range sh.players {
		if k.season == prev.ID && !p.Settled {
			keys = append(keys, k)
		}
	}
	sort.Slice(keys, func(i, j int) bool { return keys[i].player < keys[j].player })
	for _, k := range keys {
		old := sh.players[k]
		np := sh.getOrCreate(k.player, next)
		carry := int64(float64(old.EffectiveProgress) * prev.CarryRatio)
		if carry < 0 {
			carry = 0
		}
		var unclaimed []int
		for lvl := 1; lvl <= old.Level; lvl++ {
			if _, ok := old.ClaimedLevels[lvl]; !ok {
				unclaimed = append(unclaimed, lvl)
			}
		}
		rec := walRecord{
			Kind: recPlayerSettled, PlayerID: k.player,
			SeasonID: prev.ID, FromSeason: prev.ID, NextSeason: next.ID, RuleVersion: next.RuleVersion,
			FinalLevel: old.Level, Effective: old.EffectiveProgress,
			CarriedOver: carry, UnclaimedLevels: unclaimed,
		}
		if err := sh.store.append(rec); err != nil {
			return sums, err
		}
		old.Settled = true
		np.CarriedOver += carry
		np.EffectiveProgress += carry
		np.SeasonGranted += carry
		if lvl := levelFor(np.EffectiveProgress, next.LevelThresholds); lvl > np.Level {
			np.Level = lvl
		}
		sums = append(sums, SettledPlayer{
			PlayerID: k.player, FinalLevel: old.Level,
			EffectiveProgress: old.EffectiveProgress, CarriedOver: carry,
			UnclaimedLevels: unclaimed,
		})
	}
	return sums, nil
}

func (e *Engine) prevSeasonLocked(anchor time.Time) *SeasonConfig {
	var found *SeasonConfig
	for _, id := range e.seasons.list() {
		s, ok := e.seasons.get(id)
		if !ok {
			continue
		}
		if !s.End.IsZero() && !s.End.After(anchor) {
			if found == nil || s.Start.After(found.Start) {
				cp := *s
				found = &cp
			}
		}
	}
	return found
}

// resumePendingSettlements 重启后对“新赛季配置已落盘、但旧赛季仍有未冻结账本”
// 的情况自动续做结转，保证已提交/未完成状态可区分、进度不丢不重。
func (e *Engine) resumePendingSettlements() {
	e.seasonsMu.RLock()
	ids := e.seasons.list()
	var pairs [][2]SeasonID
	for _, id := range ids {
		s, ok := e.seasons.get(id)
		if !ok {
			continue
		}
		if prev := e.prevSeasonLocked(s.Start); prev != nil && prev.ID != s.ID {
			pairs = append(pairs, [2]SeasonID{prev.ID, s.ID})
		}
	}
	seasons := make(map[SeasonID]SeasonConfig)
	for _, id := range ids {
		if s, ok := e.seasons.get(id); ok {
			seasons[id] = *s
		}
	}
	e.seasonsMu.RUnlock()
	for _, pair := range pairs {
		prev := seasons[pair[0]]
		next := seasons[pair[1]]
		_ = e.settlePair(prev, next, context.Background(), nil)
	}
}

// ---------- 视图 / 维护 ----------

// View 返回某玩家当前对外可见的自洽版本快照（默认按墙钟时间所在赛季；
// 该赛季尚无账本时返回最近一个有账本的赛季）。
func (e *Engine) View(player PlayerID) (*PlayerView, error) {
	if err := e.ensureOpen(); err != nil {
		return nil, err
	}
	sh := e.shards[shardIndex(player, e.n)]
	sh.mu.RLock()
	defer sh.mu.RUnlock()

	var picked *playerProgress
	e.seasonsMu.RLock()
	if id, ok := e.seasons.activeID(e.clock()); ok {
		picked = sh.get(player, id)
	}
	e.seasonsMu.RUnlock()
	if picked == nil {
		all := sh.listPlayerSeasons(player)
		sort.Slice(all, func(i, j int) bool { return all[i].SeasonID > all[j].SeasonID })
		if len(all) > 0 {
			picked = all[0]
		} else {
			return nil, ErrSeasonNotFound
		}
	}
	claimed := make([]int, 0, len(picked.ClaimedLevels))
	for lvl := range picked.ClaimedLevels {
		claimed = append(claimed, lvl)
	}
	sort.Ints(claimed)
	daily := make(map[string]int64, len(picked.DailyGranted))
	for d, v := range picked.DailyGranted {
		daily[d] = v
	}
	status := "active"
	if picked.Settled {
		status = "settled"
	}
	return &PlayerView{
		PlayerID: player, SeasonID: picked.SeasonID, Status: status,
		RawProgress: picked.RawProgress, EffectiveProgress: picked.EffectiveProgress,
		Level: picked.Level, SeasonGranted: picked.SeasonGranted,
		DailyGranted: daily, ClaimedLevels: claimed,
	}, nil
}

// EvictSettled 显式淘汰一个已结算赛季的账本与去重集合（内存上限保障）。
func (e *Engine) EvictSettled(id SeasonID) error {
	if err := e.ensureOpen(); err != nil {
		return err
	}
	for _, sh := range e.shards {
		sh.mu.Lock()
		sh.dropSeason(id)
		sh.mu.Unlock()
	}
	return nil
}

// Snapshot 对元数据和全部分片做一次完整快照并截断各自 WAL。
func (e *Engine) Snapshot() error {
	if err := e.ensureOpen(); err != nil {
		return err
	}
	e.rulesMu.RLock()
	ruleSets := make([]RuleSet, 0)
	for _, v := range e.rules.byVersion {
		ruleSets = append(ruleSets, *v)
	}
	e.rulesMu.RUnlock()
	e.seasonsMu.RLock()
	var seasonCfgs []SeasonConfig
	for _, id := range e.seasons.list() {
		if s, ok := e.seasons.get(id); ok {
			seasonCfgs = append(seasonCfgs, *s)
		}
	}
	e.seasonsMu.RUnlock()
	ms := metaSnapshot{Rules: ruleSets, Seasons: seasonCfgs}
	if err := e.meta.saveSnapshot(ms); err != nil {
		return err
	}
	for _, sh := range e.shards {
		sh.mu.Lock()
		err := sh.store.saveSnapshot(sh.snapshotState())
		sh.mu.Unlock()
		if err != nil {
			return err
		}
	}
	return nil
}

func (e *Engine) Close() error {
	e.closedMu.Lock()
	if e.closed {
		e.closedMu.Unlock()
		return nil
	}
	e.closed = true
	e.closedMu.Unlock()
	close(e.stopJanitor)
	e.wg.Wait()
	for _, sh := range e.shards {
		if err := sh.store.close(); err != nil {
			return err
		}
	}
	return e.meta.close()
}
