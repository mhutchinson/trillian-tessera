// Copyright 2024 The Tessera authors. All Rights Reserved.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//	http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.
package posix

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync"
	"syscall"

	tessera "github.com/transparency-dev/trillian-tessera"
	"github.com/transparency-dev/trillian-tessera/api"
	"github.com/transparency-dev/trillian-tessera/api/layout"
	"github.com/transparency-dev/trillian-tessera/storage"
	"k8s.io/klog/v2"
)

const (
	dirPerm  = 0o755
	filePerm = 0o644
)

// Storage implements storage functions for a POSIX filesystem.
// It leverages the POSIX atomic operations.
type Storage struct {
	sync.Mutex
	path  string
	queue *storage.Queue

	cpFile *os.File

	curTree CurrentTreeFunc

	curSize uint64
	newCP   tessera.NewCPFunc
}

// NewTreeFunc is the signature of a function which receives information about newly integrated trees.
type NewTreeFunc func(size uint64, root []byte) error

// CurrentTree is the signature of a function which retrieves the current integrated tree size and root hash.
type CurrentTreeFunc func() (uint64, []byte, error)

// New creates a new POSIX storage.
func New(ctx context.Context, path string, curTree func() (uint64, []byte, error), opts ...func(*tessera.StorageOptions)) *Storage {
	curSize, _, err := curTree()
	if err != nil {
		panic(err)
	}
	opt := tessera.ResolveStorageOptions(nil, opts...)

	r := &Storage{
		path:    path,
		curSize: curSize,
		curTree: curTree,
		newCP:   opt.NewCP,
	}
	r.queue = storage.NewQueue(ctx, opt.BatchMaxAge, opt.BatchMaxSize, r.sequenceBatch)

	return r
}

// lockCP places a POSIX advisory lock for the checkpoint.
// Note that a) this is advisory, and b) we use an adjacent file to the checkpoint
// (`checkpoint.lock`) to avoid inherent brittleness of the `fcntrl` API (*any* `Close`
// operation on this file (even if it's a different FD) from this PID, or overwriting
// of the file by *any* process breaks the lock.)
func (s *Storage) lockCP() error {
	var err error
	if s.cpFile != nil {
		panic("not unlocked")
	}
	s.cpFile, err = os.OpenFile(filepath.Join(s.path, layout.CheckpointPath+".lock"), syscall.O_CREAT|syscall.O_RDWR|syscall.O_CLOEXEC, filePerm)
	if err != nil {
		return err
	}

	flockT := syscall.Flock_t{
		Type:   syscall.F_WRLCK,
		Whence: io.SeekStart,
		Start:  0,
		Len:    0,
	}
	for {
		if err := syscall.FcntlFlock(s.cpFile.Fd(), syscall.F_SETLKW, &flockT); err != syscall.EINTR {
			return err
		}
	}
}

// unlockCP unlocks the `checkpoint.lock` file.
func (s *Storage) unlockCP() error {
	if s.cpFile == nil {
		panic(errors.New("not locked"))
	}
	if err := s.cpFile.Close(); err != nil {
		return err
	}
	s.cpFile = nil
	return nil
}

// Add commits to sequence numbers for an entry
// Returns the sequence number assigned to the first entry in the batch, or an error.
func (s *Storage) Add(ctx context.Context, e *tessera.Entry) (uint64, error) {
	return s.queue.Add(ctx, e)()
}

// GetEntryBundle retrieves the Nth entries bundle for a log of the given size.
func (s *Storage) GetEntryBundle(ctx context.Context, index, logSize uint64) ([]byte, error) {
	return os.ReadFile(filepath.Join(s.path, layout.EntriesPath(index, logSize)))
}

// sequenceBatch writes the entries from the provided batch into the entry bundle files of the log.
//
// This func starts filling entries bundles at the next available slot in the log, ensuring that the
// sequenced entries are contiguous from the zeroth entry (i.e left-hand dense).
// We try to minimise the number of partially complete entry bundles by writing entries in chunks rather
// than one-by-one.
func (s *Storage) sequenceBatch(ctx context.Context, entries []*tessera.Entry) error {
	// Double locking:
	// - The mutex `Lock()` ensures that multiple concurrent calls to this function within a task are serialised.
	// - The POSIX `LockCP()` ensures that distinct tasks are serialised.
	s.Lock()
	if err := s.lockCP(); err != nil {
		panic(err)
	}
	defer func() {
		if err := s.unlockCP(); err != nil {
			panic(err)
		}
		s.Unlock()
	}()

	size, _, err := s.curTree()
	if err != nil {
		return err
	}
	s.curSize = size
	klog.V(1).Infof("Sequencing from %d", s.curSize)

	if len(entries) == 0 {
		return nil
	}
	currTile := &bytes.Buffer{}
	newSize := s.curSize + uint64(len(entries))
	seq := s.curSize
	bundleIndex, entriesInBundle := seq/uint64(256), seq%uint64(256)
	if entriesInBundle > 0 {
		// If the latest bundle is partial, we need to read the data it contains in for our newer, larger, bundle.
		part, err := s.GetEntryBundle(ctx, bundleIndex, s.curSize)
		if err != nil {
			return err
		}
		if _, err := currTile.Write(part); err != nil {
			return fmt.Errorf("failed to write partial bundle into buffer: %v", err)
		}
	}
	writeBundle := func(bundleIndex uint64) error {
		bf := filepath.Join(s.path, layout.EntriesPath(bundleIndex, newSize))
		if err := os.MkdirAll(filepath.Dir(bf), dirPerm); err != nil {
			return fmt.Errorf("failed to make entries directory structure: %w", err)
		}
		if err := createExclusive(bf, currTile.Bytes()); err != nil {
			if !errors.Is(err, os.ErrExist) {
				return err
			}
		}
		return nil
	}

	seqEntries := make([]storage.SequencedEntry, 0, len(entries))
	// Add new entries to the bundle
	for i, e := range entries {
		bundleData := e.MarshalBundleData(seq + uint64(i))
		if _, err := currTile.Write(bundleData); err != nil {
			return fmt.Errorf("failed to write entry %d to currTile: %v", i, err)
		}
		seqEntries = append(seqEntries, storage.SequencedEntry{
			BundleData: bundleData,
			LeafHash:   e.LeafHash(),
		})

		entriesInBundle++
		if entriesInBundle == uint64(256) {
			//  This bundle is full, so we need to write it out...
			// ... and prepare the next entry bundle for any remaining entries in the batch
			if err := writeBundle(bundleIndex); err != nil {
				return err
			}
			bundleIndex++
			entriesInBundle = 0
			currTile = &bytes.Buffer{}
		}
	}
	// If we have a partial bundle remaining once we've added all the entries from the batch,
	// this needs writing out too.
	if entriesInBundle > 0 {
		if err := writeBundle(bundleIndex); err != nil {
			return err
		}
	}

	// For simplicitly, well in-line the integration of these new entries into the Merkle structure too.
	if err := s.doIntegrate(ctx, seq, seqEntries); err != nil {
		klog.Errorf("Integrate failed: %v", err)
		return err
	}
	return nil

}

// doIntegrate handles integrating new entries into the log, and updating the checkpoint.
func (s *Storage) doIntegrate(ctx context.Context, fromSeq uint64, entries []storage.SequencedEntry) error {
	tb := storage.NewTreeBuilder(func(ctx context.Context, tileIDs []storage.TileID, treeSize uint64) ([]*api.HashTile, error) {
		n, err := s.getTiles(ctx, tileIDs, treeSize)
		if err != nil {
			return nil, fmt.Errorf("getTiles: %w", err)
		}
		return n, nil
	})

	newSize, newRoot, tiles, err := tb.Integrate(ctx, fromSeq, entries)
	if err != nil {
		klog.Errorf("Integrate: %v", err)
		return fmt.Errorf("Integrate: %v", err)
	}
	for k, v := range tiles {
		if err := s.StoreTile(ctx, uint64(k.Level), k.Index, newSize, v); err != nil {
			return fmt.Errorf("failed to set tile(%v): %v", k, err)
		}
	}

	klog.Infof("New CP: %d, %x", newSize, newRoot)
	if s.newCP != nil {
		cpRaw, err := s.newCP(newSize, newRoot)
		if err != nil {
			return fmt.Errorf("newCP: %v", err)
		}
		if err := WriteCheckpoint(s.path, cpRaw); err != nil {
			return fmt.Errorf("failed to write new checkpoint: %v", err)
		}
	}

	return nil
}

func (s *Storage) getTiles(ctx context.Context, tileIDs []storage.TileID, treeSize uint64) ([]*api.HashTile, error) {
	r := make([]*api.HashTile, 0, len(tileIDs))
	for _, id := range tileIDs {
		t, err := s.GetTile(ctx, id.Level, id.Index, treeSize)
		if err != nil {
			return nil, err
		}
		r = append(r, t)
	}
	return r, nil
}

// GetTile returns the tile at the given tile-level and tile-index.
// If no complete tile exists at that location, it will attempt to find a
// partial tile for the given tree size at that location.
func (s *Storage) GetTile(_ context.Context, level, index, logSize uint64) (*api.HashTile, error) {
	p := filepath.Join(s.path, layout.TilePath(level, index, logSize))
	t, err := os.ReadFile(p)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			// We'll signal to higher levels that it wasn't found by retuning a nil for this tile.
			return nil, nil
		}
		return nil, err
	}

	var tile api.HashTile
	if err := tile.UnmarshalText(t); err != nil {
		return nil, fmt.Errorf("failed to parse tile: %w", err)
	}
	return &tile, nil
}

// StoreTile writes a tile out to disk.
// Fully populated tiles are stored at the path corresponding to the level &
// index parameters, partially populated (i.e. right-hand edge) tiles are
// stored with a .xx suffix where xx is the number of "tile leaves" in hex.
func (s *Storage) StoreTile(_ context.Context, level, index, logSize uint64, tile *api.HashTile) error {
	tileSize := uint64(len(tile.Nodes))
	klog.V(2).Infof("StoreTile: level %d index %x ts: %x", level, index, tileSize)
	if tileSize == 0 || tileSize > 256 {
		return fmt.Errorf("tileSize %d must be > 0 and <= 256", tileSize)
	}
	t, err := tile.MarshalText()
	if err != nil {
		return fmt.Errorf("failed to marshal tile: %w", err)
	}

	tPath := filepath.Join(s.path, layout.TilePath(level, index, logSize))
	tDir := filepath.Dir(tPath)
	if err := os.MkdirAll(tDir, dirPerm); err != nil {
		return fmt.Errorf("failed to create directory %q: %w", tDir, err)
	}

	// TODO(al): use unlinked temp file
	temp := fmt.Sprintf("%s.temp", tPath)
	if err := os.WriteFile(temp, t, filePerm); err != nil {
		return fmt.Errorf("failed to write temporary tile file: %w", err)
	}
	if err := os.Rename(temp, tPath); err != nil {
		if !errors.Is(err, os.ErrExist) {
			return fmt.Errorf("failed to rename temporary tile file: %w", err)
		}
		os.Remove(temp)
	}

	if tileSize == 256 {
		partials, err := filepath.Glob(fmt.Sprintf("%s.*", tPath))
		if err != nil {
			return fmt.Errorf("failed to list partial tiles for clean up; %w", err)
		}
		// Clean up old partial tiles by symlinking them to the new full tile.
		for _, p := range partials {
			klog.V(2).Infof("relink partial %s to %s", p, tPath)
			// We have to do a little dance here to get POSIX atomicity:
			// 1. Create a new temporary symlink to the full tile
			// 2. Rename the temporary symlink over the top of the old partial tile
			tmp := fmt.Sprintf("%s.link", tPath)
			_ = os.Remove(tmp)
			if err := os.Symlink(tPath, tmp); err != nil {
				return fmt.Errorf("failed to create temp link to full tile: %w", err)
			}
			if err := os.Rename(tmp, p); err != nil {
				return fmt.Errorf("failed to rename temp link over partial tile: %w", err)
			}
		}
	}

	return nil
}

// WriteCheckpoint stores a raw log checkpoint on disk.
func WriteCheckpoint(path string, newCPRaw []byte) error {
	if err := createExclusive(filepath.Join(path, layout.CheckpointPath), newCPRaw); err != nil {
		return fmt.Errorf("failed to create checkpoint file: %w", err)
	}
	return nil
}

// Readcheckpoint returns the latest stored checkpoint.
func ReadCheckpoint(path string) ([]byte, error) {
	return os.ReadFile(filepath.Join(path, layout.CheckpointPath))
}

// createExclusive creates a file at the given path and name before writing the data in d to it.
// It will error if the file already exists, or it's unable to fully write the
// data & close the file.
func createExclusive(f string, d []byte) error {
	tmpFile, err := os.CreateTemp(filepath.Dir(f), "")
	if err != nil {
		return fmt.Errorf("unable to create temporary file: %w", err)
	}
	tmpName := tmpFile.Name()
	n, err := tmpFile.Write(d)
	if err != nil {
		return fmt.Errorf("unable to write leafdata to temporary file: %w", err)
	}
	if got, want := n, len(d); got != want {
		return fmt.Errorf("short write on leaf, wrote %d expected %d", got, want)
	}
	if err := tmpFile.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmpName, f); err != nil {
		return err
	}
	return nil
}
