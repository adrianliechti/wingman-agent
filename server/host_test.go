package server

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestLocalTransportRejectsDNSRebindingBeforeBootstrap(t *testing.T) {
	for _, host := range []string{"attacker.example:9000", "localhost.attacker.example:9000", "localhost@attacker.example", "127.0.0.1.attacker.example:9000"} {
		handler := LocalHostOnly(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { t.Error("bootstrap reached hostile Host") }))
		req := httptest.NewRequest("GET", "http://"+host+"/api/v2/bootstrap", nil)
		req.Host = host
		req.Header.Set("Origin", "http://"+host)
		req.Header.Set("Sec-Fetch-Site", "same-origin")
		req.Header.Set("X-Forwarded-Host", "localhost")
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, req)
		if response.Code != http.StatusForbidden {
			t.Fatalf("%s: %d", host, response.Code)
		}
	}
	for _, host := range []string{"localhost:9000", "127.0.0.1:9000", "[::1]:9000"} {
		if !isLoopbackHost(host) {
			t.Fatalf("refused %s", host)
		}
	}
}
