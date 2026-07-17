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

type ResizeEvent struct {
	Width  int
	Height int
}
