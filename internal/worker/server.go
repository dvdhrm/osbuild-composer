package worker

import (
	"encoding/json"
	"fmt"
	"io"
	"io/ioutil"
	"log"
	"net"
	"net/http"
	"strconv"
	"time"

	"github.com/google/uuid"
	"github.com/julienschmidt/httprouter"

	"github.com/osbuild/osbuild-composer/internal/common"
	"github.com/osbuild/osbuild-composer/internal/jobqueue"
	"github.com/osbuild/osbuild-composer/internal/osbuild"
	"github.com/osbuild/osbuild-composer/internal/target"
)

type Server struct {
	logger      *log.Logger
	jobs        jobqueue.JobQueue
	router      *httprouter.Router
	imageWriter WriteImageFunc
}

type WriteImageFunc func(composeID uuid.UUID, imageBuildID int, reader io.Reader) error

func NewServer(logger *log.Logger, jobs jobqueue.JobQueue, imageWriter WriteImageFunc) *Server {
	s := &Server{
		logger:      logger,
		jobs:        jobs,
		imageWriter: imageWriter,
	}

	s.router = httprouter.New()
	s.router.RedirectTrailingSlash = false
	s.router.RedirectFixedPath = false
	s.router.MethodNotAllowed = http.HandlerFunc(methodNotAllowedHandler)
	s.router.NotFound = http.HandlerFunc(notFoundHandler)

	s.router.POST("/job-queue/v1/jobs", s.addJobHandler)
	s.router.PATCH("/job-queue/v1/jobs/:job_id", s.updateJobHandler)
	s.router.POST("/job-queue/v1/jobs/:job_id/builds/:build_id/image", s.addJobImageHandler)

	return s
}

func (s *Server) Serve(listener net.Listener) error {
	server := http.Server{Handler: s}

	err := server.Serve(listener)
	if err != nil && err != http.ErrServerClosed {
		return err
	}

	return nil
}

func (s *Server) ServeHTTP(writer http.ResponseWriter, request *http.Request) {
	if s.logger != nil {
		log.Println(request.Method, request.URL.Path)
	}

	writer.Header().Set("Content-Type", "application/json; charset=utf-8")
	s.router.ServeHTTP(writer, request)
}

func (s *Server) Enqueue(manifest *osbuild.Manifest, targets []*target.Target) (uuid.UUID, error) {
	job := OSBuildJob{
		Manifest: manifest,
		Targets:  targets,
	}

	return s.jobs.Enqueue("osbuild", job, nil)
}

func (s *Server) JobStatus(id uuid.UUID) (state common.ComposeState, queued, started, finished time.Time, err error) {
	var result OSBuildJobResult
	var status jobqueue.JobStatus

	status, queued, started, finished, err = s.jobs.JobStatus(id, &result)
	if err != nil {
		return
	}

	state = composeStateFromJobStatus(status, result.OSBuildOutput)
	return
}

func (s *Server) JobResult(id uuid.UUID) (common.ComposeState, *common.ComposeResult, error) {
	var result OSBuildJobResult
	status, _, _, _, err := s.jobs.JobStatus(id, &result)
	if err != nil {
		return common.CWaiting, nil, err
	}

	return composeStateFromJobStatus(status, result.OSBuildOutput), result.OSBuildOutput, nil
}

// jsonErrorf() is similar to http.Error(), but returns the message in a json
// object with a "message" field.
func jsonErrorf(writer http.ResponseWriter, code int, message string, args ...interface{}) {
	writer.WriteHeader(code)

	// ignore error, because we cannot do anything useful with it
	_ = json.NewEncoder(writer).Encode(&errorResponse{
		Message: fmt.Sprintf(message, args...),
	})
}

func methodNotAllowedHandler(writer http.ResponseWriter, request *http.Request) {
	jsonErrorf(writer, http.StatusMethodNotAllowed, "method not allowed")
}

func notFoundHandler(writer http.ResponseWriter, request *http.Request) {
	jsonErrorf(writer, http.StatusNotFound, "not found")
}

func (s *Server) addJobHandler(writer http.ResponseWriter, request *http.Request, _ httprouter.Params) {
	contentType := request.Header["Content-Type"]
	if len(contentType) != 1 || contentType[0] != "application/json" {
		jsonErrorf(writer, http.StatusUnsupportedMediaType, "request must contain application/json data")
		return
	}

	var body addJobRequest
	err := json.NewDecoder(request.Body).Decode(&body)
	if err != nil {
		jsonErrorf(writer, http.StatusBadRequest, "%v", err)
		return
	}

	var job OSBuildJob
	id, err := s.jobs.Dequeue(request.Context(), []string{"osbuild"}, &job)
	if err != nil {
		jsonErrorf(writer, http.StatusInternalServerError, "%v", err)
		return
	}

	writer.WriteHeader(http.StatusCreated)
	// FIXME: handle or comment this possible error
	_ = json.NewEncoder(writer).Encode(addJobResponse{
		Id:       id,
		Manifest: job.Manifest,
		Targets:  job.Targets,
	})
}

func (s *Server) updateJobHandler(writer http.ResponseWriter, request *http.Request, params httprouter.Params) {
	contentType := request.Header["Content-Type"]
	if len(contentType) != 1 || contentType[0] != "application/json" {
		jsonErrorf(writer, http.StatusUnsupportedMediaType, "request must contain application/json data")
		return
	}

	id, err := uuid.Parse(params.ByName("job_id"))
	if err != nil {
		jsonErrorf(writer, http.StatusBadRequest, "cannot parse compose id: %v", err)
		return
	}

	var body updateJobRequest
	err = json.NewDecoder(request.Body).Decode(&body)
	if err != nil {
		jsonErrorf(writer, http.StatusBadRequest, "cannot parse request body: %v", err)
		return
	}

	// The jobqueue doesn't support setting the status before a job is
	// finished. This branch should never be hit, because the worker
	// doesn't attempt this. Change the API to remove this awkwardness.
	if body.Status != common.IBFinished && body.Status != common.IBFailed {
		jsonErrorf(writer, http.StatusBadRequest, "setting status of a job to waiting or running is not supported")
		return
	}

	err = s.jobs.FinishJob(id, OSBuildJobResult{OSBuildOutput: body.Result})
	if err != nil {
		switch err {
		case jobqueue.ErrNotExist:
			jsonErrorf(writer, http.StatusNotFound, "job does not exist: %s", id)
		case jobqueue.ErrNotRunning:
			jsonErrorf(writer, http.StatusBadRequest, "job is not running: %s", id)
		default:
			jsonErrorf(writer, http.StatusInternalServerError, "%v", err)
		}
		return
	}

	_ = json.NewEncoder(writer).Encode(updateJobResponse{})
}

func (s *Server) addJobImageHandler(writer http.ResponseWriter, request *http.Request, params httprouter.Params) {
	id, err := uuid.Parse(params.ByName("job_id"))
	if err != nil {
		jsonErrorf(writer, http.StatusBadRequest, "cannot parse compose id: %v", err)
		return
	}

	imageBuildId, err := strconv.Atoi(params.ByName("build_id"))
	if err != nil {
		jsonErrorf(writer, http.StatusBadRequest, "cannot parse image build id: %v", err)
		return
	}

	if s.imageWriter == nil {
		_, err = io.Copy(ioutil.Discard, request.Body)
	} else {
		err = s.imageWriter(id, imageBuildId, request.Body)
	}
	if err != nil {
		jsonErrorf(writer, http.StatusInternalServerError, "%v", err)
	}
}

func composeStateFromJobStatus(status jobqueue.JobStatus, output *common.ComposeResult) common.ComposeState {
	switch status {
	case jobqueue.JobPending:
		return common.CWaiting
	case jobqueue.JobRunning:
		return common.CRunning
	case jobqueue.JobFinished:
		if output.Success {
			return common.CFinished
		} else {
			return common.CFailed
		}
	}
	return common.CWaiting
}
