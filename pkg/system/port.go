package system

import (
	"fmt"
	"net"
)

func FreePort(preferred int) (int, error) {
	ln, err := net.Listen("tcp", fmt.Sprintf("localhost:%d", preferred))
	if err != nil {
		ln, err = net.Listen("tcp", "localhost:0")
		if err != nil {
			return 0, err
		}
	}
	defer ln.Close()
	return ln.Addr().(*net.TCPAddr).Port, nil
}
