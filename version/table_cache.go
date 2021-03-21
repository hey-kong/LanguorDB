package version

import (
	"sync"

	"LanguorDB/config"
	"LanguorDB/sstable"
	"LanguorDB/utils"
	"github.com/hashicorp/golang-lru"
)

type TableCache struct {
	mu     sync.Mutex
	dbName string
	cache  *lru.Cache
}

func NewTableCache(dbName string) *TableCache {
	var tableCache TableCache
	tableCache.dbName = dbName
	tableCache.cache, _ = lru.New(config.MaxOpenFiles - config.NumNonTableCacheFiles)
	return &tableCache
}

func (tableCache *TableCache) NewIterator(fileNum uint64) *sstable.Iterator {
	table, _ := tableCache.findTable(fileNum)
	if table != nil {
		return table.NewIterator()
	}
	return nil
}

func (tableCache *TableCache) Get(fileNum uint64, key []byte) ([]byte, error) {
	table, err := tableCache.findTable(fileNum)
	if table != nil {
		return table.Get(key)
	}

	return nil, err
}

func (tableCache *TableCache) Evict(fileNum uint64) {
	tableCache.cache.Remove(fileNum)
}

func (tableCache *TableCache) findTable(fileNum uint64) (*sstable.SSTable, error) {
	tableCache.mu.Lock()
	defer tableCache.mu.Unlock()
	table, ok := tableCache.cache.Get(fileNum)
	if ok {
		return table.(*sstable.SSTable), nil
	} else {
		ssTable, err := sstable.Open(utils.TableFileName(tableCache.dbName, fileNum))
		tableCache.cache.Add(fileNum, ssTable)
		return ssTable, err
	}
}
