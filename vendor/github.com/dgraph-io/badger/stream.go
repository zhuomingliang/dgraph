/*
 * Copyright 2018 Dgraph Labs, Inc. and Contributors
 *
 * Licensed under the Apache License, Version 2.0 (the "License");
 * you may not use this file except in compliance with the License.
 * You may obtain a copy of the License at
 *
 *     http://www.apache.org/licenses/LICENSE-2.0
 *
 * Unless required by applicable law or agreed to in writing, software
 * distributed under the License is distributed on an "AS IS" BASIS,
 * WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
 * See the License for the specific language governing permissions and
 * limitations under the License.
 */

package badger

import (
	"bytes"
	"context"
	"sync"
	"time"

	"github.com/dgraph-io/badger/pb"
	"github.com/dgraph-io/badger/y"
	humanize "github.com/dustin/go-humanize"
)

const pageSize = 4 << 20 // 4MB

// Stream provides a framework to concurrently iterate over a snapshot of Badger, pick up
// key-values, batch them up and call Send. Because, Stream does concurrent iteration over many
// smaller key ranges, it does NOT send keys in a strictly lexicographical order.
type Stream struct {
	// Prefix to only iterate over certain range of keys. If set to nil (default), Stream would
	// iterate over the entire DB.
	Prefix []byte

	readTs uint64
	db     *DB

	// ChooseKey is invoked each time a new key is encountered. Note that this is not called
	// on every version of the value, only the first encountered version (i.e. the highest version
	// of the value a key has). ChooseKey can be left nil to select all keys.
	//
	// Note: Calls to ChooseKey are concurrent.
	ChooseKey func(item *Item) bool

	// KeyToList, similar to ChooseKey, is only invoked on the highest version of the value. It
	// is upto the caller to iterate over the versions and generate zero, one or more KVs. It
	// is expected that the user would advance the iterator to go through the versions of the
	// values. However, the user MUST immediately return from this function on the first encounter
	// with a mismatching key. See example usage in ToList function. Can be left nil to use ToList
	// function by default.
	//
	// Note: Calls to KeyToList are concurrent.
	KeyToList func(key []byte, itr *Iterator) (*pb.KVList, error)

	// This is the method where Stream sends the final output. All calls to Send are done by a
	// single goroutine, i.e. logic within Send method can expect single threaded execution.
	Send func(*pb.KVList) error

	rangeCh chan keyRange
	kvChan  chan *pb.KVList
}

// ToList is a default implementation of KeyToList. It picks up all valid versions of the key,
// skipping over deleted or expired keys.
func (st *Stream) ToList(key []byte, itr *Iterator) (*pb.KVList, error) {
	list := &pb.KVList{}
	for ; itr.Valid(); itr.Next() {
		item := itr.Item()
		if item.IsDeletedOrExpired() {
			break
		}
		if !bytes.Equal(key, item.Key()) {
			// Break out on the first encounter with another key.
			break
		}

		valCopy, err := item.ValueCopy(nil)
		if err != nil {
			return nil, err
		}
		kv := &pb.KV{
			Key:       item.KeyCopy(nil),
			Value:     valCopy,
			UserMeta:  []byte{item.UserMeta()},
			Version:   item.Version(),
			ExpiresAt: item.ExpiresAt(),
		}
		list.Kv = append(list.Kv, kv)
		if st.db.opt.NumVersionsToKeep == 1 {
			break
		}

		if item.DiscardEarlierVersions() {
			break
		}
	}
	return list, nil
}

// keyRange is [start, end), including start, excluding end. Do ensure that the start,
// end byte slices are owned by keyRange struct.
func (st *Stream) produceRanges(ctx context.Context) {
	splits := st.db.KeySplits(st.Prefix)
	start := y.SafeCopy(nil, st.Prefix)
	for _, key := range splits {
		st.rangeCh <- keyRange{left: start, right: y.SafeCopy(nil, []byte(key))}
		start = y.SafeCopy(nil, []byte(key))
	}
	// Edge case: prefix is empty and no splits exist. In that case, we should have at least one
	// keyRange output.
	st.rangeCh <- keyRange{left: start}
	close(st.rangeCh)
}

// produceKVs picks up ranges from rangeCh, generates KV lists and sends them to kvChan.
func (st *Stream) produceKVs(ctx context.Context) error {
	var size int
	var txn *Txn
	if st.readTs > 0 {
		txn = st.db.NewTransactionAt(st.readTs, false)
	} else {
		txn = st.db.NewTransaction(false)
	}
	defer txn.Discard()

	iterate := func(kr keyRange) error {
		iterOpts := DefaultIteratorOptions
		iterOpts.AllVersions = true
		iterOpts.Prefix = st.Prefix
		iterOpts.PrefetchValues = false
		itr := txn.NewIterator(iterOpts)
		defer itr.Close()

		outList := new(pb.KVList)
		var prevKey []byte
		for itr.Seek(kr.left); itr.Valid(); {
			// it.Valid would only return true for keys with the provided Prefix in iterOpts.
			item := itr.Item()
			if bytes.Equal(item.Key(), prevKey) {
				itr.Next()
				continue
			}
			prevKey = append(prevKey[:0], item.Key()...)

			// Check if we reached the end of the key range.
			if len(kr.right) > 0 && bytes.Compare(item.Key(), kr.right) >= 0 {
				break
			}
			// Check if we should pick this key.
			if st.ChooseKey != nil && !st.ChooseKey(item) {
				continue
			}

			// Now convert to key value.
			list, err := st.KeyToList(item.KeyCopy(nil), itr)
			if err != nil {
				return err
			}
			if list == nil || len(list.Kv) == 0 {
				continue
			}
			outList.Kv = append(outList.Kv, list.Kv...)
			size += list.Size()
			if size >= pageSize {
				st.kvChan <- outList
				outList = new(pb.KVList)
				size = 0
			}
		}
		if len(outList.Kv) > 0 {
			st.kvChan <- outList
		}
		return nil
	}

	for {
		select {
		case kr, ok := <-st.rangeCh:
			if !ok {
				// Done with the keys.
				return nil
			}
			if err := iterate(kr); err != nil {
				return err
			}
		case <-ctx.Done():
			return ctx.Err()
		}
	}
}

func (st *Stream) streamKVs(ctx context.Context, logPrefix string) error {
	var count int
	var bytesSent uint64
	t := time.NewTicker(time.Second)
	defer t.Stop()
	now := time.Now()

	slurp := func(batch *pb.KVList) error {
	loop:
		for {
			select {
			case kvs, ok := <-st.kvChan:
				if !ok {
					break loop
				}
				y.AssertTrue(kvs != nil)
				batch.Kv = append(batch.Kv, kvs.Kv...)
			default:
				break loop
			}
		}
		sz := uint64(batch.Size())
		bytesSent += sz
		count += len(batch.Kv)
		t := time.Now()
		if err := st.Send(batch); err != nil {
			return err
		}
		Infof("%s Created batch of size: %s in %s.\n",
			logPrefix, humanize.Bytes(sz), time.Since(t))
		return nil
	}

outer:
	for {
		var batch *pb.KVList
		select {
		case <-ctx.Done():
			return ctx.Err()

		case <-t.C:
			dur := time.Since(now)
			durSec := uint64(dur.Seconds())
			if durSec == 0 {
				continue
			}
			speed := bytesSent / durSec
			Infof("%s Time elapsed: %s, bytes sent: %s, speed: %s/sec\n",
				logPrefix, y.FixedDuration(dur), humanize.Bytes(bytesSent), humanize.Bytes(speed))

		case kvs, ok := <-st.kvChan:
			if !ok {
				break outer
			}
			y.AssertTrue(kvs != nil)
			batch = kvs
			if err := slurp(batch); err != nil {
				return err
			}
		}
	}

	Infof("%s Sent %d keys\n", logPrefix, count)
	return nil
}

// Orchestrate runs Stream. It picks up ranges from the SSTables, then runs numGo number of
// goroutines to iterate over these ranges and batch up KVs in lists. It then runs a single
// goroutine to pick these lists, batch them up further and send to Output.Send. Orchestrate also
// spits logs out to Infof, using the logPrefix string provided.  Note that all calls to
// Output.Send are serial. In case any of these steps encounter an error, Orchestrate would stop
// execution and return that error. Orchestrate should only be called once on the same Stream
// object.
func (st *Stream) Orchestrate(ctx context.Context, numGo int, logPrefix string) error {
	st.rangeCh = make(chan keyRange, 3) // Contains keys for posting lists.

	// kvChan should only have a small capacity to ensure that we don't buffer up too much data if
	// sending is slow. So, setting this to 3.
	st.kvChan = make(chan *pb.KVList, 3)

	if st.KeyToList == nil {
		st.KeyToList = st.ToList
	}

	// Picks up ranges from Badger, and sends them to rangeCh.
	go st.produceRanges(ctx)

	errCh := make(chan error, 1) // Stores error by consumeKeys.
	var wg sync.WaitGroup
	for i := 0; i < numGo; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			// Picks up ranges from rangeCh, generates KV lists, and sends them to kvChan.
			if err := st.produceKVs(ctx); err != nil {
				select {
				case errCh <- err:
				default:
				}
			}
		}()
	}

	// Pick up key-values from kvChan and send to stream.
	kvErr := make(chan error, 1)
	go func() {
		// Picks up KV lists from kvChan, and sends them to Output.
		kvErr <- st.streamKVs(ctx, logPrefix)
	}()
	wg.Wait()        // Wait for produceKVs to be over.
	close(st.kvChan) // Now we can close kvChan.

	select {
	case err := <-errCh: // Check error from produceKVs.
		return err
	default:
	}

	// Wait for key streaming to be over.
	if err := <-kvErr; err != nil {
		return err
	}
	return nil
}

// NewStream creates a new Stream.
func (db *DB) NewStream() *Stream {
	if db.opt.managedTxns {
		panic("This API can not be called in managed mode.")
	}
	return &Stream{db: db}
}

// NewStreamAt creates a new Stream at a particular timestamp. Should only be used with managed DB.
func (db *DB) NewStreamAt(readTs uint64) *Stream {
	if !db.opt.managedTxns {
		panic("This API can only be called in managed mode.")
	}
	return &Stream{db: db, readTs: readTs}
}
