package engine

// dedupKey 定位某个玩家在某个赛季内的事件集合。
type dedupKey struct {
	season SeasonID
	player PlayerID
}

// dedupIndex 以 (赛季, 玩家) 维度记录已被结算（对外可见）的事件 ID。
// 访问始终发生在该玩家所属分片的互斥区内，内部不再加锁。
// 事件一旦 mark 即永久生效（直到赛季数据被显式 EvictSettled），
// 因此乱序/重复/迟到到达都不会被重复累计。
type dedupIndex struct {
	sets map[dedupKey]map[string]struct{}
}

func newDedupIndex() *dedupIndex {
	return &dedupIndex{sets: map[dedupKey]map[string]struct{}{}}
}

func (d *dedupIndex) key(season SeasonID, player PlayerID) dedupKey {
	return dedupKey{season: season, player: player}
}

func (d *dedupIndex) has(season SeasonID, player PlayerID, eventID string) bool {
	set, ok := d.sets[d.key(season, player)]
	if !ok {
		return false
	}
	_, ok = set[eventID]
	return ok
}

// mark 记录事件；返回 false 表示此前已存在（重复事件）。
func (d *dedupIndex) mark(season SeasonID, player PlayerID, eventID string) bool {
	k := d.key(season, player)
	set, ok := d.sets[k]
	if !ok {
		set = map[string]struct{}{}
		d.sets[k] = set
	}
	if _, dup := set[eventID]; dup {
		return false
	}
	set[eventID] = struct{}{}
	return true
}

// dropSeason 在赛季数据被显式淘汰时释放对应内存，避免长期运行内存增长。
func (d *dedupIndex) dropSeason(season SeasonID) int {
	removed := 0
	for k := range d.sets {
		if k.season == season {
			removed += len(d.sets[k])
			delete(d.sets, k)
		}
	}
	return removed
}
