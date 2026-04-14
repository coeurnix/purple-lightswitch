package runtime

import (
	"context"
	"errors"
	"sort"
	"time"
)

type QueueStats struct {
	Queued  int
	Running int
	Total   int
}

type manager struct {
	ctx    context.Context
	cancel context.CancelFunc

	mu    chan struct{}
	jobs  map[string]*Job
	queue []string
	wake  chan struct{}
	runFn func(context.Context, *Job) error

	onState          func(JobSnapshot, QueueStats)
	onClientSnapshot func(string, []JobSnapshot)
}

func newManager(runFn func(context.Context, *Job) error, onState func(JobSnapshot, QueueStats), onClientSnapshot func(string, []JobSnapshot)) *manager {
	ctx, cancel := context.WithCancel(context.Background())
	m := &manager{
		ctx:              ctx,
		cancel:           cancel,
		mu:               make(chan struct{}, 1),
		jobs:             map[string]*Job{},
		wake:             make(chan struct{}, 1),
		runFn:            runFn,
		onState:          onState,
		onClientSnapshot: onClientSnapshot,
	}
	m.mu <- struct{}{}
	go m.loop()
	return m
}

func (m *manager) Close() {
	m.cancel()
}

func (m *manager) Enqueue(job *Job) JobSnapshot {
	m.lock()
	m.jobs[job.ID] = job
	m.queue = append(m.queue, job.ID)
	snap := m.snapshotLocked(job)
	stats := m.statsLocked()
	jobs := m.jobsForClientLocked(job.ClientID)
	m.unlock()

	m.signal()
	m.onState(snap, stats)
	m.onClientSnapshot(job.ClientID, jobs)
	return snap
}

func (m *manager) CancelJob(jobID, clientID, reason string) (JobSnapshot, error) {
	m.lock()
	job := m.jobs[jobID]
	if job == nil {
		m.unlock()
		return JobSnapshot{}, errors.New("job not found")
	}
	if clientID != "" && job.ClientID != clientID {
		m.unlock()
		return JobSnapshot{}, errors.New("job does not belong to this client")
	}

	if job.Status == "completed" || job.Status == "failed" || job.Status == "canceled" {
		snap := m.snapshotLocked(job)
		m.unlock()
		return snap, nil
	}

	if job.Status == "queued" {
		job.Status = "canceled"
		job.Error = reason
		now := time.Now()
		job.EndedAt = &now
		m.removeQueuedLocked(jobID)
	} else if job.Status == "running" && job.cancel != nil {
		job.Error = reason
		job.cancel()
	}

	snap := m.snapshotLocked(job)
	stats := m.statsLocked()
	jobs := m.jobsForClientLocked(job.ClientID)
	m.unlock()

	m.onState(snap, stats)
	m.onClientSnapshot(job.ClientID, jobs)
	return snap, nil
}

func (m *manager) CancelClientJobs(clientID, reason string) ([]JobSnapshot, error) {
	m.lock()
	var affected []*Job
	for _, job := range m.jobs {
		if job.ClientID != clientID {
			continue
		}
		if job.Status == "completed" || job.Status == "failed" || job.Status == "canceled" {
			continue
		}
		if job.Status == "queued" {
			job.Status = "canceled"
			job.Error = reason
			now := time.Now()
			job.EndedAt = &now
			m.removeQueuedLocked(job.ID)
		} else if job.Status == "running" && job.cancel != nil {
			job.Error = reason
			job.cancel()
		}
		affected = append(affected, job)
	}
	if len(affected) == 0 {
		m.unlock()
		return nil, errors.New("no active jobs for client")
	}

	stats := m.statsLocked()
	snaps := make([]JobSnapshot, 0, len(affected))
	for _, job := range affected {
		snaps = append(snaps, m.snapshotLocked(job))
	}
	jobs := m.jobsForClientLocked(clientID)
	m.unlock()

	for _, snap := range snaps {
		m.onState(snap, stats)
	}
	m.onClientSnapshot(clientID, jobs)
	return snaps, nil
}

func (m *manager) JobsForClient(clientID string) []JobSnapshot {
	m.lock()
	defer m.unlock()
	return m.jobsForClientLocked(clientID)
}

func (m *manager) UpdateJobProgress(jobID string, current, total, percent int, phase string) {
	m.lock()
	job := m.jobs[jobID]
	if job == nil || job.Status != "running" {
		m.unlock()
		return
	}
	if current == job.ProgressCurrent &&
		total == job.ProgressTotal &&
		percent == job.ProgressPercent &&
		phase == job.ProgressPhase {
		m.unlock()
		return
	}
	job.ProgressCurrent = current
	job.ProgressTotal = total
	job.ProgressPercent = percent
	job.ProgressPhase = phase
	snap := m.snapshotLocked(job)
	stats := m.statsLocked()
	jobs := m.jobsForClientLocked(job.ClientID)
	m.unlock()

	m.onState(snap, stats)
	m.onClientSnapshot(job.ClientID, jobs)
}

func (m *manager) Stats() QueueStats {
	m.lock()
	defer m.unlock()
	return m.statsLocked()
}

func (m *manager) loop() {
	for {
		select {
		case <-m.ctx.Done():
			return
		default:
		}

		job := m.nextQueuedJob()
		if job == nil {
			select {
			case <-m.ctx.Done():
				return
			case <-m.wake:
				continue
			}
		}

		runCtx, cancel := context.WithCancel(m.ctx)
		m.lock()
		if job.Status != "queued" {
			m.unlock()
			cancel()
			continue
		}
		now := time.Now()
		job.Status = "running"
		job.StartedAt = &now
		job.cancel = cancel
		snap := m.snapshotLocked(job)
		stats := m.statsLocked()
		jobs := m.jobsForClientLocked(job.ClientID)
		m.unlock()

		m.onState(snap, stats)
		m.onClientSnapshot(job.ClientID, jobs)

		err := m.runFn(runCtx, job)

		m.lock()
		job.cancel = nil
		ended := time.Now()
		job.EndedAt = &ended
		switch {
		case errors.Is(err, context.Canceled):
			job.Status = "canceled"
			if job.Error == "" {
				job.Error = "canceled"
			}
		case err != nil:
			job.Status = "failed"
			job.Error = err.Error()
		default:
			job.Status = "completed"
			job.Error = ""
		}
		snap = m.snapshotLocked(job)
		stats = m.statsLocked()
		jobs = m.jobsForClientLocked(job.ClientID)
		m.unlock()

		m.onState(snap, stats)
		m.onClientSnapshot(job.ClientID, jobs)
	}
}

func (m *manager) nextQueuedJob() *Job {
	m.lock()
	defer m.unlock()
	for len(m.queue) > 0 {
		nextID := m.queue[0]
		m.queue = m.queue[1:]
		job := m.jobs[nextID]
		if job == nil || job.Status != "queued" {
			continue
		}
		return job
	}
	return nil
}

func (m *manager) signal() {
	select {
	case m.wake <- struct{}{}:
	default:
	}
}

func (m *manager) removeQueuedLocked(jobID string) {
	filtered := m.queue[:0]
	for _, queuedID := range m.queue {
		if queuedID != jobID {
			filtered = append(filtered, queuedID)
		}
	}
	m.queue = filtered
}

func (m *manager) jobsForClientLocked(clientID string) []JobSnapshot {
	list := make([]JobSnapshot, 0)
	for _, job := range m.jobs {
		if job.ClientID == clientID {
			list = append(list, m.snapshotLocked(job))
		}
	}
	sort.Slice(list, func(i, j int) bool {
		return list[i].CreatedAt.After(list[j].CreatedAt)
	})
	return list
}

func (m *manager) snapshotLocked(job *Job) JobSnapshot {
	return JobSnapshot{
		ID:              job.ID,
		ClientID:        job.ClientID,
		PresetID:        job.PresetID,
		PresetName:      job.PresetName,
		Status:          job.Status,
		Error:           job.Error,
		OutputURL:       outputURLForStatus(job),
		QueuePosition:   m.queuePositionLocked(job.ID),
		Width:           job.Width,
		Height:          job.Height,
		ProgressCurrent: job.ProgressCurrent,
		ProgressTotal:   job.ProgressTotal,
		ProgressPercent: job.ProgressPercent,
		ProgressPhase:   job.ProgressPhase,
		CreatedAt:       job.CreatedAt,
		StartedAt:       job.StartedAt,
		EndedAt:         job.EndedAt,
	}
}

func (m *manager) queuePositionLocked(jobID string) int {
	for index, queuedID := range m.queue {
		if queuedID == jobID {
			return index + 1
		}
	}
	return 0
}

func (m *manager) statsLocked() QueueStats {
	stats := QueueStats{Total: len(m.jobs)}
	for _, job := range m.jobs {
		switch job.Status {
		case "queued":
			stats.Queued++
		case "running":
			stats.Running++
		}
	}
	return stats
}

func (m *manager) lock() {
	<-m.mu
}

func (m *manager) unlock() {
	m.mu <- struct{}{}
}
