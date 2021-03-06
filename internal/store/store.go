// Package store contains primitives for representing and changing the
// osbuild-composer state.
package store

import (
	"bytes"
	"crypto/rand"
	"crypto/sha1"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/ioutil"
	"log"
	"os"
	"sort"
	"sync"
	"time"

	"github.com/osbuild/osbuild-composer/internal/compose"
	"github.com/osbuild/osbuild-composer/internal/distro"
	"github.com/osbuild/osbuild-composer/internal/jsondb"
	"github.com/osbuild/osbuild-composer/internal/osbuild"

	"github.com/osbuild/osbuild-composer/internal/blueprint"
	"github.com/osbuild/osbuild-composer/internal/common"
	"github.com/osbuild/osbuild-composer/internal/rpmmd"
	"github.com/osbuild/osbuild-composer/internal/target"

	"github.com/coreos/go-semver/semver"
	"github.com/google/uuid"
)

// The name under which to save the store to the underlying jsondb
const StoreDBName = "state"

// A Store contains all the persistent state of osbuild-composer, and is serialized
// on every change, and deserialized on start.
type Store struct {
	Blueprints        map[string]blueprint.Blueprint         `json:"blueprints"`
	Workspace         map[string]blueprint.Blueprint         `json:"workspace"`
	Composes          map[uuid.UUID]compose.Compose          `json:"composes"`
	Sources           map[string]SourceConfig                `json:"sources"`
	BlueprintsChanges map[string]map[string]blueprint.Change `json:"changes"`
	BlueprintsCommits map[string][]string                    `json:"commits"`

	mu          sync.RWMutex // protects all fields
	pendingJobs chan Job
	stateDir    *string
	db          *jsondb.JSONDatabase
}

// A Job contains the information about a compose a worker needs to process it.
type Job struct {
	ComposeID    uuid.UUID
	ImageBuildID int
	Manifest     *osbuild.Manifest
	Targets      []*target.Target
}

type SourceConfig struct {
	Name     string `json:"name" toml:"name"`
	Type     string `json:"type" toml:"type"`
	URL      string `json:"url" toml:"url"`
	CheckGPG bool   `json:"check_gpg" toml:"check_gpg"`
	CheckSSL bool   `json:"check_ssl" toml:"check_ssl"`
	System   bool   `json:"system" toml:"system"`
}

type NotFoundError struct {
	message string
}

func (e *NotFoundError) Error() string {
	return e.message
}

type NotPendingError struct {
	message string
}

func (e *NotPendingError) Error() string {
	return e.message
}

type InvalidRequestError struct {
	message string
}

func (e *InvalidRequestError) Error() string {
	return e.message
}

type NoLocalTargetError struct {
	message string
}

func (e *NoLocalTargetError) Error() string {
	return e.message
}

func New(stateDir *string) *Store {
	var s Store

	if stateDir != nil {
		err := os.Mkdir(*stateDir+"/"+"outputs", 0700)
		if err != nil && !os.IsExist(err) {
			log.Fatalf("cannot create output directory")
		}

		s.db = jsondb.New(*stateDir, 0600)
		_, err = s.db.Read(StoreDBName, &s)
		if err != nil {
			log.Fatalf("cannot read state: %v", err)
		}
	}

	s.pendingJobs = make(chan Job, 200)
	s.stateDir = stateDir

	if s.Blueprints == nil {
		s.Blueprints = make(map[string]blueprint.Blueprint)
	}
	if s.Workspace == nil {
		s.Workspace = make(map[string]blueprint.Blueprint)
	}
	if s.Composes == nil {
		s.Composes = make(map[uuid.UUID]compose.Compose)
	} else {
		// Backwards compatibility: fail all builds that are queued or
		// running. Jobs status is now handled outside of the store
		// (and the compose). The fields are kept so that previously
		// succeeded builds still show up correctly.
		for composeID, compose := range s.Composes {
			if len(compose.ImageBuilds) == 0 {
				panic("the was a compose with zero image builds, that is forbidden")
			}
			for imgID, imgBuild := range compose.ImageBuilds {
				switch imgBuild.QueueStatus {
				case common.IBRunning, common.IBWaiting:
					compose.ImageBuilds[imgID].QueueStatus = common.IBFailed
					s.Composes[composeID] = compose
				}
			}
		}

	}
	if s.Sources == nil {
		s.Sources = make(map[string]SourceConfig)
	}
	if s.BlueprintsChanges == nil {
		s.BlueprintsChanges = make(map[string]map[string]blueprint.Change)
	}
	if s.BlueprintsCommits == nil {
		s.BlueprintsCommits = make(map[string][]string)
	}

	// Populate BlueprintsCommits for existing blueprints without commit history
	// BlueprintsCommits tracks the order of the commits in BlueprintsChanges,
	// but may not be in-sync with BlueprintsChanges because it was added later.
	// This will sort the existing commits by timestamp and version to update
	// the store. BUT since the timestamp resolution is only 1s it is possible
	// that the order may be slightly wrong.
	for name := range s.BlueprintsChanges {
		if len(s.BlueprintsChanges[name]) != len(s.BlueprintsCommits[name]) {
			changes := make([]blueprint.Change, 0, len(s.BlueprintsChanges[name]))

			for commit := range s.BlueprintsChanges[name] {
				changes = append(changes, s.BlueprintsChanges[name][commit])
			}

			// Sort the changes by Timestamp then version, ascending
			sort.Slice(changes, func(i, j int) bool {
				if changes[i].Timestamp == changes[j].Timestamp {
					vI, err := semver.NewVersion(changes[i].Blueprint.Version)
					if err != nil {
						vI = semver.New("0.0.0")
					}
					vJ, err := semver.NewVersion(changes[j].Blueprint.Version)
					if err != nil {
						vJ = semver.New("0.0.0")
					}

					return vI.LessThan(*vJ)
				}
				return changes[i].Timestamp < changes[j].Timestamp
			})

			commits := make([]string, 0, len(changes))
			for _, c := range changes {
				commits = append(commits, c.Commit)
			}

			s.BlueprintsCommits[name] = commits
		}
	}

	return &s
}

func randomSHA1String() (string, error) {
	hash := sha1.New()
	data := make([]byte, 20)
	n, err := rand.Read(data)
	if err != nil {
		return "", err
	} else if n != 20 {
		return "", errors.New("randomSHA1String: short read from rand")
	}
	_, err = hash.Write(data)
	if err != nil {
		return "", err
	}
	return hex.EncodeToString(hash.Sum(nil)), nil
}

func (s *Store) change(f func() error) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	result := f()

	if s.stateDir != nil {
		err := s.db.Write(StoreDBName, s)
		if err != nil {
			panic(err)
		}
	}

	return result
}

func (s *Store) ListBlueprints() []string {
	s.mu.RLock()
	defer s.mu.RUnlock()

	names := make([]string, 0, len(s.Blueprints))
	for name := range s.Blueprints {
		names = append(names, name)
	}
	sort.Strings(names)

	return names
}

func (s *Store) GetBlueprint(name string) (*blueprint.Blueprint, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	bp, inWorkspace := s.Workspace[name]
	if !inWorkspace {
		var ok bool
		bp, ok = s.Blueprints[name]
		if !ok {
			return nil, false
		}
	}

	return &bp, inWorkspace
}

func (s *Store) GetBlueprintCommitted(name string) *blueprint.Blueprint {
	s.mu.RLock()
	defer s.mu.RUnlock()

	bp, ok := s.Blueprints[name]
	if !ok {
		return nil
	}

	return &bp
}

// GetBlueprintChange returns a specific change to a blueprint
// If the blueprint or change do not exist then an error is returned
func (s *Store) GetBlueprintChange(name string, commit string) (*blueprint.Change, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	if _, ok := s.BlueprintsChanges[name]; !ok {
		return nil, errors.New("Unknown blueprint")
	}
	change, ok := s.BlueprintsChanges[name][commit]
	if !ok {
		return nil, errors.New("Unknown commit")
	}
	return &change, nil
}

// GetBlueprintChanges returns the list of changes, oldest first
func (s *Store) GetBlueprintChanges(name string) []blueprint.Change {
	s.mu.RLock()
	defer s.mu.RUnlock()

	var changes []blueprint.Change

	for _, commit := range s.BlueprintsCommits[name] {
		changes = append(changes, s.BlueprintsChanges[name][commit])
	}

	return changes
}

func (s *Store) PushBlueprint(bp blueprint.Blueprint, commitMsg string) error {
	return s.change(func() error {
		commit, err := randomSHA1String()
		if err != nil {
			return err
		}

		// Make sure the blueprint has default values and that the version is valid
		err = bp.Initialize()
		if err != nil {
			return err
		}

		timestamp := time.Now().Format("2006-01-02T15:04:05Z")
		change := blueprint.Change{
			Commit:    commit,
			Message:   commitMsg,
			Timestamp: timestamp,
			Blueprint: bp,
		}

		delete(s.Workspace, bp.Name)
		if s.BlueprintsChanges[bp.Name] == nil {
			s.BlueprintsChanges[bp.Name] = make(map[string]blueprint.Change)
		}
		s.BlueprintsChanges[bp.Name][commit] = change
		// Keep track of the order of the commits
		s.BlueprintsCommits[bp.Name] = append(s.BlueprintsCommits[bp.Name], commit)

		if old, ok := s.Blueprints[bp.Name]; ok {
			if bp.Version == "" || bp.Version == old.Version {
				bp.BumpVersion(old.Version)
			}
		}
		s.Blueprints[bp.Name] = bp
		return nil
	})
}

func (s *Store) PushBlueprintToWorkspace(bp blueprint.Blueprint) error {
	return s.change(func() error {
		// Make sure the blueprint has default values and that the version is valid
		err := bp.Initialize()
		if err != nil {
			return err
		}

		s.Workspace[bp.Name] = bp
		return nil
	})
}

// DeleteBlueprint will remove the named blueprint from the store
// if the blueprint does not exist it will return an error
// The workspace copy is deleted unconditionally, it will not return an error if it does not exist.
func (s *Store) DeleteBlueprint(name string) error {
	return s.change(func() error {
		delete(s.Workspace, name)
		if _, ok := s.Blueprints[name]; !ok {
			return fmt.Errorf("Unknown blueprint: %s", name)
		}
		delete(s.Blueprints, name)
		return nil
	})
}

// DeleteBlueprintFromWorkspace deletes the workspace copy of a blueprint
// if the blueprint doesn't exist in the workspace it returns an error
func (s *Store) DeleteBlueprintFromWorkspace(name string) error {
	return s.change(func() error {
		if _, ok := s.Workspace[name]; !ok {
			return fmt.Errorf("Unknown blueprint: %s", name)
		}
		delete(s.Workspace, name)
		return nil
	})
}

// TagBlueprint will tag the most recent commit
// It will return an error if the blueprint doesn't exist
func (s *Store) TagBlueprint(name string) error {
	return s.change(func() error {
		_, ok := s.Blueprints[name]
		if !ok {
			return errors.New("Unknown blueprint")
		}

		if len(s.BlueprintsCommits[name]) == 0 {
			return errors.New("No commits for blueprint")
		}

		latest := s.BlueprintsCommits[name][len(s.BlueprintsCommits[name])-1]
		// If the most recent commit already has a revision, don't bump it
		if s.BlueprintsChanges[name][latest].Revision != nil {
			return nil
		}

		// Get the latest revision for this blueprint
		var revision int
		var change blueprint.Change
		for i := len(s.BlueprintsCommits[name]) - 1; i >= 0; i-- {
			commit := s.BlueprintsCommits[name][i]
			change = s.BlueprintsChanges[name][commit]
			if change.Revision != nil && *change.Revision > revision {
				revision = *change.Revision
				break
			}
		}

		// Bump the revision (if there was none it will start at 1)
		revision++
		change.Revision = &revision
		s.BlueprintsChanges[name][latest] = change
		return nil
	})
}

func (s *Store) GetCompose(id uuid.UUID) (compose.Compose, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	compose, exists := s.Composes[id]
	return compose, exists
}

// GetAllComposes creates a deep copy of all composes present in this store
// and returns them as a dictionary with compose UUIDs as keys
func (s *Store) GetAllComposes() map[uuid.UUID]compose.Compose {
	s.mu.RLock()
	defer s.mu.RUnlock()

	composes := make(map[uuid.UUID]compose.Compose)

	for id, singleCompose := range s.Composes {
		newCompose := singleCompose.DeepCopy()
		composes[id] = newCompose
	}

	return composes
}

func (s *Store) GetImageBuildResult(composeId uuid.UUID, imageBuildId int) (io.ReadCloser, error) {
	if s.stateDir == nil {
		return ioutil.NopCloser(bytes.NewBuffer([]byte("{}"))), nil
	}

	return os.Open(s.getImageBuildDirectory(composeId, imageBuildId) + "/result.json")
}

func (s *Store) GetImageBuildImage(composeId uuid.UUID, imageBuildId int) (io.ReadCloser, int64, error) {
	c, ok := s.Composes[composeId]

	if !ok {
		return nil, 0, &NotFoundError{"compose does not exist"}
	}

	localTargetOptions := c.ImageBuilds[imageBuildId].GetLocalTargetOptions()
	if localTargetOptions == nil {
		return nil, 0, &NoLocalTargetError{"compose does not have local target"}
	}

	path := fmt.Sprintf("%s/%s", s.getImageBuildDirectory(composeId, imageBuildId), localTargetOptions.Filename)

	f, err := os.Open(path)

	if err != nil {
		return nil, 0, err
	}

	fileInfo, err := f.Stat()

	if err != nil {
		return nil, 0, err
	}

	return f, fileInfo.Size(), err

}

func (s *Store) getComposeDirectory(composeID uuid.UUID) string {
	return fmt.Sprintf("%s/outputs/%s", *s.stateDir, composeID.String())
}

func (s *Store) getImageBuildDirectory(composeID uuid.UUID, imageBuildID int) string {
	return fmt.Sprintf("%s/%d", s.getComposeDirectory(composeID), imageBuildID)
}

func (s *Store) PushCompose(composeID uuid.UUID, manifest *osbuild.Manifest, imageType distro.ImageType, bp *blueprint.Blueprint, size uint64, targets []*target.Target, jobId uuid.UUID) error {
	if _, exists := s.GetCompose(composeID); exists {
		panic("a compose with this id already exists")
	}

	if targets == nil {
		targets = []*target.Target{}
	}

	// Compatibility layer for image types in Weldr API v0
	imageTypeCommon, exists := common.ImageTypeFromCompatString(imageType.Name())
	if !exists {
		panic("fatal error, compose type does not exist")
	}

	if s.stateDir != nil {
		outputDir := s.getImageBuildDirectory(composeID, 0)

		err := os.MkdirAll(outputDir, 0755)
		if err != nil {
			return fmt.Errorf("cannot create output directory for job %v: %#v", composeID, err)
		}
	}

	// FIXME: handle or comment this possible error
	_ = s.change(func() error {
		s.Composes[composeID] = compose.Compose{
			Blueprint: bp,
			ImageBuilds: []compose.ImageBuild{
				{
					Manifest:   manifest,
					ImageType:  imageTypeCommon,
					Targets:    targets,
					JobCreated: time.Now(),
					Size:       size,
					JobId:      jobId,
				},
			},
		}
		return nil
	})
	return nil
}

// PushTestCompose is used for testing
// Set testSuccess to create a fake successful compose, otherwise it will create a failed compose
// It does not actually run a compose job
func (s *Store) PushTestCompose(composeID uuid.UUID, manifest *osbuild.Manifest, imageType distro.ImageType, bp *blueprint.Blueprint, size uint64, targets []*target.Target, testSuccess bool) error {
	if targets == nil {
		targets = []*target.Target{}
	}

	// Compatibility layer for image types in Weldr API v0
	imageTypeCommon, exists := common.ImageTypeFromCompatString(imageType.Name())
	if !exists {
		panic("fatal error, compose type does not exist")
	}

	if s.stateDir != nil {
		outputDir := s.getImageBuildDirectory(composeID, 0)

		err := os.MkdirAll(outputDir, 0755)
		if err != nil {
			return fmt.Errorf("cannot create output directory for job %v: %#v", composeID, err)
		}
	}

	// FIXME: handle or comment this possible error
	_ = s.change(func() error {
		s.Composes[composeID] = compose.Compose{
			Blueprint: bp,
			ImageBuilds: []compose.ImageBuild{
				{
					QueueStatus: common.IBRunning,
					Manifest:    manifest,
					ImageType:   imageTypeCommon,
					Targets:     targets,
					JobCreated:  time.Now(),
					JobStarted:  time.Now(),
					Size:        size,
				},
			},
		}
		return nil
	})

	var status common.ImageBuildState
	var result common.ComposeResult
	if testSuccess {
		status = common.IBFinished
		result = common.ComposeResult{Success: true}
	} else {
		status = common.IBFailed
		result = common.ComposeResult{}
	}

	// Instead of starting the job, immediately set a final status
	err := s.UpdateImageBuildInCompose(composeID, 0, status, &result)
	if err != nil {
		return err
	}

	return nil
}

// DeleteCompose deletes the compose from the state file and also removes all files on disk that are
// associated with this compose
func (s *Store) DeleteCompose(id uuid.UUID) error {
	return s.change(func() error {
		if _, exists := s.Composes[id]; !exists {
			return &NotFoundError{}
		}

		delete(s.Composes, id)

		var err error
		if s.stateDir != nil {
			err = os.RemoveAll(s.getComposeDirectory(id))
			if err != nil {
				return err
			}
		}

		return err
	})
}

// UpdateImageBuildInCompose sets the status and optionally also the final image.
func (s *Store) UpdateImageBuildInCompose(composeID uuid.UUID, imageBuildID int, status common.ImageBuildState, result *common.ComposeResult) error {
	return s.change(func() error {
		// Check that the compose exists
		currentCompose, exists := s.Composes[composeID]
		if !exists {
			return &NotFoundError{"compose does not exist"}
		}
		// Check that the image build was waiting
		if currentCompose.ImageBuilds[imageBuildID].QueueStatus == common.IBWaiting {
			return &NotPendingError{"compose has not been popped"}
		}

		// write result into file
		if s.stateDir != nil && result != nil {
			f, err := os.Create(s.getImageBuildDirectory(composeID, imageBuildID) + "/result.json")

			if err != nil {
				return fmt.Errorf("cannot open result.json for job %v: %#v", composeID, err)
			}

			// FIXME: handle error
			_ = json.NewEncoder(f).Encode(result)
		}

		// Update the image build state including all target states
		err := currentCompose.UpdateState(imageBuildID, status)
		if err != nil {
			// TODO: log error
			return &InvalidRequestError{"invalid state transition: " + err.Error()}
		}

		// In case the image build is done, store the time and possibly also the image
		if status == common.IBFinished || status == common.IBFailed {
			currentCompose.ImageBuilds[imageBuildID].JobFinished = time.Now()
		}

		s.Composes[composeID] = currentCompose

		return nil
	})
}

func (s *Store) AddImageToImageUpload(composeID uuid.UUID, imageBuildID int, reader io.Reader) error {
	currentCompose, exists := s.Composes[composeID]
	if !exists {
		return &NotFoundError{"compose does not exist"}
	}

	localTargetOptions := currentCompose.ImageBuilds[imageBuildID].GetLocalTargetOptions()
	if localTargetOptions == nil {
		return &NoLocalTargetError{fmt.Sprintf("image upload requested for compse %s and image build %d but it has no local target", composeID.String(), imageBuildID)}
	}

	path := fmt.Sprintf("%s/%s", s.getImageBuildDirectory(composeID, imageBuildID), localTargetOptions.Filename)
	f, err := os.Create(path)

	if err != nil {
		return err
	}

	_, err = io.Copy(f, reader)

	if err != nil {
		return err
	}

	return nil
}

func (s *Store) PushSource(source SourceConfig) {
	// FIXME: handle or comment this possible error
	_ = s.change(func() error {
		s.Sources[source.Name] = source
		return nil
	})
}

func (s *Store) DeleteSource(name string) {
	// FIXME: handle or comment this possible error
	_ = s.change(func() error {
		delete(s.Sources, name)
		return nil
	})
}

func (s *Store) ListSources() []string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	names := make([]string, 0, len(s.Sources))
	for name := range s.Sources {
		names = append(names, name)
	}
	sort.Strings(names)

	return names
}

func (s *Store) GetSource(name string) *SourceConfig {
	s.mu.RLock()
	defer s.mu.RUnlock()

	source, ok := s.Sources[name]
	if !ok {
		return nil
	}
	return &source
}

func (s *Store) GetAllSources() map[string]SourceConfig {
	s.mu.RLock()
	defer s.mu.RUnlock()

	sources := make(map[string]SourceConfig)

	for k, v := range s.Sources {
		sources[k] = v
	}

	return sources
}

func NewSourceConfig(repo rpmmd.RepoConfig, system bool) SourceConfig {
	sc := SourceConfig{
		Name:     repo.Id,
		CheckGPG: true,
		CheckSSL: !repo.IgnoreSSL,
		System:   system,
	}

	if repo.BaseURL != "" {
		sc.URL = repo.BaseURL
		sc.Type = "yum-baseurl"
	} else if repo.Metalink != "" {
		sc.URL = repo.Metalink
		sc.Type = "yum-metalink"
	} else if repo.MirrorList != "" {
		sc.URL = repo.MirrorList
		sc.Type = "yum-mirrorlist"
	}

	return sc
}

func (s *SourceConfig) RepoConfig() rpmmd.RepoConfig {
	var repo rpmmd.RepoConfig

	repo.Id = s.Name
	repo.IgnoreSSL = !s.CheckSSL

	if s.Type == "yum-baseurl" {
		repo.BaseURL = s.URL
	} else if s.Type == "yum-metalink" {
		repo.Metalink = s.URL
	} else if s.Type == "yum-mirrorlist" {
		repo.MirrorList = s.URL
	}

	return repo
}
