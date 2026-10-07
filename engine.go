package engine

import (
	"container/list"
	"errors"
	"fmt"
	"sort"
	"sync"
	"time"
)

// 错误定义。
var (
	ErrNoSeason         = errors.New("no season covers event time")
	ErrSeasonNotStarted = errors.New("season not started yet")
	ErrClosed           = errors.New("engine closed")
)

// Config 引擎配置。
type Config struct {
	DataDir          string
	Rules            []RuleVersion
	Seasons          []SeasonConfig
	MaxCachedPlayers int           // 内存中缓存的玩家上限，超出 LRU 逐出（状态已写穿，不丢失）
	MaxGainRecords   int           // 每玩家保留的结算解释记录条数
	DayBucketsKept   int           // 每玩家保留的日桶数量
	Now              func() time.Time // 可注入时钟，便于测试；nil 用真实时间
}

func (c *Config) withDefaults() {
	if c.MaxCachedPlayers <= 0 {
		c.MaxCachedPlayers = 1024
	}
	if c.MaxGainRecords <= 0 {
		c.MaxGainRecords = 32
	}
	if c.DayBucketsKept <= 0 {
		c.DayBucketsKept = 7
	}
	if c.Now == nil {
		c.Now = time.Now
	}
}

const nStripes = 64

// Engine 玩赚成长进度与奖励结算引擎。
//
// 并发模型：
//   - 赛季归属读取用全局 RWMutex（赛季切换才取写锁，且切换只做元信息更新，结转惰性进行），
//     因此单赛季切换几乎不阻塞事件摄入；
//   - 每个玩家的状态变更由分片锁串行化，单玩家内部天然有序，不同玩家互不阻塞；
//   - 读取返回拷贝快照，任一时刻观察到的是某个自洽的完整版本。
type Engine struct {
	cfg     Config
	rules   *RuleSet
	seasons *SeasonManager
	store   *Store

	mu      sync.RWMutex // 保护赛季推进
	stripes [nStripes]sync.Mutex

	cacheMu sync.Mutex
	cache   map[string]*list.Element // playerID -> lru element(*PlayerState)
	lru     *list.List

	closed bool
}

// Open 打开（或创建）引擎，并从本地持久化恢复。
func Open(cfg Config) (*Engine, error) {
	cfg.withDefaults()
	rules, err := NewRuleSet(cfg.Rules)
	if err != nil {
		return nil, err
	}
	store, err := OpenStore(cfg.DataDir)
	if err != nil {
		return nil, err
	}
	e := &Engine{
		cfg:   cfg,
		rules: rules,
		store: store,
		cache: map[string]*list.Element{},
		lru:   list.New(),
	}
	if err := e.recover(); err != nil {
		store.Close()
		return nil, err
	}
	return e, nil
}

// recover 恢复赛季元信息，并重做未完成的赛季切换。
func (e *Engine) recover() error {
	meta, err := e.store.LoadSeasonMeta()
	if err != nil {
		return err
	}
	now := e.cfg.Now()
	if meta == nil {
		// 首次启动：按当前时间定位赛季
		sm, err := NewSeasonManager(e.cfg.Seasons, now)
		if err != nil {
			return err
		}
		e.seasons = sm
		meta = &SeasonMeta{}
		if a := sm.Active(); a != nil {
			meta.ActiveSeason = a.SeasonID
		}
		meta.Settled = sm.SettledIDs()
		if err := e.store.SaveSeasonMeta(meta); err != nil {
			return err
		}
		return nil
	}
	sm, err := NewSeasonManager(e.cfg.Seasons, now)
	if err != nil {
		return err
	}
	if err := sm.Restore(meta.ActiveSeason, meta.Settled); err != nil {
		return err
	}
	e.seasons = sm
	// 重做崩溃时未完成的切换（幂等：begin 载荷即目标元信息）
	if pend, err := e.store.PendingSwitch(); err != nil {
		return err
	} else if pend != nil {
		meta.ActiveSeason = pend.To
		meta.Settled = pend.Settled
		if err := e.store.SaveSeasonMeta(meta); err != nil {
			return err
		}
		if err := e.store.AppendSwitch(SwitchRecord{Kind: switchCommit, SwitchID: pend.SwitchID, To: pend.To}); err != nil {
			return err
		}
		if err := e.seasons.Restore(pend.To, pend.Settled); err != nil {
			return err
		}
	}
	return nil
}

// Close 关闭引擎。
func (e *Engine) Close() error {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.closed = true
	return e.store.Close()
}

func (e *Engine) stripe(id string) *sync.Mutex {
	h := uint32(0)
	for i := 0; i < len(id); i++ {
		h = h*31 + uint32(id[i])
	}
	return &e.stripes[h%nStripes]
}

// Ingest 摄入一个玩法事件，返回可解释的结算记录。
// 重复事件幂等忽略；已结算赛季的迟到事件不改写任何状态。
func (e *Engine) Ingest(ev Event) (GainRecord, error) {
	if ev.ID == "" || ev.PlayerID == "" {
		return GainRecord{}, fmt.Errorf("event id and player id required")
	}
	e.mu.RLock()
	if e.closed {
		e.mu.RUnlock()
		return GainRecord{}, ErrClosed
	}
	season := e.seasons.SeasonFor(ev.EventTime)
	if season == nil {
		e.mu.RUnlock()
		return GainRecord{}, ErrNoSeason
	}
	active := e.seasons.Active()
	if active == nil || season.SeasonID != active.SeasonID {
		e.mu.RUnlock()
		if e.seasons.IsSettled(season.SeasonID) || season.End.Before(activeTime(e)) {
			// 迟到事件：所属赛季已结算，结论不可改写
			return GainRecord{EventID: ev.ID, SeasonID: season.SeasonID, Raw: ev.Points, Effective: 0, Reason: "late"}, nil
		}
		return GainRecord{}, ErrSeasonNotStarted
	}
	rule, err := e.rules.ByVersion(season.RuleVersion)
	if err != nil {
		e.mu.RUnlock()
		return GainRecord{}, err
	}
	seasonCfg := *season
	e.mu.RUnlock()

	stripe := e.stripe(ev.PlayerID)
	stripe.Lock()
	defer stripe.Unlock()

	st, err := e.playerFor(ev.PlayerID, &seasonCfg)
	if err != nil {
		return GainRecord{}, err
	}
	if st.SeenEvents[ev.ID] {
		return GainRecord{EventID: ev.ID, SeasonID: st.SeasonID, Raw: ev.Points, Effective: 0, Reason: "duplicate",
			DayTotalAfter: st.DayProgress[dayKey(ev.EventTime)], SeasonTotalAfter: st.SeasonProgress}, nil
	}

	day := dayKey(ev.EventTime)
	dayBefore := st.DayProgress[day]
	eff, reason := computeGain(rule, dayBefore, st.SeasonProgress, ev.Points)

	st.SeenEvents[ev.ID] = true
	st.DayProgress[day] = dayBefore + float64(ev.Points)
	st.SeasonProgress += eff
	e.pruneDayBuckets(st, day)
	rec := GainRecord{
		EventID:          ev.ID,
		SeasonID:         st.SeasonID,
		Raw:              ev.Points,
		Effective:        eff,
		Reason:           reason,
		DayTotalAfter:    st.DayProgress[day],
		SeasonTotalAfter: st.SeasonProgress,
	}
	st.LastGains = append(st.LastGains, rec)
	if len(st.LastGains) > e.cfg.MaxGainRecords {
		st.LastGains = st.LastGains[len(st.LastGains)-e.cfg.MaxGainRecords:]
	}
	if err := e.store.SavePlayer(st); err != nil {
		return GainRecord{}, err
	}
	return rec, nil
}

// Claim 领取当前等级全部可领奖励。幂等：已发放的奖励不会重复发放，
// 重复领取时 Granted 为空、Already 还原首次发放口径。
func (e *Engine) Claim(playerID string) (ClaimResult, error) {
	e.mu.RLock()
	if e.closed {
		e.mu.RUnlock()
		return ClaimResult{}, ErrClosed
	}
	active := e.seasons.Active()
	if active == nil {
		e.mu.RUnlock()
		return ClaimResult{}, ErrSeasonNotStarted
	}
	seasonCfg := *active
	rule, err := e.rules.ByVersion(active.RuleVersion)
	if err != nil {
		e.mu.RUnlock()
		return ClaimResult{}, err
	}
	e.mu.RUnlock()

	stripe := e.stripe(playerID)
	stripe.Lock()
	defer stripe.Unlock()

	st, err := e.playerFor(playerID, &seasonCfg)
	if err != nil {
		return ClaimResult{}, err
	}
	level := LevelFor(rule, st.SeasonProgress)
	res := ClaimResult{PlayerID: playerID, SeasonID: st.SeasonID, Level: level}
	changed := false
	for _, rw := range RewardsUpTo(rule, level) {
		key := st.SeasonID + "/" + rw.RewardID
		if st.Claimed[key] {
			res.Already = append(res.Already, rw)
			continue
		}
		st.Claimed[key] = true
		st.ClaimedRewards = append(st.ClaimedRewards, rw)
		res.Granted = append(res.Granted, rw)
		changed = true
	}
	if changed {
		if err := e.store.SavePlayer(st); err != nil {
			return ClaimResult{}, err
		}
	}
	return res, nil
}

// View 读取玩家当前一致性快照。
func (e *Engine) View(playerID string) (PlayerView, error) {
	e.mu.RLock()
	if e.closed {
		e.mu.RUnlock()
		return PlayerView{}, ErrClosed
	}
	active := e.seasons.Active()
	if active == nil {
		e.mu.RUnlock()
		return PlayerView{}, ErrSeasonNotStarted
	}
	seasonCfg := *active
	rule, err := e.rules.ByVersion(active.RuleVersion)
	if err != nil {
		e.mu.RUnlock()
		return PlayerView{}, err
	}
	e.mu.RUnlock()

	stripe := e.stripe(playerID)
	stripe.Lock()
	defer stripe.Unlock()

	st, err := e.playerFor(playerID, &seasonCfg)
	if err != nil {
		return PlayerView{}, err
	}
	var dayTotal float64
	for _, v := range st.DayProgress {
		dayTotal += v
	}
	return PlayerView{
		PlayerID:       st.PlayerID,
		SeasonID:       st.SeasonID,
		SeasonProgress: st.SeasonProgress,
		DayProgress:    dayTotal,
		Level:          LevelFor(rule, st.SeasonProgress),
		RuleVersion:    seasonCfg.RuleVersion,
	}, nil
}

// SwitchSeason 结算当前赛季并推进到覆盖 now 的赛季。
// 两阶段提交：先写 begin 日志，再原子更新 season.json，最后写 commit。
// 进度结转是惰性的：玩家下次被访问时按规则一次性结转，切换本身 O(1)。
func (e *Engine) SwitchSeason(now time.Time) (string, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.closed {
		return "", ErrClosed
	}
	_, next, err := e.seasons.Advance(now)
	if err != nil {
		return "", err
	}
	meta := &SeasonMeta{ActiveSeason: next.SeasonID, Settled: e.seasons.SettledIDs()}
	switchID := fmt.Sprintf("%s-%d", next.SeasonID, now.UnixNano())
	if err := e.store.AppendSwitch(SwitchRecord{Kind: switchBegin, SwitchID: switchID, To: next.SeasonID, Settled: meta.Settled}); err != nil {
		return "", err
	}
	if err := e.store.SaveSeasonMeta(meta); err != nil {
		return "", err
	}
	if err := e.store.AppendSwitch(SwitchRecord{Kind: switchCommit, SwitchID: switchID, To: next.SeasonID}); err != nil {
		return "", err
	}
	return next.SeasonID, nil
}

// playerFor 取玩家状态（缓存或磁盘），并在其落后于活跃赛季时惰性结转。
// 调用方必须持有该玩家的分片锁。
func (e *Engine) playerFor(playerID string, active *SeasonConfig) (*PlayerState, error) {
	st := e.cacheGet(playerID)
	if st == nil {
		loaded, err := e.store.LoadPlayer(playerID)
		if err != nil {
			return nil, err
		}
		if loaded == nil {
			st = &PlayerState{
				PlayerID:    playerID,
				SeasonID:    active.SeasonID,
				DayProgress: map[string]float64{},
				SeenEvents:  map[string]bool{},
				Claimed:     map[string]bool{},
			}
		} else {
			st = loaded
			if st.DayProgress == nil {
				st.DayProgress = map[string]float64{}
			}
			if st.SeenEvents == nil {
				st.SeenEvents = map[string]bool{}
			}
			if st.Claimed == nil {
				st.Claimed = map[string]bool{}
			}
		}
	}
	// 惰性跨赛季结转：从玩家停留的赛季起，逐赛季应用重置比例
	if st.SeasonID != active.SeasonID {
		progress := e.carryChain(st.SeasonID, active.SeasonID, st.SeasonProgress)
		st.SeasonID = active.SeasonID
		st.SeasonProgress = progress
		st.DayProgress = map[string]float64{}
		st.SeenEvents = map[string]bool{}
		st.LastGains = nil
		if err := e.store.SavePlayer(st); err != nil {
			return nil, err
		}
	}
	e.cachePut(st)
	return st, nil
}

// carryChain 从 fromSeason 之后到 toSeason 为止，逐赛季乘上被退出赛季的重置比例。
func (e *Engine) carryChain(fromSeason, toSeason string, progress float64) float64 {
	started := false
	for i := range e.seasons.seasons {
		sc := &e.seasons.seasons[i]
		if sc.SeasonID == fromSeason {
			started = true
		}
		if !started {
			continue
		}
		if sc.SeasonID == toSeason {
			break
		}
		progress = progress * sc.CarryRatio
	}
	return progress
}

func dayKey(t time.Time) string {
	return t.UTC().Format("2006-01-02")
}

func activeTime(e *Engine) time.Time { return e.cfg.Now() }

// pruneDayBuckets 只保留最近 DayBucketsKept 个日桶，防止长期运行内存膨胀。
func (e *Engine) pruneDayBuckets(st *PlayerState, currentDay string) {
	if len(st.DayProgress) <= e.cfg.DayBucketsKept {
		return
	}
	keys := make([]string, 0, len(st.DayProgress))
	for k := range st.DayProgress {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys[:len(keys)-e.cfg.DayBucketsKept] {
		delete(st.DayProgress, k)
	}
}

func (e *Engine) cacheGet(id string) *PlayerState {
	e.cacheMu.Lock()
	defer e.cacheMu.Unlock()
	if el, ok := e.cache[id]; ok {
		e.lru.MoveToFront(el)
		return el.Value.(*PlayerState)
	}
	return nil
}

func (e *Engine) cachePut(st *PlayerState) {
	e.cacheMu.Lock()
	defer e.cacheMu.Unlock()
	if el, ok := e.cache[st.PlayerID]; ok {
		e.lru.MoveToFront(el)
		return
	}
	el := e.lru.PushFront(st)
	e.cache[st.PlayerID] = el
	for e.lru.Len() > e.cfg.MaxCachedPlayers {
		back := e.lru.Back()
		if back == nil {
			break
		}
		victim := back.Value.(*PlayerState)
		delete(e.cache, victim.PlayerID) // 状态已写穿，逐出不丢数据
		e.lru.Remove(back)
	}
}
