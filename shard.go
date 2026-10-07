package engine

import (
	"hash/fnv"
	"sort"
	"sync"
)

type ppKey struct {
	player PlayerID
	season SeasonID
}

// shard 是按玩家哈希分片的互斥单元：
//   - 同一玩家的摄入/领取/结算始终落在同一分片并串行化；
//   - 不同玩家多数情况下落在不同分片，高并发互不阻塞；
//   - 每个分片独立 WAL，日志写入也不会被其他分片阻塞。
type shard struct {
	id      uint32
	store   *store
	mu      sync.RWMutex // 单写多读：摄入/领取用 Lock，快照/视图用 RLock
	players map[ppKey]*playerProgress
	dedup   *dedupIndex
}

func newShard(id uint32, st *store) *shard {
	return &shard{
		id:      id,
		store:   st,
		players: map[ppKey]*playerProgress{},
		dedup:   newDedupIndex(),
	}
}

func shardIndex(id PlayerID, n int) uint32 {
	h := fnv.New32a()
	_, _ = h.Write([]byte(string(id)))
	return h.Sum32() % uint32(n)
}

// get 取玩家在某赛季的账本，不存在返回 nil（调用方持锁）。
func (s *shard) get(player PlayerID, season SeasonID) *playerProgress {
	return s.players[ppKey{player, season}]
}

// getOrCreate 取或建立玩家在某赛季的账本（调用方持写锁）。
func (s *shard) getOrCreate(player PlayerID, season SeasonConfig) *playerProgress {
	k := ppKey{player, season.ID}
	if p, ok := s.players[k]; ok {
		return p
	}
	p := newPlayerProgress(player, season.ID, season.RuleVersion)
	s.players[k] = p
	return p
}

// listPlayerSeasons 返回某玩家在本分片内全部赛季账本（调用方持读锁）。
func (s *shard) listPlayerSeasons(player PlayerID) []*playerProgress {
	var out []*playerProgress
	for k, p := range s.players {
		if k.player == player {
			out = append(out, p)
		}
	}
	return out
}

func (s *shard) snapshotState() shardSnapshot {
	snap := shardSnapshot{ShardID: s.id}
	keys := make([]ppKey, 0, len(s.players))
	for k := range s.players {
		keys = append(keys, k)
	}
	sortKeys(keys)
	for _, k := range keys {
		set := s.dedup.sets[dedupKey{season: k.season, player: k.player}]
		if len(set) == 0 {
			continue
		}
		ids := make([]string, 0, len(set))
		for id := range set {
			ids = append(ids, id)
		}
		sort.Strings(ids)
		snap.Dedup = append(snap.Dedup, dedupEntry{SeasonID: k.season, PlayerID: k.player, EventIDs: ids})
	}
	for _, k := range keys {
		snap.Players = append(snap.Players, s.players[k])
	}
	return snap
}

func sortKeys(keys []ppKey) {
	sort.Slice(keys, func(i, j int) bool {
		if keys[i].player != keys[j].player {
			return keys[i].player < keys[j].player
		}
		return keys[i].season < keys[j].season
	})
}

func (s *shard) restoreSnapshot(snap *shardSnapshot) {
	for _, e := range snap.Dedup {
		for _, id := range e.EventIDs {
			s.dedup.mark(e.SeasonID, e.PlayerID, id)
		}
	}
	for _, p := range snap.Players {
		if p.DailyGranted == nil {
			p.DailyGranted = map[string]int64{}
		}
		if p.ClaimedLevels == nil {
			p.ClaimedLevels = map[int]string{}
		}
		s.players[ppKey{p.PlayerID, p.SeasonID}] = p
	}
}

func (s *shard) dropSeason(id SeasonID) int {
	n := 0
	for k := range s.players {
		if k.season == id {
			delete(s.players, k)
			n++
		}
	}
	s.dedup.dropSeason(id)
	return n
}
