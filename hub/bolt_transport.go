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

	log "github.com/sirupsen/logrus"
	bolt "go.etcd.io/bbolt"
)

const defaultBoltBucketName = "updates"

// BoltTransport implements the TransportInterface using the Bolt database.
type BoltTransport struct {
	sync.RWMutex
	db               *bolt.DB
	bucketName       string
	size             uint64
	cleanupFrequency float64
	subscribers      map[*Subscriber]struct{}
	closed           chan struct{}
	closedOnce       sync.Once
	lastSeq          uint64
	lastEventID      string
}

// NewBoltTransport create a new BoltTransport.
func NewBoltTransport(u *url.URL) (*BoltTransport, error) {
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

	db, err := bolt.Open(path, 0o600, nil)
	if err != nil {
		return nil, fmt.Errorf(`%q: %s: %w`, u, err, ErrInvalidTransportDSN)
	}

	return &BoltTransport{
		db:               db,
		bucketName:       bucketName,
		size:             size,
		cleanupFrequency: cleanupFrequency,
		subscribers:      make(map[*Subscriber]struct{}),
		closed:           make(chan struct{}),
		lastEventID:      getDBLastEventID(db, bucketName),
	}, nil
}

func getDBLastEventID(db *bolt.DB, bucketName string) string {
	lastEventID := EarliestLastEventID
	db.View(func(tx *bolt.Tx) error {
		b := tx.Bucket([]byte(bucketName))
		if b == nil {
			return nil // No data
		}

		if k, _ := b.Cursor().Last(); k != nil {
			lastEventID = string(k[8:])
		}

		return nil
	})

	return lastEventID
}

// Dispatch dispatches an update to all subscribers and persists it in BoltDB.
func (t *BoltTransport) Dispatch(update *Update) error {
	select {
	case <-t.closed:
		return ErrClosedTransport
	default:
	}

	AssignUUID(update)
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

	for subscriber := range t.subscribers {
		if !subscriber.Dispatch(update, false) {
			delete(t.subscribers, subscriber)
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
		prefix := make([]byte, 8)
		binary.BigEndian.PutUint64(prefix, seq)

		// The sequence value is prepended to the update id to create an ordered list
		key := bytes.Join([][]byte{prefix, []byte(updateID)}, []byte{})

		// The DB is append only
		bucket.FillPercent = 1

		t.lastSeq = seq
		t.lastEventID = updateID
		if err := bucket.Put(key, updateJSON); err != nil {
			return err
		}

		return t.cleanup(bucket, seq)
	})
}

// AddSubscriber adds a new subscriber to the transport.
func (t *BoltTransport) AddSubscriber(s *Subscriber) error {
	select {
	case <-t.closed:
		return ErrClosedTransport
	default:
	}

	t.Lock()
	t.subscribers[s] = struct{}{}
	toSeq := t.lastSeq
	t.Unlock()

	if s.RequestLastEventID != "" {
		t.dispatchHistory(s, toSeq)
	}

	return nil
}

// GetSubscribers get the list of active subscribers.
func (t *BoltTransport) GetSubscribers() (lastEventID string, subscribers []*Subscriber) {
	t.RLock()
	defer t.RUnlock()
	subscribers = make([]*Subscriber, len(t.subscribers))

	i := 0
	for subscriber := range t.subscribers {
		subscribers[i] = subscriber
		i++
	}

	return t.lastEventID, subscribers
}

func (t *BoltTransport) dispatchHistory(s *Subscriber, toSeq uint64) {
	t.db.View(func(tx *bolt.Tx) error {
		b := tx.Bucket([]byte(t.bucketName))
		if b == nil {
			s.HistoryDispatched(EarliestLastEventID)

			return nil // No data
		}

		c := b.Cursor()
		responseLastEventID := EarliestLastEventID
		afterFromID := s.RequestLastEventID == EarliestLastEventID
		for k, v := c.First(); k != nil; k, v = c.Next() {
			if !afterFromID {
				responseLastEventID = string(k[8:])
				if responseLastEventID == s.RequestLastEventID {
					afterFromID = true
				}

				continue
			}

			var update *Update
			if err := json.Unmarshal(v, &update); err != nil {
				s.HistoryDispatched(responseLastEventID)
				log.Error(fmt.Errorf("bolt history: %w", err))

				return err
			}

			if !s.Dispatch(update, true) || (toSeq > 0 && binary.BigEndian.Uint64(k[:8]) >= toSeq) {
				s.HistoryDispatched(responseLastEventID)

				return nil
			}
		}
		s.HistoryDispatched(responseLastEventID)

		return nil
	})
}

// Close closes the Transport.
func (t *BoltTransport) Close() (err error) {
	t.closedOnce.Do(func() {
		close(t.closed)

		t.Lock()
		for subscriber := range t.subscribers {
			subscriber.Disconnect()
			delete(t.subscribers, subscriber)
		}
		t.Unlock()

		err = t.db.Close()
	})

	return err
}

// cleanup removes entries in the history above the size limit, triggered probabilistically.
func (t *BoltTransport) cleanup(bucket *bolt.Bucket, lastID uint64) error {
	if t.size == 0 ||
		t.cleanupFrequency == 0 ||
		t.size >= lastID ||
		(t.cleanupFrequency != 1 && rand.Float64() < t.cleanupFrequency) { // nolint:gosec
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
