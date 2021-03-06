package hub

import (
	"bytes"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"math/rand"
	"net/url"
	"strconv"
	"sync"
	"time"

	bolt "go.etcd.io/bbolt"
	"go.uber.org/atomic"

	log "github.com/sirupsen/logrus"
)

const defaultBoltBucketName = "updates"

// BoltTransport implements the TransportInterface using the Bolt database.
type BoltTransport struct {
	sync.Mutex
	db                *bolt.DB
	bucketName        string
	size              uint64
	cleanupFrequency  float64
	pipes             map[*Pipe]struct{}
	done              chan struct{}
	lastSeq           atomic.Uint64
	bufferSize        int
	bufferFullTimeout time.Duration
}

// NewBoltTransport create a new BoltTransport.
func NewBoltTransport(u *url.URL, bufferSize int, bufferFullTimeout time.Duration) (*BoltTransport, error) {
	var err error
	q := u.Query()
	bucketName := defaultBoltBucketName
	if q.Get("bucket_name") != "" {
		bucketName = q.Get("bucket_name")
	}

	size := uint64(0)
	sizeParameter := q.Get("size")
	if sizeParameter != "" {
		size, err = strconv.ParseUint(sizeParameter, 10, 64)
		if err != nil {
			return nil, fmt.Errorf(`%q: invalid "size" parameter %q: %s: %w`, u, sizeParameter, err, ErrInvalidTransportDSN)
		}
	}

	cleanupFrequency := 0.3
	cleanupFrequencyParameter := q.Get("cleanup_frequency")
	if cleanupFrequencyParameter != "" {
		cleanupFrequency, err = strconv.ParseFloat(cleanupFrequencyParameter, 64)
		if err != nil {
			return nil, fmt.Errorf(`%q: invalid "cleanup_frequency" parameter %q: %w`, u, cleanupFrequencyParameter, ErrInvalidTransportDSN)
		}
	}

	path := u.Path // absolute path (bolt:///path.db)
	if path == "" {
		path = u.Host // relative path (bolt://path.db)
	}
	if path == "" {
		return nil, fmt.Errorf(`%q: missing path: %w`, u, ErrInvalidTransportDSN)
	}

	db, err := bolt.Open(path, 0600, nil)
	if err != nil {
		return nil, fmt.Errorf(`%q: %s: %w`, u, err, ErrInvalidTransportDSN)
	}

	return &BoltTransport{
		db:               db,
		bucketName:       bucketName,
		size:             size,
		cleanupFrequency: cleanupFrequency,
		pipes:            make(map[*Pipe]struct{}), done: make(chan struct{}),
		bufferSize:        bufferSize,
		bufferFullTimeout: bufferFullTimeout,
	}, nil
}

// Write pushes updates in the Transport.
func (t *BoltTransport) Write(update *Update) error {
	select {
	case <-t.done:
		return ErrClosedTransport
	default:
	}

	updateJSON, err := json.Marshal(*update)
	if err != nil {
		return err
	}

	// We cannot use RLock() because Bolt allows only one read-write transaction at a time
	t.Lock()
	defer t.Unlock()

	if err := t.persist(update.ID, updateJSON); err != nil {
		return err
	}

	for pipe := range t.pipes {
		if !pipe.Write(update) {
			delete(t.pipes, pipe)
		}
	}

	return nil
}

// persist stores update in the database.
func (t *BoltTransport) persist(updateID string, updateJSON []byte) error {
	return t.db.Update(func(tx *bolt.Tx) error {
		bucket, err := tx.CreateBucketIfNotExists([]byte(t.bucketName))
		if err != nil {
			return err
		}

		seq, err := bucket.NextSequence()
		if err != nil {
			return err
		}
		t.lastSeq.Store(seq)
		prefix := make([]byte, 8)
		binary.BigEndian.PutUint64(prefix, seq)

		// The sequence value is prepended to the update id to create an ordered list
		key := bytes.Join([][]byte{prefix, []byte(updateID)}, []byte{})

		if err := t.cleanup(bucket, seq); err != nil {
			return err
		}

		// The DB is append only
		bucket.FillPercent = 1
		return bucket.Put(key, updateJSON)
	})
}

// CreatePipe returns a pipe fetching updates from the given point in time.
func (t *BoltTransport) CreatePipe(fromID string) (*Pipe, error) {
	t.Lock()
	defer t.Unlock()

	select {
	case <-t.done:
		return nil, ErrClosedTransport
	default:
	}

	pipe := NewPipe(t.bufferSize, t.bufferFullTimeout)
	t.pipes[pipe] = struct{}{}
	if fromID == "" {
		return pipe, nil
	}

	toSeq := t.lastSeq.Load()
	go t.fetch(fromID, toSeq, pipe)

	return pipe, nil
}

func (t *BoltTransport) fetch(fromID string, toSeq uint64, pipe *Pipe) {
	err := t.db.View(func(tx *bolt.Tx) error {
		b := tx.Bucket([]byte(t.bucketName))
		if b == nil {
			return nil // No data
		}

		c := b.Cursor()
		afterFromID := false
		for k, v := c.First(); k != nil; k, v = c.Next() {
			if !afterFromID {
				if string(k[8:]) == fromID {
					afterFromID = true
				}

				continue
			}

			var update *Update
			if err := json.Unmarshal(v, &update); err != nil {
				return err
			}

			if !pipe.Write(update) || (toSeq > 0 && binary.BigEndian.Uint64(k[:8]) >= toSeq) {
				return nil
			}
		}

		return nil
	})
	if err != nil {
		log.Error(fmt.Errorf("bolt history: %w", err))
	}
}

// Close closes the Transport.
func (t *BoltTransport) Close() error {
	select {
	case <-t.done:
		return nil
	default:
	}

	t.Lock()
	defer t.Unlock()
	for pipe := range t.pipes {
		close(pipe.Read())
	}
	close(t.done)
	t.db.Close()

	return nil
}

// cleanup removes entries in the history above the size limit, triggered probabilistically.
func (t *BoltTransport) cleanup(bucket *bolt.Bucket, lastID uint64) error {
	if t.size == 0 ||
		t.cleanupFrequency == 0 ||
		t.size >= lastID ||
		(t.cleanupFrequency != 1 && rand.Float64() < t.cleanupFrequency) {
		return nil
	}

	removeUntil := lastID - t.size
	c := bucket.Cursor()
	for k, _ := c.First(); k != nil; k, _ = c.Next() {
		if binary.BigEndian.Uint64(k[:8]) > removeUntil {
			break
		}

		if err := bucket.Delete(k); err != nil {
			return err
		}
	}

	return nil
}
