// Package stream is a Kafka-shaped broker that runs inside this process.
//
// The platform's ingest path is written against Kafka semantics — topics with
// partitions, key-hash partitioning, an idempotent producer keyed by
// (producer id, sequence), consumer groups with deterministic range assignment,
// and offsets the group commits — so that putting franz-go in front of a real
// cluster is a wiring change rather than a rewrite. Nothing above this package
// knows whether the broker is a map in memory or a Confluent cluster.
//
// Segments are file-backed when a directory is configured, and each group's
// committed offsets are written beside them. A broker reopened on the same
// directory restores its records and its dedupe table, and a consumer rejoining
// the same group resumes from the committed offset rather than from zero. That
// is the property the demo exercises and the one an exactly-once ingest story
// rests on.
//
// It is a reference implementation, not a cluster: everything is JSON on disk
// behind one mutex, there is no replication, no retention and no fsync, and the
// idempotence table grows for the life of a broker rather than expiring with a
// transactional session. Those are the right trades for a single process and
// the wrong ones for a broker you would page someone about.
package stream

import (
	"errors"
	"hash/fnv"
	"time"
)

// Errors returned by this package.
var (
	// ErrUnknownTopic is returned when a topic has not been created.
	ErrUnknownTopic = errors.New("stream: unknown topic")
	// ErrTopicExists is returned when a topic is recreated with a different
	// partition count, which would silently repartition every key.
	ErrTopicExists = errors.New("stream: topic already exists with a different partition count")
	// ErrClosed is returned by every operation on a closed broker or consumer.
	ErrClosed = errors.New("stream: closed")
	// ErrBadPartitionCount is returned when a topic is created with no partitions.
	ErrBadPartitionCount = errors.New("stream: partition count must be positive")
	// ErrNoTopics is returned when a consumer joins a group without subscribing
	// to anything.
	ErrNoTopics = errors.New("stream: consumer must subscribe to at least one topic")
)

// Record is one message on a topic. Partition and Offset are assigned by the
// broker on produce and are meaningless on the way in.
type Record struct {
	Topic     string            `json:"topic"`
	Partition int32             `json:"partition"`
	Offset    int64             `json:"offset"`
	Key       []byte            `json:"key,omitempty"`
	Value     []byte            `json:"value,omitempty"`
	Headers   map[string]string `json:"headers,omitempty"`
	Timestamp time.Time         `json:"timestamp"`

	// ProducerID and Seq are the idempotence identity. A produce carrying a
	// (ProducerID, Seq) pair the broker has already accepted returns the
	// original partition and offset instead of appending a second copy, which
	// is what makes a retried produce safe to issue blindly.
	ProducerID string `json:"producer_id,omitempty"`
	Seq        int64  `json:"seq,omitempty"`
}

// TopicPartition names one partition of one topic. It is the unit of
// assignment, of offset commit and of ordering: ordering is guaranteed within a
// TopicPartition and nowhere else.
type TopicPartition struct {
	Topic     string `json:"topic"`
	Partition int32  `json:"partition"`
}

// String renders the partition as topic/partition, which is also the key used
// in the on-disk offset file.
func (tp TopicPartition) String() string {
	return tp.Topic + "/" + itoa(int64(tp.Partition))
}

// Less orders partitions so that every assignment, poll and rendering of a
// partition set is byte-identical between runs.
func (tp TopicPartition) Less(o TopicPartition) bool {
	if tp.Topic != o.Topic {
		return tp.Topic < o.Topic
	}
	return tp.Partition < o.Partition
}

// PartitionFor returns the partition a record lands in. Keys hash with FNV-1a
// so the mapping is stable across processes and languages; a record with no key
// hashes its value rather than round-robining, because round-robin is not
// reproducible and this platform's replays depend on reproducibility.
func PartitionFor(key, value []byte, partitions int) int32 {
	if partitions <= 1 {
		return 0
	}
	h := fnv.New32a()
	if len(key) > 0 {
		_, _ = h.Write(key)
	} else {
		_, _ = h.Write(value)
	}
	return int32(h.Sum32() % uint32(partitions))
}

func itoa(v int64) string {
	if v == 0 {
		return "0"
	}
	neg := v < 0
	if neg {
		v = -v
	}
	var buf [24]byte
	i := len(buf)
	for v > 0 {
		i--
		buf[i] = byte('0' + v%10)
		v /= 10
	}
	if neg {
		i--
		buf[i] = '-'
	}
	return string(buf[i:])
}
