package languorDB

import (
	"log"
	"os"
	"sort"

	"LanguorDB/config"
	"LanguorDB/errors"
	"LanguorDB/internalkey"
	"LanguorDB/utils"
)

type FileMetaData struct {
	allowSeeks uint64
	number     uint64
	fileSize   uint64
	smallest   *internalkey.InternalKey
	largest    *internalkey.InternalKey
}

type Metas []*FileMetaData

func (arr Metas) Len() int {
	return len(arr)
}

func (arr Metas) Less(i, j int) bool {
	ret := internalkey.UserKeyComparator(arr[i].smallest.UserKey, arr[j].smallest.UserKey)
	if ret == 0 {
		return arr[i].number < arr[j].number
	}
	return ret == -1
}

func (arr Metas) Swap(i, j int) {
	arr[i], arr[j] = arr[j], arr[i]
}

type Version struct {
	tableCache     *TableCache
	nextFileNumber uint64
	seq            uint64
	files          [config.NumLevels][]*FileMetaData
	// Per-level internalkey at which the next compaction at that level should start.
	// Either an empty string, or a valid InternalKey.
	compactPointer [config.NumLevels]*internalkey.InternalKey

	// Coarse-grain compaction index.
	index [config.NumLevels]*Index
}

func New(dbName string) *Version {
	var v Version
	v.tableCache = NewTableCache(dbName)
	v.nextFileNumber = 1
	return &v
}

func Load(dbName string, number uint64) (*Version, error) {
	fileName := utils.DescriptorFileName(dbName, number)
	file, err := os.Open(fileName)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	v := New(dbName)
	return v, v.DecodeFrom(file)
}

func (v *Version) Save() (uint64, error) {
	tmp := v.nextFileNumber
	fileName := utils.DescriptorFileName(v.tableCache.dbName, v.nextFileNumber)
	v.nextFileNumber++
	file, err := os.Create(fileName)
	if err != nil {
		return tmp, err
	}
	defer file.Close()
	return tmp, v.EncodeTo(file)
}

func (v *Version) Log() {
	for level := 0; level < config.NumLevels; level++ {
		for i := 0; i < len(v.files[level]); i++ {
			log.Printf("version[%d]: %d", level, v.files[level][i].number)
		}
	}
}
func (v *Version) Copy() *Version {
	var c Version

	c.tableCache = v.tableCache
	c.nextFileNumber = v.nextFileNumber
	c.seq = v.seq
	for level := 0; level < config.NumLevels; level++ {
		c.files[level] = make([]*FileMetaData, len(v.files[level]))
		copy(c.files[level], v.files[level])
	}
	return &c
}
func (v *Version) NextSeq() uint64 {
	v.seq++
	return v.seq
}

func (v *Version) NumLevelFiles(l int) int {
	return len(v.files[l])
}

func (v *Version) Get(key []byte) ([]byte, error) {
	var tmp []*FileMetaData
	var tmp2 [1]*FileMetaData
	var files []*FileMetaData
	// We can search level-by-level since entries never hop across
	// levels.  Therefore we are guaranteed that if we find data
	// in an smaller level, later levels are irrelevant.
	for level := 0; level < config.NumLevels; level++ {
		numFiles := len(v.files[level])
		if numFiles == 0 {
			continue
		}
		if level == 0 {
			// Level-0 files may overlap each other.  Find all files that
			// overlap user_key and process them in order from newest to oldest.
			for i := 0; i < numFiles; i++ {
				f := v.files[level][i]
				if internalkey.UserKeyComparator(key, f.smallest.UserKey) >= 0 && internalkey.UserKeyComparator(key, f.largest.UserKey) <= 0 {
					tmp = append(tmp, f)
				}
			}
			if len(tmp) == 0 {
				continue
			}
			sort.Slice(tmp, func(i, j int) bool { return tmp[i].number > tmp[j].number })
			numFiles = len(tmp)
			files = tmp
		} else {
			index := v.findFile(v.files[level], key)
			if index >= numFiles {
				files = nil
				numFiles = 0
			} else {
				tmp2[0] = v.files[level][index]
				if internalkey.UserKeyComparator(key, tmp2[0].smallest.UserKey) < 0 {
					files = nil
					numFiles = 0
				} else {
					files = tmp2[:]
					numFiles = 1
				}
			}
		}
		for i := 0; i < numFiles; i++ {
			f := files[i]
			value, err := v.tableCache.Get(f.number, key)
			if err != errors.ErrNotFound {
				return value, err
			}
		}
	}
	return nil, errors.ErrNotFound
}

func (v *Version) findFile(files []*FileMetaData, key []byte) int {
	left := 0
	right := len(files)
	for left < right {
		mid := (left + right) / 2
		f := files[mid]
		if internalkey.UserKeyComparator(f.largest.UserKey, key) < 0 {
			// Key at "mid.largest" is < "target".  Therefore all
			// files at or before "mid" are uninteresting.
			left = mid + 1
		} else {
			// Key at "mid.largest" is >= "target".  Therefore all files
			// after "mid" are uninteresting.
			right = mid
		}
	}
	return right
}
