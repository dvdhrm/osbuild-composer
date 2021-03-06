// Package fsjobqueue implements a filesystem-backed job queue. It implements
// the interfaces in package jobqueue.
//
// Jobs are stored in the file system, using the `jsondb` package. However,
// this package does not use the file system as a database, but keeps some
// state in memory. This means that access to a given directory myst be
// exclusive to only one `fsJobQueue` object at a time. A single `fsJobQueue`
// can be safely accessed from multiple goroutines, though.
//
// Data is stored non-reduntantly. Any data structure necessary for efficient
// access (e.g., dependants) are kept in memory.
package fsjobqueue

import (
	"context"
	"encoding/json"
	"fmt"
	"reflect"
	"sort"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/osbuild/osbuild-composer/internal/jobqueue"
	"github.com/osbuild/osbuild-composer/internal/jsondb"
)

type fsJobQueue struct {
	db *jsondb.JSONDatabase

	// Maps job types to channels of job ids for that type. Only access
	// through pendingChannel() to ensure concurrent access is restricted
	// by the mutex.
	pending      map[string]chan uuid.UUID
	pendingMutex sync.Mutex

	// Maps job ids to the jobs that depend on it, if any of those
	// dependants have not yet finished. Only acccess while holding the
	// associated mutex.
	dependants      map[uuid.UUID][]uuid.UUID
	dependantsMutex sync.Mutex
}

// On-disk job struct. Contains all necessary (but non-redundant) information
// about a job. These are not held in memory by the job queue, but
// (de)serialized on each access.
type job struct {
	Id           uuid.UUID       `json:"id"`
	Type         string          `json:"type"`
	Args         json.RawMessage `json:"args,omitempty"`
	Dependencies []uuid.UUID     `json:"dependencies"`
	Result       json.RawMessage `json:"result,omitempty"`

	Status     jobqueue.JobStatus `json:"status"`
	QueuedAt   time.Time          `json:"queued-at,omitempty"`
	StartedAt  time.Time          `json:"started-at,omitempty"`
	FinishedAt time.Time          `json:"finished-at,omitempty"`
}

// Create a new fsJobQueue object for `dir`. This object must have exclusive
// access to `dir`. If `dir` contains jobs created from previous runs, they are
// loaded and rescheduled to run if necessary.
func New(dir string) (*fsJobQueue, error) {
	q := &fsJobQueue{
		db:         jsondb.New(dir, 0600),
		pending:    make(map[string]chan uuid.UUID),
		dependants: make(map[uuid.UUID][]uuid.UUID),
	}

	// Look for jobs that are still pending and build the dependant map.
	ids, err := q.db.List()
	if err != nil {
		return nil, fmt.Errorf("error listing jobs: %v", err)
	}
	for _, id := range ids {
		uuid, err := uuid.Parse(id)
		if err != nil {
			return nil, fmt.Errorf("invalid job '%s' in db: %v", id, err)
		}
		j, err := q.readJob(uuid)
		if err != nil {
			return nil, err
		}
		// We only enqueue jobs that were previously pending.
		if j.Status != jobqueue.JobPending {
			continue
		}
		// Initialize dependants for this job.
		q.dependantsMutex.Lock()
		defer q.dependantsMutex.Unlock()
		for _, dep := range j.Dependencies {
			q.dependants[dep] = append(q.dependants[dep], j.Id)
		}
		// Enqueue a job if all its dependencies (if there are any)
		// have finished, but the job itself hasn't run yet.
		n, err := q.countFinishedJobs(j.Dependencies)
		if err != nil {
			return nil, err
		}
		if n == len(j.Dependencies) {
			q.pendingChannel(j.Type) <- j.Id
		}
	}

	return q, nil
}

func (q *fsJobQueue) Enqueue(jobType string, args interface{}, dependencies []uuid.UUID) (uuid.UUID, error) {
	var j = job{
		Id:           uuid.New(),
		Type:         jobType,
		Dependencies: uniqueUUIDList(dependencies),
		Status:       jobqueue.JobPending,
		QueuedAt:     time.Now(),
	}

	var err error
	j.Args, err = json.Marshal(args)
	if err != nil {
		return uuid.Nil, fmt.Errorf("error marshaling job arguments: %v", err)
	}

	// Verify dependencies and check how many of them are already finished.
	finished, err := q.countFinishedJobs(j.Dependencies)
	if err != nil {
		return uuid.Nil, err
	}

	// Write the job before updating in-memory state, so that the latter
	// doesn't become corrupt when writing fails.
	err = q.db.Write(j.Id.String(), j)
	if err != nil {
		return uuid.Nil, fmt.Errorf("cannot write job: %v:", err)
	}

	// If all dependencies have finished, or there are none, queue the job.
	// Otherwise, update dependants so that this check is done again when
	// FinishJob() is called for a dependency.
	if finished == len(j.Dependencies) {
		q.pendingChannel(j.Type) <- j.Id
	} else {
		q.dependantsMutex.Lock()
		defer q.dependantsMutex.Unlock()
		for _, id := range j.Dependencies {
			q.dependants[id] = append(q.dependants[id], j.Id)
		}
	}

	return j.Id, nil
}

func (q *fsJobQueue) Dequeue(ctx context.Context, jobTypes []string, args interface{}) (uuid.UUID, error) {
	// Return early if the conext is already canceled.
	if err := ctx.Err(); err != nil {
		return uuid.Nil, err
	}

	// Use reflect.Select(), because the `select` statement cannot operate
	// on an unknown amount of channels.
	cases := []reflect.SelectCase{
		{
			Dir:  reflect.SelectRecv,
			Chan: reflect.ValueOf(ctx.Done()),
		},
	}
	for _, jt := range jobTypes {
		cases = append(cases, reflect.SelectCase{
			Dir:  reflect.SelectRecv,
			Chan: reflect.ValueOf(q.pendingChannel(jt)),
		})
	}

	chosen, value, recvOK := reflect.Select(cases)
	if chosen == 0 && !recvOK {
		return uuid.Nil, ctx.Err()
	}
	// pending channels are never closed
	if !recvOK {
		panic("pending channel was closed unexpectedly")
	}
	id := value.Interface().(uuid.UUID)

	j, err := q.readJob(id)
	if err != nil {
		return uuid.Nil, err
	}

	err = json.Unmarshal(j.Args, args)
	if err != nil {
		return uuid.Nil, fmt.Errorf("error unmarshaling arguments for job '%s': %v", j.Id, err)
	}

	j.Status = jobqueue.JobRunning
	j.StartedAt = time.Now()

	err = q.db.Write(id.String(), j)
	if err != nil {
		return uuid.Nil, fmt.Errorf("error writing job %s: %v", id, err)
	}

	return j.Id, nil
}

func (q *fsJobQueue) FinishJob(id uuid.UUID, result interface{}) error {
	j, err := q.readJob(id)
	if err != nil {
		return err
	}

	if j.Status != jobqueue.JobRunning {
		return jobqueue.ErrNotRunning
	}

	j.Status = jobqueue.JobFinished
	j.FinishedAt = time.Now()

	j.Result, err = json.Marshal(result)
	if err != nil {
		return fmt.Errorf("error marshaling result: %v", err)
	}

	// Write before notifying dependants, because it will be read again.
	err = q.db.Write(id.String(), j)
	if err != nil {
		return fmt.Errorf("error writing job %s: %v", id, err)
	}

	q.dependantsMutex.Lock()
	defer q.dependantsMutex.Unlock()
	for _, depid := range q.dependants[id] {
		dep, err := q.readJob(depid)
		if err != nil {
			return err
		}
		n, err := q.countFinishedJobs(dep.Dependencies)
		if err != nil {
			return err
		}
		if n == len(dep.Dependencies) {
			q.pendingChannel(dep.Type) <- dep.Id
		}
	}
	delete(q.dependants, id)

	return nil
}

func (q *fsJobQueue) JobStatus(id uuid.UUID, result interface{}) (status jobqueue.JobStatus, queued, started, finished time.Time, err error) {
	var j *job

	j, err = q.readJob(id)
	if err != nil {
		return
	}

	if j.Status == jobqueue.JobFinished {
		err = json.Unmarshal(j.Result, result)
		if err != nil {
			err = fmt.Errorf("error unmarshaling result for job '%s': %v", id, err)
			return
		}
	}

	status = j.Status
	queued = j.QueuedAt
	started = j.StartedAt
	finished = j.FinishedAt

	return
}

// Returns the number of finished jobs in `ids`.
func (q *fsJobQueue) countFinishedJobs(ids []uuid.UUID) (int, error) {
	n := 0
	for _, id := range ids {
		j, err := q.readJob(id)
		if err != nil {
			return 0, err
		}
		if j.Status == jobqueue.JobFinished {
			n += 1
		}
	}

	return n, nil
}

// Reads job with `id`. This is a thin wrapper around `q.db.Read`, which
// returns the job directly, or and error if a job with `id` does not exist.
func (q *fsJobQueue) readJob(id uuid.UUID) (*job, error) {
	var j job
	exists, err := q.db.Read(id.String(), &j)
	if err != nil {
		return nil, fmt.Errorf("error reading job '%s': %v", id, err)
	}
	if !exists {
		// return corrupt database?
		return nil, jobqueue.ErrNotExist
	}
	return &j, nil
}

// Safe access to the pending channel for `jobType`. Channels are created on
// demand.
func (q *fsJobQueue) pendingChannel(jobType string) chan uuid.UUID {
	q.pendingMutex.Lock()
	defer q.pendingMutex.Unlock()

	c, exists := q.pending[jobType]
	if !exists {
		c = make(chan uuid.UUID, 100)
		q.pending[jobType] = c
	}

	return c
}

// Sorts and removes duplicates from `ids`.
func uniqueUUIDList(ids []uuid.UUID) []uuid.UUID {
	s := map[uuid.UUID]bool{}
	for _, id := range ids {
		s[id] = true
	}

	l := []uuid.UUID{}
	for id := range s {
		l = append(l, id)
	}

	sort.Slice(l, func(i, j int) bool {
		for b := 0; b < 16; b++ {
			if l[i][b] < l[j][b] {
				return true
			}
		}
		return false
	})

	return l
}
