package common

import (
	"encoding/json"
)

func getStateMapping() []string {
	return []string{"WAITING", "RUNNING", "FINISHED", "FAILED"}
}

type ImageBuildState int

const (
	IBWaiting ImageBuildState = iota
	IBRunning
	IBFinished
	IBFailed
)

// ToString converts ImageBuildState into a human readable string
func (ibs ImageBuildState) ToString() string {
	return getStateMapping()[int(ibs)]
}

func unmarshalStateHelper(data []byte, mapping []string) (int, error) {
	var stringInput string
	err := json.Unmarshal(data, &stringInput)
	if err != nil {
		return 0, err
	}
	for n, str := range getStateMapping() {
		if str == stringInput {
			return n, nil
		}
	}
	return 0, &CustomJsonConversionError{"invalid image build status:" + stringInput}
}

// UnmarshalJSON converts a JSON string into an ImageBuildState
func (ibs *ImageBuildState) UnmarshalJSON(data []byte) error {
	val, err := unmarshalStateHelper(data, getStateMapping())
	if err != nil {
		return err
	}
	*ibs = ImageBuildState(val)
	return nil
}

func (ibs ImageBuildState) MarshalJSON() ([]byte, error) {
	return json.Marshal(getStateMapping()[ibs])
}

type ComposeState int

const (
	CWaiting ComposeState = iota
	CRunning
	CFinished
	CFailed
)

// ToString converts ImageBuildState into a human readable string
func (cs ComposeState) ToString() string {
	return getStateMapping()[int(cs)]
}

// UnmarshalJSON converts a JSON string into an ImageBuildState
func (ibs *ComposeState) UnmarshalJSON(data []byte) error {
	val, err := unmarshalStateHelper(data, getStateMapping())
	if err != nil {
		return err
	}
	*ibs = ComposeState(val)
	return nil
}

func (ibs ComposeState) MarshalJSON() ([]byte, error) {
	return json.Marshal(getStateMapping()[ibs])
}
