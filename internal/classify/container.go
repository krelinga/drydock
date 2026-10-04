package classify

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
)

// 5. The devcontainer CLI result (design §6)

// ContainerOutcome is the result of a `devcontainer up`.
type ContainerOutcome uint8

const (
	ContainerRunning ContainerOutcome = iota
	ContainerFailed
)

// Container is the parsed result.
//
// Step is what makes a failure actionable: design §6 writes an event for
// every step precisely so the UI can name the one that failed. **The CLI does
// not supply it, so ClassifyContainer never sets it.** Recorded against
// devcontainer 0.89.0, a failed `up` reports only `outcome`, `message`, and a
// human `description`, which is sometimes specific ("postCreateCommand from
// devcontainer.json failed.") and sometimes not ("An error occurred setting up
// the container." for an image that cannot be pulled). Deriving a step from
// that prose would be inventing one. Step is filled by Drydock's own step
// tracking around the calls it makes — `up` is one of §6's eight steps, not a
// source of them.
//
// ContainerID is set on failure too when the CLI reports one: a failed
// postCreateCommand leaves the container created and running, so a failed
// `up` can still own a container that teardown and reconciliation must know
// about.
//
// Message and Description are the CLI's own words, for display beside the
// step. Message can quote a command line from the repository's
// devcontainer.json; treat it as untrusted text.
type Container struct {
	Outcome     ContainerOutcome
	ContainerID string
	RemoteUser  string
	Step        string
	Message     string
	Description string
}

// upResult is the shape `devcontainer up` prints on stdout as one JSON
// object, success or failure (0.89.0). Logs go to stderr regardless of
// --log-format; there is no `--json` flag, and passing one is a usage error
// with empty stdout.
type upResult struct {
	Outcome     *string `json:"outcome"`
	ContainerID string  `json:"containerId"`
	RemoteUser  string  `json:"remoteUser"`
	Message     string  `json:"message"`
	Description string  `json:"description"`
}

// ClassifyContainer parses the stdout of `devcontainer up`. Never scrape
// `docker ps` — the machine-readable result is the contract (§2.5), and the
// fake binary's contract test pins its shape (testing §6.1).
//
// Input is stdout alone, and exactly one JSON object is expected: trailing
// data, a missing `outcome`, or a success without a `containerId` is an error
// rather than a verdict, because each means the contract moved. `outcome`
// "success" is ContainerRunning; any other value is ContainerFailed.
func ClassifyContainer(upJSON []byte) (Container, error) {
	trimmed := bytes.TrimSpace(upJSON)
	if len(trimmed) == 0 {
		return Container{}, errors.New("devcontainer up: empty result on stdout")
	}
	dec := json.NewDecoder(bytes.NewReader(trimmed))
	var r upResult
	if err := dec.Decode(&r); err != nil {
		return Container{}, fmt.Errorf("devcontainer up: result is not a JSON object: %w", err)
	}
	if dec.InputOffset() != int64(len(trimmed)) {
		return Container{}, errors.New("devcontainer up: trailing data after the result object")
	}
	if r.Outcome == nil || *r.Outcome == "" {
		return Container{}, errors.New("devcontainer up: result has no outcome")
	}
	c := Container{
		Outcome:     ContainerFailed,
		ContainerID: r.ContainerID,
		RemoteUser:  r.RemoteUser,
		Message:     r.Message,
		Description: r.Description,
	}
	if *r.Outcome == "success" {
		if r.ContainerID == "" {
			return Container{}, errors.New("devcontainer up: success without a containerId")
		}
		c.Outcome = ContainerRunning
	}
	return c, nil
}
