package code

import "github.com/adrianliechti/wingman-agent/pkg/tui/theme"

type Modal string

const (
	ModalNone        Modal = ""
	ModalPicker      Modal = "picker"
	ModalFilePicker  Modal = "file-picker"
	ModalDiff        Modal = "diff"
	ModalDiagnostics Modal = "diagnostics"
)

type AppPhase int

const (
	PhaseIdle AppPhase = iota
	PhasePreparing
	PhaseThinking
	PhaseStreaming
	PhaseToolRunning
)

type PhaseConfig struct {
	Message string
	Color   string
}

func GetPhaseConfig(phase AppPhase) PhaseConfig {
	t := theme.Default

	switch phase {
	case PhasePreparing:
		return PhaseConfig{
			Message: "Preparing...",
			Color:   t.BrBlack.String(),
		}
	case PhaseThinking, PhaseStreaming:
		return PhaseConfig{
			Message: "Thinking...",
			Color:   t.Cyan.String(),
		}
	case PhaseToolRunning:
		return PhaseConfig{
			Message: "Running...",
			Color:   t.Yellow.String(),
		}
	default:
		return PhaseConfig{
			Message: "",
			Color:   t.BrBlack.String(),
		}
	}
}

type Mode int

const (
	ModeAgent Mode = iota
	ModePlan
)
