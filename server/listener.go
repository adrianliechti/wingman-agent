package server

import (
	"context"
	"net"
	"net/http"
	"strconv"
)

// serveLocal serves until a listener fails or one of the contexts ends.
func serveLocal(handler http.Handler, listeners []net.Listener, done ...context.Context) error {
	srv := &http.Server{Handler: handler}
	for _, listener := range listeners {
		defer listener.Close()
	}
	for _, ctx := range done {
		stop := context.AfterFunc(ctx, func() { _ = srv.Close() })
		defer stop()
	}
	errs := make(chan error, len(listeners))
	for _, listener := range listeners {
		go func() { errs <- srv.Serve(listener) }()
	}
	for range listeners {
		if err := <-errs; err != nil && err != http.ErrServerClosed {
			return err
		}
	}
	return nil
}

func listenLocal(port, preferred int) ([]net.Listener, error) {
	if port == 0 {
		if listeners, err := listenLoopback(preferred); err == nil {
			return listeners, nil
		}
	}
	return listenLoopback(port)
}

// Clients resolve "localhost" to either loopback address (Windows tries ::1 first), so serve both.
func listenLoopback(port int) ([]net.Listener, error) {
	var hosts []string
	for _, host := range []string{"127.0.0.1", "::1"} {
		if probe, err := net.Listen("tcp", net.JoinHostPort(host, "0")); err == nil {
			probe.Close()
			hosts = append(hosts, host)
		}
	}
	if len(hosts) == 0 {
		hosts = []string{"localhost"}
	}
	for attempt := 0; ; attempt++ {
		listeners, err := listenHosts(hosts, port)
		// A random port can be taken on the other address; choose again.
		if err == nil || port != 0 || attempt == 7 {
			return listeners, err
		}
	}
}

func listenHosts(hosts []string, port int) ([]net.Listener, error) {
	var listeners []net.Listener
	for _, host := range hosts {
		listener, err := net.Listen("tcp", net.JoinHostPort(host, strconv.Itoa(port)))
		if err != nil {
			for _, open := range listeners {
				open.Close()
			}
			return nil, err
		}
		listeners = append(listeners, listener)
		port = listener.Addr().(*net.TCPAddr).Port
	}
	return listeners, nil
}
