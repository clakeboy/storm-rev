package storm

import (
	"time"

	"github.com/blevesearch/bleve/v2"
)

const (
	defaultBleveBatchDelay     = 2 * time.Millisecond
	defaultBleveBatchMaxDocs   = 500
	defaultBleveBatchMaxBytes  = 8 << 20
	defaultBleveBatchQueueSize = 128
)

// bleveIndexRequest represents index work that belongs to one completed Bolt
// transaction. The request is acknowledged after every mutation in the group
// has been persisted by Bleve.
type bleveIndexRequest struct {
	saves   []*savedRecord
	deletes []*deletedRecord
	drops   []string
	jobs    []*durableIndexJob
	rebuild func() error
	done    chan error
	tables  map[string]struct{}
}

func completedBleveIndexRequest(err error) *bleveIndexRequest {
	req := &bleveIndexRequest{done: make(chan error, 1)}
	req.done <- err
	return req
}

func (r *bleveIndexRequest) wait() error {
	if r == nil {
		return nil
	}
	return <-r.done
}

// submitIndexMutations queues committed mutations without waiting for Bleve.
// Callers enqueue while holding DB.indexCommitMu, then release that mutex before
// waiting. This preserves Bolt commit order without keeping the Bolt writer lock
// while Bleve persists or merges segments.
func (m *bleveIndexManager) submitIndexMutations(saves []*savedRecord, deletes []*deletedRecord) *bleveIndexRequest {
	if m == nil || (len(saves) == 0 && len(deletes) == 0) {
		return completedBleveIndexRequest(nil)
	}

	req := newBleveIndexRequest(saves, deletes, nil, nil)
	return m.enqueueIndexRequest(req)
}

func newBleveIndexRequest(saves []*savedRecord, deletes []*deletedRecord, drops []string, jobs []*durableIndexJob) *bleveIndexRequest {
	req := &bleveIndexRequest{
		saves:   append([]*savedRecord(nil), saves...),
		deletes: append([]*deletedRecord(nil), deletes...),
		drops:   append([]string(nil), drops...),
		jobs:    append([]*durableIndexJob(nil), jobs...),
		done:    make(chan error, 1),
		tables:  make(map[string]struct{}),
	}
	for _, record := range req.saves {
		if record != nil && record.cfg != nil {
			req.tables[record.cfg.Name] = struct{}{}
		}
	}
	for _, record := range req.deletes {
		if record != nil && record.cfg != nil {
			req.tables[record.cfg.Name] = struct{}{}
		}
	}
	for _, table := range req.drops {
		if table != "" {
			req.tables[table] = struct{}{}
		}
	}
	return req
}

func (m *bleveIndexManager) submitDurableIndexJob(job *durableIndexJob) *bleveIndexRequest {
	if m == nil || job == nil {
		return completedBleveIndexRequest(nil)
	}
	select {
	case <-m.recoveryDone:
	case <-m.closing:
		return completedBleveIndexRequest(bleve.ErrorIndexClosed)
	}
	return m.submitDurableIndexJobNow(job)
}

func (m *bleveIndexManager) submitDurableIndexJobNow(job *durableIndexJob) *bleveIndexRequest {
	if m == nil || job == nil {
		return completedBleveIndexRequest(nil)
	}

	m.outboxMu.Lock()
	if req := m.inFlight[job.Sequence]; req != nil {
		m.outboxMu.Unlock()
		return req
	}
	saves, deletes, drops, err := durableJobRecords(job)
	if err != nil {
		m.outboxMu.Unlock()
		return completedBleveIndexRequest(err)
	}
	req := newBleveIndexRequest(saves, deletes, drops, []*durableIndexJob{job})
	m.inFlight[job.Sequence] = req
	m.outboxMu.Unlock()

	queued := m.enqueueIndexRequest(req)
	if queued != req {
		m.releaseDurableRequests(req)
	}
	return queued
}

func (m *bleveIndexManager) submitRebuild(table string, rebuild func() error) *bleveIndexRequest {
	if m == nil || rebuild == nil {
		return completedBleveIndexRequest(nil)
	}
	select {
	case <-m.recoveryDone:
	case <-m.closing:
		return completedBleveIndexRequest(bleve.ErrorIndexClosed)
	}
	req := &bleveIndexRequest{
		rebuild: rebuild,
		done:    make(chan error, 1),
		tables:  map[string]struct{}{table: {}},
	}
	return m.enqueueIndexRequest(req)
}

func (m *bleveIndexManager) enqueueIndexRequest(req *bleveIndexRequest) *bleveIndexRequest {
	if req == nil || (len(req.saves) == 0 && len(req.deletes) == 0 && len(req.drops) == 0 && req.rebuild == nil) {
		return completedBleveIndexRequest(nil)
	}

	m.queueMu.RLock()
	if m.queueClosed {
		m.queueMu.RUnlock()
		return completedBleveIndexRequest(bleve.ErrorIndexClosed)
	}
	m.markPending(req.tables)
	m.indexRequests <- req
	m.queueMu.RUnlock()
	return req
}

func (m *bleveIndexManager) runIndexCoordinator() {
	defer m.indexWG.Done()

	var carried *bleveIndexRequest
	for {
		first := carried
		carried = nil
		if first == nil {
			var ok bool
			first, ok = <-m.indexRequests
			if !ok {
				return
			}
		}
		if first.rebuild != nil {
			m.processIndexRequests([]*bleveIndexRequest{first})
			continue
		}

		requests := []*bleveIndexRequest{first}
		mutationCount := len(first.saves) + len(first.deletes) + len(first.drops)
		closed := false
		timer := time.NewTimer(m.batchDelay)

	collect:
		for mutationCount < m.batchMaxDocs {
			select {
			case req, ok := <-m.indexRequests:
				if !ok {
					closed = true
					break collect
				}
				if req.rebuild != nil {
					carried = req
					break collect
				}
				requests = append(requests, req)
				mutationCount += len(req.saves) + len(req.deletes) + len(req.drops)
			case <-timer.C:
				break collect
			}
		}
		if !timer.Stop() {
			select {
			case <-timer.C:
			default:
			}
		}
		m.processIndexRequests(requests)
		if closed && carried == nil {
			return
		}
	}
}

func (m *bleveIndexManager) processIndexRequests(requests []*bleveIndexRequest) {
	if m.requestObserver != nil {
		m.requestObserver(requests)
	}
	durable := durableRequests(requests)
	notified := false
	attempts := 0
	var finalErr error

	for {
		attempts++
		finalErr = m.applyIndexRequests(requests)
		if finalErr == nil && len(durable) > 0 {
			var clean map[string]bool
			clean, finalErr = ackDurableIndexJobs(m.db, durable)
			if finalErr == nil {
				for table, isClean := range clean {
					if isClean {
						m.clearDirty(table)
					}
				}
			}
		}
		if finalErr == nil || len(durable) == 0 {
			break
		}

		for _, req := range requests {
			for table := range req.tables {
				m.markDirty(table)
			}
		}
		_ = markDurableIndexJobsDirty(m.db, durable)
		if !notified {
			for _, req := range requests {
				req.done <- finalErr
			}
			notified = true
		}
		if attempts >= 3 && m.repair != nil && durableRequestsRepairable(requests) {
			if repairErr := m.repair(durableReindexTargets(durable), durableJobsMaxSequence(durable)); repairErr == nil {
				finalErr = nil
				break
			}
		}

		timer := time.NewTimer(250 * time.Millisecond)
		select {
		case <-timer.C:
		case <-m.closing:
			if !timer.Stop() {
				select {
				case <-timer.C:
				default:
				}
			}
			goto finished
		}
	}

finished:
	for _, req := range requests {
		m.finishPending(req.tables, finalErr)
		if !notified {
			req.done <- finalErr
		}
	}
	m.releaseDurableRequestsForJobs(durable)
}

func durableJobsMaxSequence(jobs []*durableIndexJob) uint64 {
	var sequence uint64
	for _, job := range jobs {
		if job != nil && job.Sequence > sequence {
			sequence = job.Sequence
		}
	}
	return sequence
}

func durableRequestsRepairable(requests []*bleveIndexRequest) bool {
	for _, req := range requests {
		if len(req.drops) > 0 {
			return false
		}
	}
	return true
}

func durableRequests(requests []*bleveIndexRequest) []*durableIndexJob {
	var jobs []*durableIndexJob
	for _, req := range requests {
		jobs = append(jobs, req.jobs...)
	}
	return jobs
}

func (m *bleveIndexManager) releaseDurableRequests(req *bleveIndexRequest) {
	if req == nil {
		return
	}
	m.releaseDurableRequestsForJobs(req.jobs)
}

func (m *bleveIndexManager) releaseDurableRequestsForJobs(jobs []*durableIndexJob) {
	m.outboxMu.Lock()
	defer m.outboxMu.Unlock()
	for _, job := range jobs {
		if job != nil {
			delete(m.inFlight, job.Sequence)
		}
	}
}

type bleveMutation struct {
	cfg    *structConfig
	data   any
	doc    map[string]any
	id     []byte
	delete bool
}

type bleveMutationGroup struct {
	cfg       *structConfig
	mutations map[string]*bleveMutation
	order     []string
}

// applyIndexRequests coalesces repeated mutations of the same document before
// writing. Channel arrival order matches Bolt commit order because submissions
// are serialized by DB.indexCommitMu.
func (m *bleveIndexManager) applyIndexRequests(requests []*bleveIndexRequest) error {
	var regular []*bleveIndexRequest
	flush := func() error {
		if len(regular) == 0 {
			return nil
		}
		err := m.applyMutationRequests(regular)
		regular = nil
		return err
	}

	for _, req := range requests {
		if req.rebuild != nil {
			if err := flush(); err != nil {
				return err
			}
			if err := req.rebuild(); err != nil {
				return err
			}
			continue
		}
		if len(req.drops) == 0 {
			regular = append(regular, req)
			continue
		}
		if err := flush(); err != nil {
			return err
		}
		for _, table := range req.drops {
			if err := m.dropTable(table); err != nil {
				return err
			}
		}
		if len(req.saves) > 0 || len(req.deletes) > 0 {
			regular = append(regular, req)
		}
	}
	return flush()
}

func (m *bleveIndexManager) applyMutationRequests(requests []*bleveIndexRequest) error {
	groups := make(map[string]*bleveMutationGroup)
	setMutation := func(mutation *bleveMutation) {
		if mutation == nil || mutation.cfg == nil {
			return
		}
		group := groups[mutation.cfg.Name]
		if group == nil {
			group = &bleveMutationGroup{
				cfg:       mutation.cfg,
				mutations: make(map[string]*bleveMutation),
			}
			groups[mutation.cfg.Name] = group
		}
		group.cfg = mutation.cfg
		key := string(mutation.id)
		if _, exists := group.mutations[key]; !exists {
			group.order = append(group.order, key)
		}
		group.mutations[key] = mutation
	}

	for _, req := range requests {
		for _, record := range req.saves {
			if record == nil {
				continue
			}
			setMutation(&bleveMutation{cfg: record.cfg, data: record.data, doc: record.doc, id: record.id})
		}
		for _, record := range req.deletes {
			if record == nil {
				continue
			}
			setMutation(&bleveMutation{cfg: record.cfg, id: record.id, delete: true})
		}
	}

	m.writeMu.Lock()
	defer m.writeMu.Unlock()
	for tableName, group := range groups {
		if err := m.applyMutationGroup(group); err != nil {
			m.markDirty(tableName)
			return err
		}
	}
	return nil
}

func (m *bleveIndexManager) applyMutationGroup(group *bleveMutationGroup) error {
	if group == nil || len(group.mutations) == 0 {
		return nil
	}
	idx, err := m.tableIndex(group.cfg)
	if err != nil {
		return err
	}

	batch := idx.NewBatch()
	flush := func() error {
		if batch.Size() == 0 {
			return nil
		}
		if m.batchObserver != nil {
			m.batchObserver(group.cfg.Name, batch.Size(), batch.TotalDocsSize())
		}
		if m.batchError != nil {
			if err := m.batchError(group.cfg.Name, batch.Size(), batch.TotalDocsSize()); err != nil {
				return err
			}
		}
		if err := idx.Batch(batch); err != nil {
			return err
		}
		batch = idx.NewBatch()
		return nil
	}

	for _, key := range group.order {
		mutation := group.mutations[key]
		if mutation.delete {
			batch.Delete(encodeDocID(mutation.id))
		} else {
			doc := mutation.doc
			if doc == nil {
				var err error
				doc, err = m.document(mutation.cfg, mutation.data)
				if err != nil {
					return err
				}
			}
			if err := batch.Index(encodeDocID(mutation.id), doc); err != nil {
				return err
			}
		}
		if (m.batchMaxDocs > 0 && batch.Size() >= m.batchMaxDocs) ||
			(m.batchMaxBytes > 0 && batch.TotalDocsSize() >= m.batchMaxBytes) {
			if err := flush(); err != nil {
				return err
			}
		}
	}
	return flush()
}

func (m *bleveIndexManager) markPending(tables map[string]struct{}) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for tableName := range tables {
		m.pending[tableName]++
	}
}

func (m *bleveIndexManager) hasPending() bool {
	if m == nil {
		return false
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.pending) > 0
}

func (m *bleveIndexManager) finishPending(tables map[string]struct{}, err error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for tableName := range tables {
		if m.pending[tableName] > 1 {
			m.pending[tableName]--
		} else {
			delete(m.pending, tableName)
		}
		if err != nil {
			m.dirty[tableName] = true
		}
	}
}

func (m *bleveIndexManager) stopIndexCoordinator() {
	m.closeOnce.Do(func() {
		close(m.closing)
	})
	m.recoveryWG.Wait()

	m.queueMu.Lock()
	if !m.queueClosed {
		m.queueClosed = true
		close(m.indexRequests)
	}
	m.queueMu.Unlock()
	m.indexWG.Wait()
}
