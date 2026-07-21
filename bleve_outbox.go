package storm

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"math"
	"sort"
	"strings"
	"time"

	bolt "go.etcd.io/bbolt"
)

const (
	bleveOutboxBucket = "__storm_bleve_outbox"
	bleveStateBucket  = "__storm_bleve_state"

	durableIndexUpsert = "upsert"
	durableIndexDelete = "delete"
	durableIndexDrop   = "drop"
)

type durableIndexField struct {
	Name   string    `json:"name"`
	Kind   string    `json:"kind"`
	Text   string    `json:"text,omitempty"`
	Number uint64    `json:"number,omitempty"`
	Bool   bool      `json:"bool,omitempty"`
	Time   time.Time `json:"time,omitempty"`
}

type durableIndexMutation struct {
	Operation string              `json:"operation"`
	Table     string              `json:"table"`
	Root      []string            `json:"root,omitempty"`
	ID        []byte              `json:"id,omitempty"`
	Fields    []durableIndexField `json:"fields,omitempty"`
}

type durableIndexJob struct {
	Sequence  uint64                  `json:"sequence"`
	Schemas   map[string]storedSchema `json:"schemas,omitempty"`
	Mutations []durableIndexMutation  `json:"mutations"`
}

type durableIndexState struct {
	Desired uint64 `json:"desired"`
	Applied uint64 `json:"applied"`
	Dirty   bool   `json:"dirty,omitempty"`
}

type durableReindexTarget struct {
	Root  []string
	Table string
}

func durableReindexTargets(jobs []*durableIndexJob) []durableReindexTarget {
	seen := make(map[string]struct{})
	var targets []durableReindexTarget
	for _, job := range jobs {
		if job == nil {
			continue
		}
		for _, mutation := range job.Mutations {
			key := strings.Join(mutation.Root, "\x00") + "\x01" + mutation.Table
			if _, exists := seen[key]; exists {
				continue
			}
			seen[key] = struct{}{}
			targets = append(targets, durableReindexTarget{
				Root:  append([]string(nil), mutation.Root...),
				Table: mutation.Table,
			})
		}
	}
	return targets
}

func persistDurableIndexJob(tx *bolt.Tx, root []string, saves []*savedRecord, deletes []*deletedRecord, drops []string) (*durableIndexJob, error) {
	job := &durableIndexJob{Schemas: make(map[string]storedSchema)}

	for _, record := range saves {
		if record == nil || record.cfg == nil || !configUsesBleve(record.cfg) {
			continue
		}
		doc := record.doc
		if doc == nil {
			return nil, fmt.Errorf("missing prepared Bleve document for %s", record.cfg.Name)
		}
		fields, err := encodeDurableIndexFields(doc)
		if err != nil {
			return nil, err
		}
		job.Schemas[record.cfg.Name] = storedSchemaFromConfig(record.cfg)
		job.Mutations = append(job.Mutations, durableIndexMutation{
			Operation: durableIndexUpsert,
			Table:     record.cfg.Name,
			Root:      append([]string(nil), root...),
			ID:        append([]byte(nil), record.id...),
			Fields:    fields,
		})
	}

	for _, record := range deletes {
		if record == nil || record.cfg == nil || !configUsesBleve(record.cfg) {
			continue
		}
		job.Schemas[record.cfg.Name] = storedSchemaFromConfig(record.cfg)
		job.Mutations = append(job.Mutations, durableIndexMutation{
			Operation: durableIndexDelete,
			Table:     record.cfg.Name,
			Root:      append([]string(nil), root...),
			ID:        append([]byte(nil), record.id...),
		})
	}

	for _, table := range drops {
		if table == "" {
			continue
		}
		job.Mutations = append(job.Mutations, durableIndexMutation{
			Operation: durableIndexDrop,
			Table:     table,
			Root:      append([]string(nil), root...),
		})
	}

	if len(job.Mutations) == 0 {
		return nil, nil
	}

	outbox, err := tx.CreateBucketIfNotExists([]byte(bleveOutboxBucket))
	if err != nil {
		return nil, err
	}
	stateBucket, err := tx.CreateBucketIfNotExists([]byte(bleveStateBucket))
	if err != nil {
		return nil, err
	}
	sequence, err := outbox.NextSequence()
	if err != nil {
		return nil, err
	}
	job.Sequence = sequence

	raw, err := json.Marshal(job)
	if err != nil {
		return nil, err
	}
	if err := outbox.Put(indexSequenceKey(sequence), raw); err != nil {
		return nil, err
	}

	for table := range durableJobTables(job) {
		state, err := readDurableIndexState(stateBucket, table)
		if err != nil {
			return nil, err
		}
		state.Desired = sequence
		if err := writeDurableIndexState(stateBucket, table, state); err != nil {
			return nil, err
		}
	}
	return job, nil
}

func encodeDurableIndexFields(doc map[string]any) ([]durableIndexField, error) {
	names := make([]string, 0, len(doc))
	for name := range doc {
		names = append(names, name)
	}
	sort.Strings(names)

	fields := make([]durableIndexField, 0, len(names))
	for _, name := range names {
		field := durableIndexField{Name: name}
		switch value := doc[name].(type) {
		case string:
			field.Kind = "string"
			field.Text = value
		case float64:
			field.Kind = "number"
			field.Number = math.Float64bits(value)
		case bool:
			field.Kind = "bool"
			field.Bool = value
		case time.Time:
			field.Kind = "time"
			field.Time = value
		default:
			return nil, fmt.Errorf("unsupported durable Bleve field %s type %T", name, value)
		}
		fields = append(fields, field)
	}
	return fields, nil
}

func decodeDurableIndexFields(fields []durableIndexField) (map[string]any, error) {
	doc := make(map[string]any, len(fields))
	for _, field := range fields {
		switch field.Kind {
		case "string":
			doc[field.Name] = field.Text
		case "number":
			doc[field.Name] = math.Float64frombits(field.Number)
		case "bool":
			doc[field.Name] = field.Bool
		case "time":
			doc[field.Name] = field.Time
		default:
			return nil, fmt.Errorf("unsupported durable Bleve field kind %q", field.Kind)
		}
	}
	return doc, nil
}

func durableJobRecords(job *durableIndexJob) ([]*savedRecord, []*deletedRecord, []string, error) {
	if job == nil {
		return nil, nil, nil, nil
	}
	configs := make(map[string]*structConfig, len(job.Schemas))
	configFor := func(table string) (*structConfig, error) {
		if cfg := configs[table]; cfg != nil {
			return cfg, nil
		}
		schema, ok := job.Schemas[table]
		if !ok {
			return nil, fmt.Errorf("missing durable schema for %s", table)
		}
		cfg := structConfigFromStoredSchema(&schema)
		configs[table] = cfg
		return cfg, nil
	}

	var saves []*savedRecord
	var deletes []*deletedRecord
	var drops []string
	for _, mutation := range job.Mutations {
		switch mutation.Operation {
		case durableIndexDrop:
			drops = append(drops, mutation.Table)
		case durableIndexDelete:
			cfg, err := configFor(mutation.Table)
			if err != nil {
				return nil, nil, nil, err
			}
			deletes = append(deletes, &deletedRecord{cfg: cfg, id: append([]byte(nil), mutation.ID...)})
		case durableIndexUpsert:
			cfg, err := configFor(mutation.Table)
			if err != nil {
				return nil, nil, nil, err
			}
			doc, err := decodeDurableIndexFields(mutation.Fields)
			if err != nil {
				return nil, nil, nil, err
			}
			saves = append(saves, &savedRecord{cfg: cfg, id: append([]byte(nil), mutation.ID...), doc: doc})
		default:
			return nil, nil, nil, fmt.Errorf("unsupported durable Bleve operation %q", mutation.Operation)
		}
	}
	return saves, deletes, drops, nil
}

func loadDurableIndexJobs(db *bolt.DB) ([]*durableIndexJob, map[string]durableIndexState, error) {
	var jobs []*durableIndexJob
	states := make(map[string]durableIndexState)
	err := db.View(func(tx *bolt.Tx) error {
		if bucket := tx.Bucket([]byte(bleveOutboxBucket)); bucket != nil {
			if err := bucket.ForEach(func(_, raw []byte) error {
				if raw == nil {
					return nil
				}
				job := &durableIndexJob{}
				if err := json.Unmarshal(raw, job); err != nil {
					return err
				}
				jobs = append(jobs, job)
				return nil
			}); err != nil {
				return err
			}
		}
		if bucket := tx.Bucket([]byte(bleveStateBucket)); bucket != nil {
			return bucket.ForEach(func(table, raw []byte) error {
				if raw == nil {
					return nil
				}
				state := durableIndexState{}
				if err := json.Unmarshal(raw, &state); err != nil {
					return err
				}
				states[string(table)] = state
				return nil
			})
		}
		return nil
	})
	return jobs, states, err
}

func ackDurableIndexJobs(db *bolt.DB, jobs []*durableIndexJob) (map[string]bool, error) {
	clean := make(map[string]bool)
	err := db.Update(func(tx *bolt.Tx) error {
		outbox := tx.Bucket([]byte(bleveOutboxBucket))
		stateBucket, err := tx.CreateBucketIfNotExists([]byte(bleveStateBucket))
		if err != nil {
			return err
		}
		for _, job := range jobs {
			if job == nil {
				continue
			}
			if outbox != nil {
				if err := outbox.Delete(indexSequenceKey(job.Sequence)); err != nil {
					return err
				}
			}
			for table := range durableJobTables(job) {
				state, err := readDurableIndexState(stateBucket, table)
				if err != nil {
					return err
				}
				if job.Sequence > state.Applied {
					state.Applied = job.Sequence
				}
				state.Dirty = state.Applied < state.Desired
				clean[table] = !state.Dirty
				if err := writeDurableIndexState(stateBucket, table, state); err != nil {
					return err
				}
			}
		}
		return nil
	})
	return clean, err
}

func markDurableIndexJobsDirty(db *bolt.DB, jobs []*durableIndexJob) error {
	return db.Update(func(tx *bolt.Tx) error {
		bucket, err := tx.CreateBucketIfNotExists([]byte(bleveStateBucket))
		if err != nil {
			return err
		}
		for _, job := range jobs {
			for table := range durableJobTables(job) {
				state, err := readDurableIndexState(bucket, table)
				if err != nil {
					return err
				}
				state.Dirty = true
				if err := writeDurableIndexState(bucket, table, state); err != nil {
					return err
				}
			}
		}
		return nil
	})
}

func markDurableIndexTableDirty(db *bolt.DB, table string) error {
	return db.Update(func(tx *bolt.Tx) error {
		bucket, err := tx.CreateBucketIfNotExists([]byte(bleveStateBucket))
		if err != nil {
			return err
		}
		state, err := readDurableIndexState(bucket, table)
		if err != nil {
			return err
		}
		state.Dirty = true
		return writeDurableIndexState(bucket, table, state)
	})
}

func durableJobTables(job *durableIndexJob) map[string]struct{} {
	tables := make(map[string]struct{})
	if job == nil {
		return tables
	}
	for _, mutation := range job.Mutations {
		if mutation.Table != "" {
			tables[mutation.Table] = struct{}{}
		}
	}
	return tables
}

func readDurableIndexState(bucket *bolt.Bucket, table string) (durableIndexState, error) {
	state := durableIndexState{}
	if bucket == nil {
		return state, nil
	}
	raw := bucket.Get([]byte(table))
	if raw == nil {
		return state, nil
	}
	return state, json.Unmarshal(raw, &state)
}

func writeDurableIndexState(bucket *bolt.Bucket, table string, state durableIndexState) error {
	raw, err := json.Marshal(state)
	if err != nil {
		return err
	}
	return bucket.Put([]byte(table), raw)
}

func indexSequenceKey(sequence uint64) []byte {
	key := make([]byte, 8)
	binary.BigEndian.PutUint64(key, sequence)
	return key
}

func durableOutboxSequence(tx *bolt.Tx) uint64 {
	bucket := tx.Bucket([]byte(bleveOutboxBucket))
	if bucket == nil {
		return 0
	}
	return bucket.Sequence()
}

func durableIndexTableState(tx *bolt.Tx, table string) (durableIndexState, error) {
	return readDurableIndexState(tx.Bucket([]byte(bleveStateBucket)), table)
}

// completeDurableReindex removes mutations already represented by a rebuilt
// Bolt snapshot while preserving mutations for other tables in shared jobs.
func completeDurableReindex(tx *bolt.Tx, root []string, table string, through uint64) error {
	outbox := tx.Bucket([]byte(bleveOutboxBucket))
	if outbox != nil {
		type outboxEntry struct {
			key []byte
			raw []byte
		}
		var entries []outboxEntry
		cursor := outbox.Cursor()
		for key, raw := cursor.First(); key != nil; key, raw = cursor.Next() {
			if len(key) != 8 || binary.BigEndian.Uint64(key) > through || raw == nil {
				continue
			}
			entries = append(entries, outboxEntry{
				key: append([]byte(nil), key...),
				raw: append([]byte(nil), raw...),
			})
		}
		for _, entry := range entries {
			job := &durableIndexJob{}
			if err := json.Unmarshal(entry.raw, job); err != nil {
				return err
			}
			mutations := job.Mutations[:0]
			for _, mutation := range job.Mutations {
				if mutation.Table != table || !sameStringPath(mutation.Root, root) {
					mutations = append(mutations, mutation)
				}
			}
			if len(mutations) == len(job.Mutations) {
				continue
			}
			if len(mutations) == 0 {
				if err := outbox.Delete(entry.key); err != nil {
					return err
				}
				continue
			}
			job.Mutations = mutations
			delete(job.Schemas, table)
			updated, err := json.Marshal(job)
			if err != nil {
				return err
			}
			if err := outbox.Put(entry.key, updated); err != nil {
				return err
			}
		}
	}

	stateBucket, err := tx.CreateBucketIfNotExists([]byte(bleveStateBucket))
	if err != nil {
		return err
	}
	state, err := readDurableIndexState(stateBucket, table)
	if err != nil {
		return err
	}
	remaining, err := durableOutboxContainsTable(outbox, table)
	if err != nil {
		return err
	}
	if state.Desired <= through && !remaining {
		state.Applied = state.Desired
		state.Dirty = false
	} else {
		state.Dirty = true
	}
	return writeDurableIndexState(stateBucket, table, state)
}

func durableOutboxContainsTable(outbox *bolt.Bucket, table string) (bool, error) {
	if outbox == nil {
		return false, nil
	}
	found := false
	err := outbox.ForEach(func(_, raw []byte) error {
		if found || raw == nil {
			return nil
		}
		job := &durableIndexJob{}
		if err := json.Unmarshal(raw, job); err != nil {
			return err
		}
		for _, mutation := range job.Mutations {
			if mutation.Table == table {
				found = true
				break
			}
		}
		return nil
	})
	return found, err
}

func sameStringPath(left, right []string) bool {
	if len(left) != len(right) {
		return false
	}
	for i := range left {
		if left[i] != right[i] {
			return false
		}
	}
	return true
}

func durableOutboxEmpty(db *bolt.DB) (bool, error) {
	empty := true
	err := db.View(func(tx *bolt.Tx) error {
		bucket := tx.Bucket([]byte(bleveOutboxBucket))
		if bucket == nil {
			return nil
		}
		key, _ := bucket.Cursor().First()
		empty = key == nil
		return nil
	})
	return empty, err
}

func (s *DB) FlushBleve(ctx context.Context) error {
	if ctx == nil {
		ctx = context.Background()
	}
	for {
		empty, err := durableOutboxEmpty(s.Bolt)
		if err != nil || (empty && !s.indexer.hasPending()) {
			return err
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(10 * time.Millisecond):
		}
	}
}

// submitCommittedDurableIndexJob queues a job while indexCommitMu is still
// held, then releases the ordering gate before an optional synchronous wait.
func (n *node) submitCommittedDurableIndexJob(job *durableIndexJob) error {
	req := n.s.indexer.submitDurableIndexJob(job)
	n.s.indexCommitMu.Unlock()
	if n.s.bleveAsync {
		return nil
	}
	return req.wait()
}
