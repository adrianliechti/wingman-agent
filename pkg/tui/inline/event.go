package inline

type Key int

const (
	KeyRune Key = iota
	KeyEnter
	KeyTab
	KeyBacktab
	KeyEsc
	KeyBackspace
	KeyDelete
	KeyUp
	KeyDown
	KeyLeft
	KeyRight
	KeyHome
	KeyEnd
	KeyPgUp
	KeyPgDn
	KeyCtrl
)

type Event any

type KeyEvent struct {
	Key  Key
	Rune rune
	Alt  bool
}

type PasteEvent struct {
	Text string
}

type MouseEvent struct {
	WheelDelta int
	X          int
	Y          int
}

type ResizeEvent struct {
	Width  int
	Height int
}
