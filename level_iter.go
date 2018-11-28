// Copyright 2018 The LevelDB-Go and Pebble Authors. All rights reserved. Use
// of this source code is governed by a BSD-style license that can be found in
// the LICENSE file.

package pebble

import (
	"sort"

	"github.com/petermattis/pebble/db"
)

// tableNewIter creates a new iterator for the given file number.
type tableNewIter func(meta *fileMetadata) (internalIterator, error)

// levelIter provides a merged view of the sstables in a level.
//
// levelIter is used during compaction and as part of the Iterator
// implementation. When used as part of the Iterator implementation, level
// iteration needs to "pause" at sstable boundaries if a range deletion
// tombstone is the source of that boundary. We know if a range tombstone is
// the smallest or largest key in a file because the kind will be
// InternalKeyKindRangeDeletion. If the boundary key is a range deletion
// tombstone, we materialize a fake entry to return from levelIter. This
// prevents mergingIter from advancing past the sstable until the sstable
// contains the smallest (or largest for reverse iteration) key in the merged
// heap. Note that dbIter treat a range deletion tombstone as a no-op and
// processes range deletions via rangeDelMap.
type levelIter struct {
	opts  *db.IterOptions
	cmp   db.Compare
	index int
	// The key to return when iterating past an sstable boundary and that
	// boundary is a range deletion tombstone. Note that if boundary != nil, then
	// iter == nil, and if iter != nil, then boundary == nil.
	boundary        *db.InternalKey
	iter            internalIterator
	newIter         tableNewIter
	newRangeDelIter tableNewIter
	rangeDel        *rangeDelLevel
	files           []fileMetadata
	err             error
}

// levelIter implements the internalIterator interface.
var _ internalIterator = (*levelIter)(nil)

func newLevelIter(
	opts *db.IterOptions, cmp db.Compare, newIter tableNewIter, files []fileMetadata,
) *levelIter {
	l := &levelIter{}
	l.init(opts, cmp, newIter, files)
	return l
}

func (l *levelIter) init(
	opts *db.IterOptions, cmp db.Compare, newIter tableNewIter, files []fileMetadata,
) {
	l.opts = opts
	l.cmp = cmp
	l.index = -1
	l.newIter = newIter
	l.files = files
}

func (l *levelIter) initRangeDel(newRangeDelIter tableNewIter, rangeDel *rangeDelLevel) {
	l.newRangeDelIter = newRangeDelIter
	l.rangeDel = rangeDel
}

func (l *levelIter) findFileGE(key []byte) int {
	// Find the earliest file whose largest key is >= ikey.
	return sort.Search(len(l.files), func(i int) bool {
		return l.cmp(l.files[i].largest.UserKey, key) >= 0
	})
}

func (l *levelIter) findFileLT(key []byte) int {
	// Find the last file whose smallest key is < ikey.
	index := sort.Search(len(l.files), func(i int) bool {
		return l.cmp(l.files[i].smallest.UserKey, key) >= 0
	})
	return index - 1
}

func (l *levelIter) loadFile(index, dir int) bool {
	l.boundary = nil
	if l.index == index {
		return l.iter != nil
	}
	if l.iter != nil {
		l.err = l.iter.Close()
		if l.err != nil {
			return false
		}
		l.iter = nil
	}

	for ; ; index += dir {
		l.index = index
		if l.index < 0 || l.index >= len(l.files) {
			return false
		}

		f := &l.files[l.index]
		if lowerBound := l.opts.GetLowerBound(); lowerBound != nil {
			if l.cmp(f.largest.UserKey, lowerBound) < 0 {
				// The largest key in the sstable is smaller than the lower bound.
				if dir < 0 {
					return false
				}
				continue
			}
		}
		if upperBound := l.opts.GetUpperBound(); upperBound != nil {
			if l.cmp(f.smallest.UserKey, upperBound) >= 0 {
				// The smallest key in the sstable is greater than or equal to the
				// lower bound.
				if dir > 0 {
					return false
				}
				continue
			}
		}

		if l.rangeDel != nil {
			// TODO(peter,rangedel): If the table is entirely covered by a range
			// deletion tombstone, skip it.
		}

		l.iter, l.err = l.newIter(f)
		if l.err != nil || l.iter == nil {
			return false
		}
		if l.rangeDel != nil {
			iter, err := l.newRangeDelIter(f)
			if err != nil {
				l.err = err
				return false
			}
			l.rangeDel.init(iter)
		}
		return true
	}
}

func (l *levelIter) SeekGE(key []byte) {
	// NB: the top-level dbIter has already adjusted key based on
	// IterOptions.LowerBound.
	if l.loadFile(l.findFileGE(key), 1) {
		l.iter.SeekGE(key)
		l.skipEmptyFileForward()
	}
}

func (l *levelIter) SeekLT(key []byte) {
	// NB: the top-level dbIter has already adjusted key based on
	// IterOptions.UpperBound.
	if l.loadFile(l.findFileLT(key), -1) {
		l.iter.SeekLT(key)
		l.skipEmptyFileBackward()
	}
}

func (l *levelIter) First() {
	// NB: the top-level dbIter will call SeekGE if IterOptions.LowerBound is
	// set.
	if l.loadFile(0, 1) {
		l.iter.First()
		l.skipEmptyFileForward()
	}
}

func (l *levelIter) Last() {
	// NB: the top-level dbIter will call SeekLT if IterOptions.UpperBound is
	// set.
	if l.loadFile(len(l.files)-1, -1) {
		l.iter.Last()
		l.skipEmptyFileBackward()
	}
}

func (l *levelIter) Next() bool {
	if l.err != nil {
		return false
	}

	if l.iter == nil {
		if l.boundary != nil {
			if l.loadFile(l.index+1, 1) {
				l.iter.First()
				l.skipEmptyFileForward()
				return true
			}
			return false
		}
		if l.index == -1 && l.loadFile(0, 1) {
			// The iterator was positioned off the beginning of the level. Position
			// at the first entry.
			l.iter.First()
			l.skipEmptyFileForward()
			return true
		}
		return false
	}

	if l.iter.Next() {
		return true
	}
	return l.skipEmptyFileForward()
}

func (l *levelIter) Prev() bool {
	if l.err != nil {
		return false
	}

	if l.iter == nil {
		if l.boundary != nil {
			if l.loadFile(l.index-1, -1) {
				l.iter.Last()
				l.skipEmptyFileBackward()
				return true
			}
			return false
		}
		if n := len(l.files); l.index == n && l.loadFile(n-1, -1) {
			// The iterator was positioned off the end of the level. Position at the
			// last entry.
			l.iter.Last()
			l.skipEmptyFileBackward()
			return true
		}
		return false
	}

	if l.iter.Prev() {
		return true
	}
	return l.skipEmptyFileBackward()
}

func (l *levelIter) skipEmptyFileForward() bool {
	for !l.iter.Valid() {
		if l.err = l.iter.Close(); l.err != nil {
			return false
		}
		l.iter = nil

		if l.rangeDel != nil {
			// We're being used as part of a dbIter and we've reached the end of the
			// sstable. If the boundary is a range deletion tombstone, return that key.
			if f := &l.files[l.index]; f.largest.Kind() == db.InternalKeyKindRangeDelete {
				l.boundary = &f.largest
				return true
			}
		}

		// Current file was exhausted. Move to the next file.
		if !l.loadFile(l.index+1, 1) {
			return false
		}
		l.iter.First()
	}
	return true
}

func (l *levelIter) skipEmptyFileBackward() bool {
	for !l.iter.Valid() {
		if l.err = l.iter.Close(); l.err != nil {
			return false
		}
		l.iter = nil

		if l.rangeDel != nil {
			// We're being used as part of a dbIter and we've reached the end of the
			// sstable. If the boundary is a range deletion tombstone, return that key.
			if f := &l.files[l.index]; f.smallest.Kind() == db.InternalKeyKindRangeDelete {
				l.boundary = &f.smallest
				return true
			}
		}

		// Current file was exhausted. Move to the previous file.
		if !l.loadFile(l.index-1, -1) {
			return false
		}
		l.iter.Last()
	}
	return true
}

func (l *levelIter) Key() db.InternalKey {
	if l.iter == nil {
		if l.boundary != nil {
			return *l.boundary
		}
		return db.InvalidInternalKey
	}
	return l.iter.Key()
}

func (l *levelIter) Value() []byte {
	if l.iter == nil {
		return nil
	}
	return l.iter.Value()
}

func (l *levelIter) Valid() bool {
	if l.iter == nil {
		return l.boundary != nil
	}
	return l.iter.Valid()
}

func (l *levelIter) Error() error {
	if l.err != nil || l.iter == nil {
		return l.err
	}
	return l.iter.Error()
}

func (l *levelIter) Close() error {
	if l.iter != nil {
		l.err = l.iter.Close()
		l.iter = nil
	}
	return l.err
}
