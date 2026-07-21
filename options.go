package storm

import (
	"os"
	"time"

	"github.com/clakeboy/storm-rev/v2/codec"
	"github.com/clakeboy/storm-rev/v2/index"
	bolt "go.etcd.io/bbolt"
)

// BoltOptions used to pass options to BoltDB.
func BoltOptions(mode os.FileMode, options *bolt.Options) func(*Options) error {
	return func(opts *Options) error {
		opts.boltMode = mode
		opts.boltOptions = options
		return nil
	}
}

// Codec used to set a custom encoder and decoder. The default is Sonic-backed JSON.
func Codec(c codec.MarshalUnmarshaler) func(*Options) error {
	return func(opts *Options) error {
		opts.codec = c
		return nil
	}
}

// Batch enables the use of batch instead of update for read-write transactions.
func Batch() func(*Options) error {
	return func(opts *Options) error {
		opts.batchMode = true
		return nil
	}
}

// BleveBatchDelay sets the positive group-commit window used to combine index
// mutations from concurrent writes after their Bolt transactions commit.
func BleveBatchDelay(delay time.Duration) func(*Options) error {
	return func(opts *Options) error {
		opts.bleveBatchDelay = delay
		return nil
	}
}

// BleveBatchMaxDocs sets a positive limit for document mutations in one Bleve batch.
func BleveBatchMaxDocs(max int) func(*Options) error {
	return func(opts *Options) error {
		opts.bleveBatchMaxDocs = max
		return nil
	}
}

// BleveBatchMaxBytes sets a positive limit for mapped document bytes in one Bleve batch.
func BleveBatchMaxBytes(max uint64) func(*Options) error {
	return func(opts *Options) error {
		opts.bleveBatchMaxBytes = max
		return nil
	}
}

// BleveBatchQueueSize sets a positive bound for committed write groups waiting
// for Bleve. Once full, new indexed writes apply backpressure after releasing
// Bolt's writer lock.
func BleveBatchQueueSize(size int) func(*Options) error {
	return func(opts *Options) error {
		opts.bleveBatchQueueSize = size
		return nil
	}
}

// BleveAsyncWrites explicitly selects the default asynchronous index behavior:
// mutating APIs return after the Bolt transaction and durable outbox commit.
func BleveAsyncWrites() func(*Options) error {
	return func(opts *Options) error {
		opts.bleveAsyncWrites = true
		return nil
	}
}

// BleveSyncWrites makes mutating APIs wait for their durable outbox entry to be
// applied to Bleve. It is intended for callers that require synchronous index
// visibility or immediate Bleve error reporting.
func BleveSyncWrites() func(*Options) error {
	return func(opts *Options) error {
		opts.bleveAsyncWrites = false
		return nil
	}
}

func Debug() func(*Options) error {
	return func(opts *Options) error {
		opts.debug = true
		return nil
	}
}

// Root used to set the root bucket. See also the From method.
func Root(root ...string) func(*Options) error {
	return func(opts *Options) error {
		opts.rootBucket = root
		return nil
	}
}

// UseDB allows Storm to use an existing open Bolt.DB.
// Warning: storm.DB.Close() will close the bolt.DB instance.
func UseDB(b *bolt.DB) func(*Options) error {
	return func(opts *Options) error {
		opts.path = b.Path()
		opts.bolt = b
		return nil
	}
}

// Limit sets the maximum number of records to return
func Limit(limit int) func(*index.Options) {
	return func(opts *index.Options) {
		opts.Limit = limit
	}
}

// Skip sets the number of records to skip
func Skip(offset int) func(*index.Options) {
	return func(opts *index.Options) {
		opts.Skip = offset
	}
}

// Reverse will return the results in descending order
func Reverse() func(*index.Options) {
	return func(opts *index.Options) {
		opts.Reverse = true
	}
}

// Options are used to customize the way Storm opens a database.
type Options struct {
	// Handles encoding and decoding of objects
	codec codec.MarshalUnmarshaler

	// Bolt file mode
	boltMode os.FileMode

	// Bolt options
	boltOptions *bolt.Options

	// Enable batch mode for read-write transaction, instead of update mode
	batchMode bool

	// Bleve group-commit and batch bounds.
	bleveBatchDelay     time.Duration
	bleveBatchMaxDocs   int
	bleveBatchMaxBytes  uint64
	bleveBatchQueueSize int
	bleveAsyncWrites    bool

	// The root bucket name
	rootBucket []string

	// Path of the database file
	path string

	// Bolt is still easily accessible
	bolt *bolt.DB

	// debug mode
	debug bool
}
