package prompt

import (
	_ "embed"
)

var (
	//go:embed instructions.txt
	Instructions string

	//go:embed main.txt
	Main string

	//go:embed agent.txt
	Agent string
)
