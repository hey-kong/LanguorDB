package languorDB

import (
	"encoding/binary"
	"io"
	"log"

	"LanguorDB/config"
	"LanguorDB/internalkey"
	"LanguorDB/memtable"
	"LanguorDB/sstable"
	"LanguorDB/utils"
)

type Compaction struct {
	level  int
	inputs [2][]*FileMetaData
}

func (c *Compaction) isTrivialMove() bool {
	return len(c.inputs[0]) == 1 && len(c.inputs[1]) == 0
}

func (c *Compaction) Log() {
	log.Printf("Compaction, level:%d", c.level)
	for i := 0; i < len(c.inputs[0]); i++ {
		log.Printf("inputs[0]: %d", c.inputs[0][i].number)
	}
	for i := 0; i < len(c.inputs[1]); i++ {
		log.Printf("inputs[1]: %d", c.inputs[1][i].number)
	}
}

func (meta *FileMetaData) EncodeTo(w io.Writer) error {
	binary.Write(w, binary.LittleEndian, meta.allowSeeks)
	binary.Write(w, binary.LittleEndian, meta.fileSize)
	binary.Write(w, binary.LittleEndian, meta.number)
	meta.smallest.EncodeTo(w)
	meta.largest.EncodeTo(w)
	return nil
}

func (meta *FileMetaData) DecodeFrom(r io.Reader) error {
	binary.Read(r, binary.LittleEndian, &meta.allowSeeks)
	binary.Read(r, binary.LittleEndian, &meta.fileSize)
	binary.Read(r, binary.LittleEndian, &meta.number)
	meta.smallest = new(internalkey.InternalKey)
	meta.smallest.DecodeFrom(r)
	meta.largest = new(internalkey.InternalKey)
	meta.largest.DecodeFrom(r)
	return nil
}

func (v *Version) EncodeTo(w io.Writer) error {
	binary.Write(w, binary.LittleEndian, v.nextFileNumber)
	binary.Write(w, binary.LittleEndian, v.seq)
	for level := 0; level < config.NumLevels; level++ {
		numFiles := len(v.files[level])
		binary.Write(w, binary.LittleEndian, int32(numFiles))

		for i := 0; i < numFiles; i++ {
			v.files[level][i].EncodeTo(w)
		}
	}
	return nil
}

func (v *Version) DecodeFrom(r io.Reader) error {
	binary.Read(r, binary.LittleEndian, &v.nextFileNumber)
	binary.Read(r, binary.LittleEndian, &v.seq)
	var numFiles int32
	for level := 0; level < config.NumLevels; level++ {
		binary.Read(r, binary.LittleEndian, &numFiles)
		v.files[level] = make([]*FileMetaData, numFiles)
		for i := 0; i < int(numFiles); i++ {
			var meta FileMetaData
			meta.DecodeFrom(r)
			v.files[level][i] = &meta
		}
	}
	return nil
}

func (v *Version) deleteFile(level int, meta *FileMetaData) {
	numFiles := len(v.files[level])
	for i := 0; i < numFiles; i++ {
		if v.files[level][i].number == meta.number {
			v.files[level] = append(v.files[level][:i], v.files[level][i+1:]...)
			log.Printf("deleteFile, level:%d, num:%d", level, meta.number)
			break
		}
	}
}

func (v *Version) addFile(level int, meta *FileMetaData) {
	log.Printf("addFile, level:%d, num:%d, %s-%s", level, meta.number, string(meta.smallest.UserKey), string(meta.largest.UserKey))
	if level == 0 {
		// 0层没有排序
		v.files[level] = append(v.files[level], meta)
	} else {
		numFiles := len(v.files[level])
		index := v.findFile(v.files[level], meta.smallest.UserKey)
		if index >= numFiles {
			v.files[level] = append(v.files[level], meta)
		} else {
			var tmp []*FileMetaData
			tmp = append(tmp, v.files[level][:index]...)
			tmp = append(tmp, meta)
			v.files[level] = append(tmp, v.files[level][index:]...)
		}
	}
}

func (v *Version) WriteLevel0Table(imm *memtable.MemTable) {
	var meta FileMetaData
	meta.allowSeeks = 1 << 30
	meta.number = v.nextFileNumber
	v.nextFileNumber++
	builder := sstable.NewTableBuilder((utils.TableFileName(v.tableCache.dbName, meta.number)))
	iter := imm.NewIterator()
	iter.SeekToFirst()
	if iter.Valid() {
		meta.smallest = iter.InternalKey()
		for ; iter.Valid(); iter.Next() {
			meta.largest = iter.InternalKey()
			builder.Add(iter.InternalKey())
		}
		builder.Finish()
		meta.fileSize = uint64(builder.FileSize())
		meta.smallest.UserValue = nil
		meta.largest.UserValue = nil
	}

	// select appropriate level
	level := 0
	if !v.overlapInLevel(0, meta.smallest.UserKey, meta.largest.UserKey) {
		for ; level < config.MaxMemCompactLevel; level++ {
			if v.overlapInLevel(level+1, meta.smallest.UserKey, meta.largest.UserKey) {
				break
			}
		}
	}

	v.addFile(level, &meta)
}

func (v *Version) overlapInLevel(level int, smallestKey, largestKey []byte) bool {
	numFiles := len(v.files[level])
	if numFiles == 0 {
		return false
	}
	if level == 0 {
		for i := 0; i < numFiles; i++ {
			f := v.files[level][i]
			if internalkey.UserKeyComparator(smallestKey, f.largest.UserKey) > 0 || internalkey.UserKeyComparator(f.smallest.UserKey, largestKey) > 0 {
				continue
			} else {
				return true
			}
		}
	} else {
		index := v.findFile(v.files[level], smallestKey)
		if index >= numFiles {
			return false
		}
		if internalkey.UserKeyComparator(largestKey, v.files[level][index].smallest.UserKey) > 0 {
			return true
		}
	}
	return false
}

func (v *Version) DoCompactionWork() bool {
	c := v.pickCompaction()
	if c == nil {
		return false
	}
	log.Printf("DoCompactionWork begin\n")
	defer log.Printf("DoCompactionWork end\n")
	c.Log()
	if c.isTrivialMove() {
		// Move file to next level
		v.deleteFile(c.level, c.inputs[0][0])
		v.addFile(c.level+1, c.inputs[0][0])
		return true
	}
	var list []*FileMetaData
	var currentKey *internalkey.InternalKey
	iter := v.makeInputsIterator(c)
	for iter.SeekToFirst(); iter.Valid(); iter.Next() {
		var meta FileMetaData
		meta.allowSeeks = 1 << 30
		meta.number = v.nextFileNumber
		v.nextFileNumber++
		builder := sstable.NewTableBuilder((utils.TableFileName(v.tableCache.dbName, meta.number)))

		meta.smallest = iter.InternalKey()
		for ; iter.Valid(); iter.Next() {
			if currentKey != nil {
				// Remove duplicate KVs
				ret := internalkey.UserKeyComparator(iter.InternalKey().UserKey, currentKey.UserKey)
				if ret == 0 {
					continue
				} else if ret < 0 {
					log.Fatalf("%s < %s", string(iter.InternalKey().UserKey), string(currentKey.UserKey))
				}
				currentKey = iter.InternalKey()
			}
			meta.largest = iter.InternalKey()
			builder.Add(iter.InternalKey())
			if builder.FileSize() > config.MaxFileSize {
				break
			}
		}
		builder.Finish()
		meta.fileSize = uint64(builder.FileSize())
		meta.smallest.UserValue = nil
		meta.largest.UserValue = nil

		list = append(list, &meta)
	}

	for i := 0; i < len(c.inputs[0]); i++ {
		v.deleteFile(c.level, c.inputs[0][i])
	}
	for i := 0; i < len(c.inputs[1]); i++ {
		v.deleteFile(c.level+1, c.inputs[1][i])
	}
	for i := 0; i < len(list); i++ {
		v.addFile(c.level+1, list[i])
	}
	return true
}

func (v *Version) makeInputsIterator(c *Compaction) *MergingIterator {
	var list []*sstable.Iterator
	for i := 0; i < len(c.inputs[0]); i++ {
		list = append(list, v.tableCache.NewIterator(c.inputs[0][i].number))
	}
	for i := 0; i < len(c.inputs[1]); i++ {
		list = append(list, v.tableCache.NewIterator(c.inputs[1][i].number))
	}
	return NewMergingIterator(list)
}

func (v *Version) pickCompaction() *Compaction {
	var c Compaction
	c.level = v.pickCompactionLevel()
	if c.level < 0 {
		return nil
	}
	var smallest, largest *internalkey.InternalKey
	// Files in level 0 may overlap each other, so pick up all overlapping ones
	if c.level == 0 {
		c.inputs[0] = append(c.inputs[0], v.files[c.level]...)
		smallest = c.inputs[0][0].smallest
		largest = c.inputs[0][0].largest
		for i := 1; i < len(c.inputs[0]); i++ {
			f := c.inputs[0][i]
			if internalkey.InternalKeyComparator(f.largest, largest) > 0 {
				largest = f.largest
			}
			if internalkey.InternalKeyComparator(f.smallest, smallest) < 0 {
				smallest = f.smallest
			}
		}
	} else {
		// Pick the first file that comes after compact_pointer_[level]
		for i := 0; i < len(v.files[c.level]); i++ {
			f := v.files[c.level][i]
			if v.compactPointer[c.level] == nil || internalkey.InternalKeyComparator(f.largest, v.compactPointer[c.level]) > 0 {
				c.inputs[0] = append(c.inputs[0], f)
				break
			}
		}
		if len(c.inputs[0]) == 0 {
			c.inputs[0] = append(c.inputs[0], v.files[c.level][0])
		}
		smallest = c.inputs[0][0].smallest
		largest = c.inputs[0][0].largest
	}

	for i := 0; i < len(v.files[c.level+1]); i++ {
		f := v.files[c.level+1][i]

		if internalkey.InternalKeyComparator(f.largest, smallest) < 0 || internalkey.InternalKeyComparator(f.smallest, largest) > 0 {
			// "f" is completely before specified range; skip it,  // "f" is completely after specified range; skip it
		} else {
			c.inputs[1] = append(c.inputs[1], f)
		}
	}
	return &c
}

func (v *Version) pickCompactionLevel() int {
	// We treat level-0 specially by bounding the number of files
	// instead of number of bytes for two reasons:
	//
	// (1) With larger write-buffer sizes, it is nice not to do too
	// many level-0 compactions.
	//
	// (2) The files in level-0 are merged on every read and
	// therefore we wish to avoid too many files when the individual
	// file size is small (perhaps because of a small write-buffer
	// setting, or very high compression ratios, or lots of
	// overwrites/deletions).
	compactionLevel := -1
	bestScore := 1.0
	score := 0.0
	for level := 0; level < config.NumLevels-1; level++ {
		if level == 0 {
			score = float64(len(v.files[0])) / float64(config.L0_CompactionTrigger)
		} else {
			score = float64(totalFileSize(v.files[level])) / maxBytesForLevel(level)
		}

		if score > bestScore {
			bestScore = score
			compactionLevel = level
		}
	}
	return compactionLevel
}

func totalFileSize(files []*FileMetaData) uint64 {
	var sum uint64
	for i := 0; i < len(files); i++ {
		sum += files[i].fileSize
	}
	return sum
}

func maxBytesForLevel(level int) float64 {
	// Note: the result for level zero is not really used since we set
	// the level-0 compaction threshold based on number of files.

	// Result for both level-0 and level-1
	result := 10. * 1048576.0
	for level > 1 {
		result *= 10
		level--
	}
	return result
}
