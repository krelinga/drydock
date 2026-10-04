package classify

// 5. The devcontainer CLI result (design §6)

// ContainerOutcome is the result of a `devcontainer up`.
type ContainerOutcome uint8

const (
	ContainerRunning ContainerOutcome = iota
	ContainerFailed
)

// Container is the parsed result. Step is what makes a failure actionable:
// design §6 writes an event for every step precisely so the UI can name the
// one that failed, and "failed" alone throws away the only thing that makes a
// rebuild an informed choice.
type Container struct {
	Outcome     ContainerOutcome
	ContainerID string
	RemoteUser  string
	Step        string
}

// ClassifyContainer parses `devcontainer up --json` output. Never scrape
// `docker ps` — the machine-readable result is the contract (§2.5), and the
// fake binary's contract test pins its shape (testing §6.1).
func ClassifyContainer(upJSON []byte) (Container, error) { panic("not implemented: Phase 5") }
