package code

type AppPhase int

const (
	PhaseIdle AppPhase = iota
	PhasePreparing
	PhaseThinking
	PhaseStreaming
	PhaseToolRunning
)

type Mode int

const (
	ModeAgent Mode = iota
	ModePlan
)
