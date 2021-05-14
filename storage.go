package storage

import (
	"fmt"
	"io/fs"
	"io/ioutil"
	"log"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/nakabonne/tsdbe/partition"
	"github.com/nakabonne/tsdbe/partition/disk"
	"github.com/nakabonne/tsdbe/partition/memory"
	"github.com/nakabonne/tsdbe/wal"
	"github.com/nakabonne/tsdbe/cgroup"
	"github.com/nakabonne/tsdbe/timerpool"
)

var (
	// Limit the concurrency for data ingestion to GOMAXPROCS, since this operation
	// is CPU bound, so there is no sense in running more than GOMAXPROCS concurrent
	// goroutines on data ingestion path.
	defaultWorkersLimit = cgroup.AvailableCPUs()
	writeTimeout        = 30 * time.Second

	partitionDirRegex = regexp.MustCompile(`^p-.+`)
)

// Storage provides goroutine safe capabilities of insertion into and retrieval from partitions.
type Storage interface {
	Reader
	Writer
	// FlushRows persists all in-memory partitions ready to persisted.
	FlushRows() error
}

// Reader provides reading access to time series data.
type Reader interface {
	SelectRows(metricName string, start, end int64) []partition.DataPoint
}

// Writer provides writing access to time series data.
type Writer interface {
	InsertRows(rows []partition.Row) error
	// Wait waits until all tasks got done.
	Wait()
}

// NewStorage gives back a new storage along with the initial partition.
func NewStorage(wal wal.WAL, partitionDuration time.Duration, dataPath string) (Storage, error) {
	if partitionDuration <= 0 {
		return nil, fmt.Errorf("invalid partitionDuration given: %v", partitionDuration)
	}
	s := &storage{
		partitionList:  partition.NewPartitionList(),
		workersLimitCh: make(chan struct{}, defaultWorkersLimit),
		wal:            wal,
		partitionTTL:   partitionDuration,
		dataPath:       dataPath,
	}

	if s.inMemoryMode() {
		s.partitionList.Insert(memory.NewMemoryPartition(wal, partitionDuration))
		return s, nil
	}

	if err := os.MkdirAll(dataPath, fs.ModePerm); err != nil {
		return nil, fmt.Errorf("failed to make data directory %s: %w", dataPath, err)
	}
	files, err := ioutil.ReadDir(dataPath)
	if err != nil {
		return nil, fmt.Errorf("failed to open data directory: %w", err)
	}
	if len(files) == 0 {
		s.partitionList.Insert(memory.NewMemoryPartition(wal, partitionDuration))
		return s, nil
	}

	// Read existent partitions from the disk.
	isPartitionDir := func(f fs.FileInfo) bool {
		return f.IsDir() && partitionDirRegex.MatchString(f.Name())
	}
	partitions := make([]partition.Partition, 0, len(files))
	for _, f := range files {
		if !isPartitionDir(f) {
			continue
		}
		path := filepath.Join(dataPath, f.Name())
		part, err := disk.OpenDiskPartition(path)
		if err != nil {
			return nil, fmt.Errorf("failed to open disk partition for %s: %w", path, err)
		}
		partitions = append(partitions, part)
	}
	sort.Slice(partitions, func(i, j int) bool {
		return partitions[i].MinTimestamp() < partitions[j].MinTimestamp()
	})
	for _, p := range partitions {
		s.partitionList.Insert(p)
	}
	s.partitionList.Insert(memory.NewMemoryPartition(wal, partitionDuration))

	return s, nil
}

type storage struct {
	partitionList partition.PartitionList

	wal          wal.WAL
	partitionTTL time.Duration
	dataPath     string

	workersLimitCh chan struct{}
	// wg must be incremented to guarantee all writes are done gracefully.
	wg sync.WaitGroup
}

func (s *storage) InsertRows(rows []partition.Row) error {
	s.wg.Add(1)
	defer s.wg.Done()

	// Limit the number of concurrent goroutines to prevent from out of memory
	// errors and CPU trashing even if too many goroutines attempt to write.
	select {
	case s.workersLimitCh <- struct{}{}:
	default:
		t := timerpool.Get(writeTimeout)
		select {
		case s.workersLimitCh <- struct{}{}:
			timerpool.Put(t)
		case <-t.C:
			return fmt.Errorf("failed to write a data point in %s, since it is overloaded with %d concurrent writers",
				writeTimeout, defaultWorkersLimit)
		}

	}
	p := s.getPartition()
	if err := p.InsertRows(rows); err != nil {
		return fmt.Errorf("failed to insert rows: %w", err)
	}
	<-s.workersLimitCh
	return nil
}

// getPartition returns a writable partition. If none, it creates a new one.
func (s *storage) getPartition() partition.Partition {
	head := s.partitionList.GetHead()
	if !head.ReadOnly() {
		return head
	}

	// All partitions seems to be unavailable so add a new partition to the list.

	p := memory.NewMemoryPartition(s.wal, s.partitionTTL)
	s.partitionList.Insert(p)
	return p
}

func (s *storage) SelectRows(metricName string, start, end int64) []partition.DataPoint {
	res := make([]partition.DataPoint, 0)

	// Iterate over all partitions from the newest one.
	iterator := s.partitionList.NewIterator()
	for iterator.Next() {
		part, err := iterator.Value()
		if err != nil {
			// FIXME: Replace logger
			log.Printf("invalid partition found: %v\n", err)
			continue
		}
		if part.MaxTimestamp() < start {
			// No need to keep going anymore
			break
		}
		if part.MinTimestamp() > end {
			continue
		}
		points := part.SelectRows(metricName, start, end)
		// in order to keep the order in ascending.
		res = append(points, res...)
	}
	return res
}

func (s *storage) FlushRows() error {
	iterator := s.partitionList.NewIterator()
	for iterator.Next() {
		part, err := iterator.Value()
		if err != nil {
			return fmt.Errorf("invalid partition found: %w", err)
		}
		if p, ok := part.(partition.MemoryPartition); !ok || !p.ReadyToBePersisted() {
			continue
		}

		if s.inMemoryMode() {
			if err := s.partitionList.Remove(part); err != nil {
				return fmt.Errorf("failed to remove partition: %w", err)
			}
			continue
		}

		// Start swapping in-memory partition for disk one.
		// The disk partition will place at where in-memory one existed.

		rows := make([]partition.Row, 0, part.Size())
		rows = append(rows, part.SelectAll()...)
		// TODO: Use https://github.com/oklog/ulid instead of uuid
		dir := filepath.Join(s.dataPath, fmt.Sprintf("p-%s", uuid.New()))
		newPart, err := disk.NewDiskPartition(dir, rows, part.MinTimestamp(), part.MaxTimestamp())
		if err != nil {
			return fmt.Errorf("failed to generate disk partition for %s: %w", dir, err)
		}
		if err := s.partitionList.Swap(part, newPart); err != nil {
			return fmt.Errorf("failed to swap partitions: %w", err)
		}
	}
	return nil
}

func (s *storage) Wait() {
	s.wg.Wait()
	// TODO: Prevent from new goroutines calling Write(), for graceful shutdown.
	// TODO: Flush data points within the all memory partition into the backend.
}

func (s *storage) inMemoryMode() bool {
	return s.dataPath == ""
}