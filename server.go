package server

import (
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"
)

func loggingMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Printf("\n%v -- [%v] \"%v %v %v\" TLS: %t", strings.Split(r.RemoteAddr, ":")[0], time.Now().Format("2006-01-02 15:04:05"), r.Method, r.RequestURI, r.Proto, r.TLS != nil)
		//fmt.Printf("TLS (ALPN %s, SNI: %s, HandshakeComplete: %t, PeerCertificates: %s)\n", r.TLS.NegotiatedProtocol, r.TLS.ServerName, r.TLS.HandshakeComplete, r.TLS.PeerCertificates)
		fmt.Printf("\n\tHost: %s\n", r.Host)
		for k, v := range r.Header {
			fmt.Printf("\t%s: %s\n", k, strings.Join(v, " "))
		}
		if _, err := io.Copy(os.Stdout, r.Body); err != nil {
			log.Fatal(err)
		}
		next.ServeHTTP(w, r)

	})
}

func proxyHandler(w http.ResponseWriter, r *http.Request) {
	log.Println("Connection came in")

	if r.Method != http.MethodConnect {
		log.Printf("Unsupported method: %s", r.Method)
		http.Error(w, "405 - Method Not Allowed", http.StatusMethodNotAllowed)
		return
	}
	log.Printf("Accepted CONNECT request for host: %s", r.URL.Host)

	hijacker, ok := w.(http.Hijacker)
	if !ok {
		log.Println("Hijacking not supported")
		http.Error(w, "500 - Hijacking not supported", http.StatusInternalServerError)
		return
	}
	clientConn, _, err := hijacker.Hijack()
	if err != nil {
		log.Printf("Failed to hijack connection: %v", err)
		http.Error(w, "500 - Internal Server Error", http.StatusInternalServerError)
		return
	}
	defer clientConn.Close()

	destConn, err := net.Dial("tcp", r.URL.Host)
	if err != nil {
		log.Printf("Failed to connect to destination: %v", err)
		// We can't write an HTTP error here because the connection is already hijacked.
		return
	}
	defer destConn.Close()

	// 4. Send a 200 OK response to the client to signal that the tunnel is ready
	// The HTTP/2 server will automatically frame this and send it.
	// For HTTP/1.1, we would have to write it manually:
	// clientConn.Write([]byte("HTTP/1.1 200 Connection established\r\n\r\n"))
	// However, with Go's HTTP/2 server, hijacking after the handler returns is the way.
	// We simply return, and the server knows we've taken over.
	log.Printf("Tunnel established to %s", r.URL.Host)

	// 5. Proxy data in both directions
	var wg sync.WaitGroup
	wg.Add(2)

	go func() {
		defer wg.Done()
		io.Copy(destConn, clientConn)
	}()

	go func() {
		defer wg.Done()
		io.Copy(clientConn, destConn)
	}()

	wg.Wait()
	log.Printf("Connection to %s closed.", r.URL.Host)
}

func main_Server() {

	mux := http.NewServeMux()
	// mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
	// 	fmt.Fprintln(w, "test")
	// })

	mux.HandleFunc("/", proxyHandler)

	httpProto := &http.Protocols{}
	httpProto.SetUnencryptedHTTP2(true) //--> only

	server := &http.Server{
		Protocols: httpProto,
		Addr:      ":8888",
		Handler:   loggingMiddleware(mux),
	}
	log.Println("Starting HTTP/2 CONNECT proxy on http://localhost:8888")
	err := server.ListenAndServe()
	if err != nil {
		log.Fatalf("Failed to start server: %v", err)
	}

}
