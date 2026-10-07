package engine

import (
	"bufio"
	"encoding/json"
	"errors"
	"io"
	"os"
	"sort"
)

func (s *store) snapshotFilePath() string { return s.snapPath() }

func (s *store) readRecords() ([]walRecord, error) {
	if s.inMem {
		return nil, nil
	}
	f, err := os.Open(s.logPath())
	if err != nil {
		return nil, err
	}
	defer f.Close()
	var out []walRecord
	dec := json.NewDecoder(bufio.NewReader(f))
	for {
		var rec walRecord
		if err := dec.Decode(&rec); err != nil {
			if err == io.EOF {
				break
			}
			// 崩溃可能只在文件尾部留下不完整的一行（torn write）：
			// 该记录从未完成 fsync 提交，忽略尾部即可；
			// 若其后仍有数据则属于真正损坏，报错而不是静默吞掉。
			if isTrailingTruncation(err, f) {
				break
			}
			return nil, err
		}
		out = append(out, rec)
	}
	return out, nil
}

// isTrailingTruncation 判断解码错误是否恰好发生在文件末尾的不完整记录上。
func isTrailingTruncation(err error, f *os.File) bool {
	if err == io.EOF {
		return true
	}
	if err == io.ErrUnexpectedEOF {
		if pos, perr := f.Seek(0, io.SeekCurrent); perr == nil {
			if end, serr := f.Seek(0, io.SeekEnd); serr == nil {
				_, _ = f.Seek(pos, io.SeekStart)
				return pos < end
			}
		}
		return true
	}
	// json 语法错误也只可能是尾部撕裂；非尾部损坏会因后面还有合法行
	// 而无法被整体判定，这里保守地仅接受“错误点已到文件尾”的情况。
	var se *json.SyntaxError
	if errors.As(err, &se) {
		if end, serr := f.Seek(0, io.SeekEnd); serr == nil {
			return se.Offset >= end-1
		}
	}
	return false
}

// restore 从最近一次快照 + 其后的 WAL 重建全部内存状态。
// 快照是完整版本，日志中每条记录都是已提交的状态迁移，
// 恢复后不会出现进度跳变、重复发放或半份领取残留。
func (e *Engine) restore() error {
	if ms, ok, err := loadSnapshot[metaSnapshot](e.meta.snapshotFilePath()); err != nil {
		return err
	} else if ok {
		for _, rs := range ms.Rules {
			if err := e.rules.register(rs); err != nil {
				return err
			}
		}
		for _, sc := range ms.Seasons {
			if err := e.seasons.add(sc); err != nil {
				return err
			}
		}
	}
	metaRecs, err := e.meta.readRecords()
	if err != nil {
		return err
	}
	for _, rec := range metaRecs {
		switch rec.Kind {
		case recRuleRegistered:
			if rec.Rule != nil {
				_ = e.rules.register(*rec.Rule)
			}
		case recSeasonAdded:
			if rec.Season != nil {
				_ = e.seasons.add(*rec.Season)
			}
		}
	}

	for _, sh := range e.shards {
		if snap, ok, err := loadSnapshot[shardSnapshot](sh.store.snapshotFilePath()); err != nil {
			return err
		} else if ok {
			sh.restoreSnapshot(snap)
		}
		recs, err := sh.store.readRecords()
		if err != nil {
			return err
		}
		if err := e.replayShard(sh, recs); err != nil {
			return err
		}
	}
	return nil
}

func (e *Engine) replayShard(sh *shard, recs []walRecord) error {
	committed := map[string]bool{}
	aborted := map[string]bool{}
	for _, rec := range recs {
		if rec.Claim != nil {
			switch rec.Kind {
			case recClaimCommitted:
				committed[rec.Claim.ClaimID] = true
			case recClaimAborted:
				aborted[rec.Claim.ClaimID] = true
			}
		}
	}
	// 快照内可能已带未完成的 prepared（快照本身只捕获完整状态，
	// prepared 不是发放结论，但需要被正确终结）。
	snapshotPending := map[string]bool{}
	for _, p := range sh.players {
		if p.PendingClaim != nil {
			snapshotPending[p.PendingClaim.ClaimID] = true
		}
	}

	for _, rec := range recs {
		if err := e.applyReplay(sh, rec); err != nil {
			return err
		}
	}

	// 崩溃恢复：只有 prepared、没有 committed 的领取一律终结为 aborted，
	// 并把 abort 写回 WAL，绝不残留半份记录。
	for _, rec := range recs {
		if rec.Kind == recClaimPrepared && rec.Claim != nil &&
			!committed[rec.Claim.ClaimID] && !aborted[rec.Claim.ClaimID] {
			ab := *rec.Claim
			ab.Status = ClaimAborted
			if err := sh.store.append(walRecord{Kind: recClaimAborted, Claim: &ab}); err != nil {
				return err
			}
			aborted[ab.ClaimID] = true
		}
	}
	for _, p := range sh.players {
		if p.PendingClaim != nil &&
			(committed[p.PendingClaim.ClaimID] || aborted[p.PendingClaim.ClaimID]) {
			p.PendingClaim = nil
		}
	}
	for id := range snapshotPending {
		if !committed[id] && !aborted[id] {
			for _, p := range sh.players {
				if p.PendingClaim != nil && p.PendingClaim.ClaimID == id {
					p.PendingClaim = nil
				}
			}
		}
	}
	return nil
}

func (e *Engine) applyReplay(sh *shard, rec walRecord) error {
	switch rec.Kind {
	case recEventAccepted:
		sc, ok := e.seasons.get(rec.SeasonID)
		if !ok || rec.Breakdown == nil {
			return nil
		}
		p := sh.getOrCreate(rec.PlayerID, *sc)
		b := rec.Breakdown
		p.RawProgress += b.Gross
		p.EffectiveProgress += b.Granted
		p.SeasonGranted += b.Granted
		p.DailyGranted[b.DayBucket] += b.Granted
		n := rec.OccurredAt.UnixNano()
		idx := sort.Search(len(p.EventTimes), func(i int) bool { return p.EventTimes[i] > n })
		p.EventTimes = append(p.EventTimes, 0)
		copy(p.EventTimes[idx+1:], p.EventTimes[idx:])
		p.EventTimes[idx] = n
		if rec.LevelAfter > p.Level {
			p.Level = rec.LevelAfter
		}
		sh.dedup.mark(rec.SeasonID, rec.PlayerID, b.EventID)

	case recClaimPrepared:
		if rec.Claim == nil {
			return nil
		}
		sc, ok := e.seasons.get(rec.Claim.SeasonID)
		if !ok {
			return nil
		}
		p := sh.getOrCreate(rec.Claim.PlayerID, *sc)
		cp := *rec.Claim
		p.PendingClaim = &cp

	case recClaimCommitted:
		if rec.Claim == nil {
			return nil
		}
		sc, ok := e.seasons.get(rec.Claim.SeasonID)
		if !ok {
			return nil
		}
		p := sh.getOrCreate(rec.Claim.PlayerID, *sc)
		cp := *rec.Claim
		p.ClaimedLevels[cp.Level] = cp.ClaimID
		p.PendingClaim = nil

	case recClaimAborted:
		if rec.Claim == nil {
			return nil
		}
		if p := sh.get(rec.Claim.PlayerID, rec.Claim.SeasonID); p != nil &&
			p.PendingClaim != nil && p.PendingClaim.ClaimID == rec.Claim.ClaimID {
			p.PendingClaim = nil
		}

	case recPlayerSettled:
		sc, ok := e.seasons.get(rec.SeasonID)
		if !ok {
			return nil
		}
		old := sh.getOrCreate(rec.PlayerID, *sc)
		if old.Settled {
			return nil // 结转幂等：已提交的不重复计入
		}
		nextSC, ok2 := e.seasons.get(rec.NextSeason)
		if !ok2 {
			return nil
		}
		np := sh.getOrCreate(rec.PlayerID, *nextSC)
		old.Settled = true
		if rec.FinalLevel > old.Level {
			old.Level = rec.FinalLevel
		}
		np.CarriedOver += rec.CarriedOver
		np.EffectiveProgress += rec.CarriedOver
		np.SeasonGranted += rec.CarriedOver
		if lvl := levelFor(np.EffectiveProgress, nextSC.LevelThresholds); lvl > np.Level {
			np.Level = lvl
		}
	}
	return nil
}
