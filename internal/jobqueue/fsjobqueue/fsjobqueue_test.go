package fsjobqueue_test

import (
	"context"
	"encoding/json"
	"io/ioutil"
	"os"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/osbuild/osbuild-composer/internal/jobqueue"
	"github.com/osbuild/osbuild-composer/internal/jobqueue/fsjobqueue"
)

type testResult struct {
}

func cleanupTempDir(t *testing.T, dir string) {
	err := os.RemoveAll(dir)
	require.NoError(t, err)
}

func newTemporaryQueue(t *testing.T) (jobqueue.JobQueue, string) {
	dir, err := ioutil.TempDir("", "jobqueue-test-")
	require.NoError(t, err)

	q, err := fsjobqueue.New(dir)
	require.NoError(t, err)
	require.NotNil(t, q)

	return q, dir
}

func pushTestJob(t *testing.T, q jobqueue.JobQueue, jobType string, args interface{}, dependencies []uuid.UUID) uuid.UUID {
	id, err := q.Enqueue(jobType, args, dependencies)
	require.NoError(t, err)
	require.NotEmpty(t, id)
	return id
}

func finishNextTestJob(t *testing.T, q jobqueue.JobQueue, jobType string, result interface{}) uuid.UUID {
	id, err := q.Dequeue(context.Background(), []string{jobType}, &json.RawMessage{})
	require.NoError(t, err)
	require.NotEmpty(t, id)

	err = q.FinishJob(id, result)
	require.NoError(t, err)

	return id
}

func TestNonExistant(t *testing.T) {
	q, err := fsjobqueue.New("/non-existant-directory")
	require.Error(t, err)
	require.Nil(t, q)
}

func TestErrors(t *testing.T) {
	q, dir := newTemporaryQueue(t)
	defer cleanupTempDir(t, dir)

	// not serializable to JSON
	id, err := q.Enqueue("test", make(chan string), nil)
	require.Error(t, err)
	require.Equal(t, uuid.Nil, id)

	// invalid dependency
	id, err = q.Enqueue("test", "arg0", []uuid.UUID{uuid.New()})
	require.Error(t, err)
	require.Equal(t, uuid.Nil, id)
}

func TestArgs(t *testing.T) {
	type argument struct {
		I int
		S string
	}

	q, dir := newTemporaryQueue(t)
	defer cleanupTempDir(t, dir)

	oneargs := argument{7, "🐠"}
	one := pushTestJob(t, q, "fish", oneargs, nil)

	twoargs := argument{42, "🐙"}
	two := pushTestJob(t, q, "octopus", twoargs, nil)

	var args argument
	id, err := q.Dequeue(context.Background(), []string{"octopus"}, &args)
	require.NoError(t, err)
	require.Equal(t, two, id)
	require.Equal(t, twoargs, args)

	id, err = q.Dequeue(context.Background(), []string{"fish"}, &args)
	require.NoError(t, err)
	require.Equal(t, one, id)
	require.Equal(t, oneargs, args)
}

func TestJobTypes(t *testing.T) {
	q, dir := newTemporaryQueue(t)
	defer cleanupTempDir(t, dir)

	one := pushTestJob(t, q, "octopus", nil, nil)
	two := pushTestJob(t, q, "clownfish", nil, nil)

	require.Equal(t, two, finishNextTestJob(t, q, "clownfish", testResult{}))
	require.Equal(t, one, finishNextTestJob(t, q, "octopus", testResult{}))

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	id, err := q.Dequeue(ctx, []string{"zebra"}, nil)
	require.Equal(t, err, context.Canceled)
	require.Equal(t, uuid.Nil, id)
}

func TestDependencies(t *testing.T) {
	q, dir := newTemporaryQueue(t)
	defer cleanupTempDir(t, dir)

	t.Run("done-before-pushing-dependant", func(t *testing.T) {
		one := pushTestJob(t, q, "test", nil, nil)
		two := pushTestJob(t, q, "test", nil, nil)

		r := []uuid.UUID{}
		r = append(r, finishNextTestJob(t, q, "test", testResult{}))
		r = append(r, finishNextTestJob(t, q, "test", testResult{}))
		require.ElementsMatch(t, []uuid.UUID{one, two}, r)

		j := pushTestJob(t, q, "test", nil, []uuid.UUID{one, two})
		status, _, _, _, err := q.JobStatus(j, nil)
		require.NoError(t, err)
		require.Equal(t, jobqueue.JobPending, status)

		require.Equal(t, j, finishNextTestJob(t, q, "test", testResult{}))

		status, _, _, _, err = q.JobStatus(j, &testResult{})
		require.NoError(t, err)
		require.Equal(t, jobqueue.JobFinished, status)
	})

	t.Run("done-after-pushing-dependant", func(t *testing.T) {
		one := pushTestJob(t, q, "test", nil, nil)
		two := pushTestJob(t, q, "test", nil, nil)

		j := pushTestJob(t, q, "test", nil, []uuid.UUID{one, two})
		status, _, _, _, err := q.JobStatus(j, nil)
		require.NoError(t, err)
		require.Equal(t, jobqueue.JobPending, status)

		r := []uuid.UUID{}
		r = append(r, finishNextTestJob(t, q, "test", testResult{}))
		r = append(r, finishNextTestJob(t, q, "test", testResult{}))
		require.ElementsMatch(t, []uuid.UUID{one, two}, r)

		require.Equal(t, j, finishNextTestJob(t, q, "test", testResult{}))

		status, _, _, _, err = q.JobStatus(j, &testResult{})
		require.NoError(t, err)
		require.Equal(t, jobqueue.JobFinished, status)
	})
}
