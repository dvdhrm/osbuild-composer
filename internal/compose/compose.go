// Package compose encapsulates the concept of a compose. It is a separate module from common, because it includes
// target which in turn includes common and thus it would create a cyclic dependency, which is forbidden in golang.
package compose

import (
	"time"

	"github.com/google/uuid"
	"github.com/osbuild/osbuild-composer/internal/blueprint"
	"github.com/osbuild/osbuild-composer/internal/common"
	"github.com/osbuild/osbuild-composer/internal/osbuild"
	"github.com/osbuild/osbuild-composer/internal/target"
)

type StateTransitionError struct {
	message string
}

func (ste *StateTransitionError) Error() string {
	return ste.message
}

// ImageBuild represents a single image build inside a compose
type ImageBuild struct {
	Id          int               `json:"id"`
	ImageType   common.ImageType  `json:"image_type"`
	Manifest    *osbuild.Manifest `json:"manifest"`
	Targets     []*target.Target  `json:"targets"`
	JobCreated  time.Time         `json:"job_created"`
	JobStarted  time.Time         `json:"job_started"`
	JobFinished time.Time         `json:"job_finished"`
	Size        uint64            `json:"size"`
	JobId       uuid.UUID         `json:"jobid,omitempty"`

	// Kept for backwards compatibility. Image builds which were done
	// before the move to the job queue use this to store whether they
	// finished successfully.
	QueueStatus common.ImageBuildState `json:"queue_status,omitempty"`
}

// DeepCopy creates a copy of the ImageBuild structure
func (ib *ImageBuild) DeepCopy() ImageBuild {
	var newManifestPtr *osbuild.Manifest = nil
	if ib.Manifest != nil {
		manifestCopy := *ib.Manifest
		newManifestPtr = &manifestCopy
	}
	var newTargets []*target.Target
	for _, t := range ib.Targets {
		newTarget := *t
		newTargets = append(newTargets, &newTarget)
	}
	// Create new image build struct
	return ImageBuild{
		Id:          ib.Id,
		QueueStatus: ib.QueueStatus,
		ImageType:   ib.ImageType,
		Manifest:    newManifestPtr,
		Targets:     newTargets,
		JobCreated:  ib.JobCreated,
		JobStarted:  ib.JobStarted,
		JobFinished: ib.JobFinished,
		Size:        ib.Size,
		JobId:       ib.JobId,
	}
}

func (ib *ImageBuild) GetLocalTargetOptions() *target.LocalTargetOptions {
	for _, t := range ib.Targets {
		switch options := t.Options.(type) {
		case *target.LocalTargetOptions:
			return options
		}
	}

	return nil
}

// A Compose represent the task of building a set of images from a single blueprint.
// It contains all the information necessary to generate the inputs for the job, as
// well as the job's state.
type Compose struct {
	Blueprint   *blueprint.Blueprint `json:"blueprint"`
	ImageBuilds []ImageBuild         `json:"image_builds"`
}

// DeepCopy creates a copy of the Compose structure
func (c *Compose) DeepCopy() Compose {
	var newBpPtr *blueprint.Blueprint = nil
	if c.Blueprint != nil {
		bpCopy := *c.Blueprint
		newBpPtr = &bpCopy
	}
	newImageBuilds := []ImageBuild{}
	for _, ib := range c.ImageBuilds {
		newImageBuilds = append(newImageBuilds, ib.DeepCopy())
	}
	return Compose{
		Blueprint:   newBpPtr,
		ImageBuilds: newImageBuilds,
	}
}

// UpdateState changes a state of a single image build inside the Compose
func (c *Compose) UpdateState(imageBuildId int, newState common.ImageBuildState) error {
	switch newState {
	case common.IBWaiting:
		return &StateTransitionError{"image build cannot be moved into waiting state"}
	case common.IBRunning:
		if c.ImageBuilds[imageBuildId].QueueStatus == common.IBWaiting || c.ImageBuilds[imageBuildId].QueueStatus == common.IBRunning {
			c.ImageBuilds[imageBuildId].QueueStatus = newState
		} else {
			return &StateTransitionError{"only waiting image build can be transitioned into running state"}
		}
	case common.IBFinished, common.IBFailed:
		if c.ImageBuilds[imageBuildId].QueueStatus == common.IBRunning {
			c.ImageBuilds[imageBuildId].QueueStatus = newState
			for _, t := range c.ImageBuilds[imageBuildId].Targets {
				t.Status = newState
			}
		} else {
			return &StateTransitionError{"only running image build can be transitioned into finished or failed state"}
		}
	default:
		return &StateTransitionError{"invalid state"}
	}
	return nil
}
