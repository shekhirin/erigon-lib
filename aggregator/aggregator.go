/*
   Copyright 2021 Erigon contributors

   Licensed under the Apache License, Version 2.0 (the "License");
   you may not use this file except in compliance with the License.
   You may obtain a copy of the License at

       http://www.apache.org/licenses/LICENSE-2.0

   Unless required by applicable law or agreed to in writing, software
   distributed under the License is distributed on an "AS IS" BASIS,
   WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
   See the License for the specific language governing permissions and
   limitations under the License.
*/

package aggregator

import (
	"os"
	"path"
	"regexp"
	"strconv"

	"github.com/google/btree"
	"github.com/ledgerwatch/erigon-lib/compress"
	"github.com/ledgerwatch/erigon-lib/recsplit"
	"github.com/ledgerwatch/log/v3"
)

// Aggregator of multiple state files to support state reader and state writer
// The convension for the file names are as follows
// State is composed of three types of files:
// 1. Accounts. keys are addresses (20 bytes), values are encoding of accounts
// 2. Contract storage. Keys are concatenation of addresses (20 bytes) and storage locations (32 bytes), values have their leading zeroes removed
// 3. Contract codes. Keys are addresses (20 bytes), values are bycodes
// Within each type, any file can cover an interval of block numbers, for example, `accounts.1-16` represents changes in accounts
// that were effected by the blocks from 1 to 16, inclusively. The second component of the interval will be called "end block" for the file.
// Finally, for each type and interval, there are two files - one with the compressed data (extension `dat`),
// and another with the index (extension `idx`) consisting of the minimal perfect hash table mapping keys to the offsets of corresponding keys
// in the data file
// Aggregator consists (apart from the file it is aggregating) of the 4 parts:
// 1. Persistent table of expiration time for each of the files. Key - name of the file, value - timestamp, at which the file can be removed
// 2. Transient (in-memory) mapping the "end block" of each file to the objects required for accessing the file (compress.Decompressor and resplit.Index)
// 3. Persistent tables (one for accounts, one for contract storage, and one for contract code) summarising all the 1-block state diff files
//    that were not yet merged together to form larger files. In these table, keys are the same as keys in the state diff files, but values are also
//    augemented by the number of state diff files this key is present. This number gets decremented every time when a 1-block state diff files is removed
//    from the summary table (due to being merged). And when this number gets to 0, the record is deleted from the summary table.
// 4. Aggregating persistent hash table

type Aggregator struct {
	diffDir    string // Directory where the state diff files are stored
	dbDir      string // Directory where persistent parts of the aggregator are stored
	byEndBlock *btree.BTree
}

type byEndBlockItem struct {
	endBlock    uint64
	accountsD   *compress.Decompressor
	accountsIdx *recsplit.Index
	storageD    *compress.Decompressor
	storageIdx  *recsplit.Index
	codeD       *compress.Decompressor
	codeIdx     *recsplit.Index
}

func (i *byEndBlockItem) Less(than btree.Item) bool {
	return i.endBlock < than.(*byEndBlockItem).endBlock
}

func NewAggregator(diffDir string, dbDir string) (*Aggregator, error) {
	a := &Aggregator{
		diffDir: diffDir,
		dbDir:   dbDir,
	}
	byEndBlock := btree.New(32)
	// Scan the diff directory and create the mapping of end blocks to files
	files, err := os.ReadDir(diffDir)
	if err != nil {
		return nil, err
	}
	re := regexp.MustCompile(`(accounts|storage|code).([0-9]+)-([0-9]+).(dat|idx)`)
	for _, f := range files {
		name := f.Name()
		subs := re.FindStringSubmatch(name)
		if len(subs) != 5 {
			log.Warn("File ignored by aggregator, more than 4 submatches", "name", name)
			continue
		}
		var startBlock, endBlock uint64
		if startBlock, err = strconv.ParseUint(subs[2], 10, 64); err != nil {
			log.Warn("File ignored by aggregator, parsing startBlock", "error", err, "name", name)
			continue
		}
		if endBlock, err = strconv.ParseUint(subs[3], 10, 64); err != nil {
			log.Warn("File ignored by aggregator, parsing endBlock", "error", err, "name", name)
			continue
		}
		if startBlock > endBlock {
			log.Warn("File ignored by aggregator, startBlock > endBlock", "name", name)
			continue
		}
		var item *byEndBlockItem = &byEndBlockItem{endBlock: endBlock}
		i := byEndBlock.Get(item)
		if i != nil {
			byEndBlock.ReplaceOrInsert(item)
		} else {
			item = i.(*byEndBlockItem)
		}
		var d *compress.Decompressor
		var idx *recsplit.Index
		switch subs[4] {
		case "dat":
			if d, err = compress.NewDecompressor(path.Join(diffDir, name)); err != nil {
				return nil, err
			}
		case "idx":
			if idx, err = recsplit.NewIndex(path.Join(diffDir, name)); err != nil {
				return nil, err
			}
		}
		switch subs[1] {
		case "accounts":
			if d != nil {
				item.accountsD = d
			} else {
				item.accountsIdx = idx
			}
		case "storage":
			if d != nil {
				item.storageD = d
			} else {
				item.storageIdx = idx
			}
		case "code":
			if d != nil {
				item.codeD = d
			} else {
				item.codeIdx = idx
			}
		}
	}
	// TODO make sure all decompressors and indexes created are closed in case of an error
	a.byEndBlock = byEndBlock
	return a, nil
}
