package main

import (
	"bytes"
	"crypto/tls"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"golang.org/x/net/http2"
	"golang.org/x/net/http2/hpack"
)

func NoRedirect(req *http.Request, via []*http.Request) error {
	return http.ErrUseLastResponse
}

type proberesult struct {
	address string
	status  string
}

type proxyConn struct {
	framer       *http2.Framer
	proxyAddress string
	localAddr    string
	mu           sync.RWMutex      // mutex for conns
	conns        map[uint32]string // map streamID to address
	nextStreamID uint32
}

type tunnelConn struct {
	proxyConn *proxyConn
	streamId  uint32
	rxChan    chan []byte
	rxBuff    bytes.Buffer
}

func (t *tunnelConn) Write(b []byte) (n int, err error) {
	err = t.proxyConn.framer.WriteData(t.streamId, false, b)
	n = len(b)
	return
}

func (t *tunnelConn) Read(b []byte) (n int, err error) {
	// If the internal buffer is empty, block and wait for next data
	if t.rxBuff.Len() == 0 {
		data, ok := <-t.rxChan
		if !ok {
			return 0, io.EOF
		}
		t.rxBuff.Write(data)
	}
	// Always read from buffer
	return t.rxBuff.Read(b)

}

func (t *tunnelConn) Close() error {
	log.Fatalln("Proxy connection closed")
	return nil
}

func (t *tunnelConn) LocalAddr() net.Addr {
	s := strings.Split(t.proxyConn.localAddr, ":")
	port, err := strconv.Atoi(s[1])
	if err != nil {
		log.Printf("Error getting remote address: %s", err)
	}
	return &net.TCPAddr{IP: net.IP(s[0]), Port: port}
}

func (t *tunnelConn) RemoteAddr() net.Addr {
	t.proxyConn.mu.RLock()
	address, ok := t.proxyConn.conns[t.streamId]
	if !ok {
		log.Println("Could not get RemoteAdrr")
	}
	t.proxyConn.mu.RUnlock()
	s := strings.Split(address, ":")
	port, err := strconv.Atoi(s[1])
	if err != nil {
		log.Printf("Error getting remote address: %s", err)
	}
	return &net.TCPAddr{IP: net.IP(s[0]), Port: port}
}

var ErrNotImplemented = errors.New("not implemented")

func (p *tunnelConn) SetDeadline(t time.Time) error {
	return ErrNotImplemented
}

func (p *tunnelConn) SetReadDeadline(t time.Time) error {
	return ErrNotImplemented
}

func (p *tunnelConn) SetWriteDeadline(t time.Time) error {
	return ErrNotImplemented
}

func getHTTP2Conn(proxyAddress string) (*http2.Framer, string, net.Conn) {
	url, err := url.Parse(proxyAddress)
	if err != nil {
		log.Fatalln("Error creating proxy url", err)
	}
	var conn net.Conn
	if url.Scheme == "http" {
		conn, err = net.Dial("tcp", url.Host)
		if err != nil {
			log.Fatalln("Error connecting:", err)
		}
	}
	if url.Scheme == "https" {
		conn, err = tls.Dial("tcp", url.Host, &tls.Config{InsecureSkipVerify: true, NextProtos: []string{"h2"}})
		if err != nil {
			log.Fatalln("Error connecting:", err)
		}
	}
	_, err = conn.Write([]byte(http2.ClientPreface))
	if err != nil {
		conn.Close()
		log.Printf("Error writing http2 client preface: %s", err)
	}
	return http2.NewFramer(conn, conn), conn.LocalAddr().String(), conn

}

func (p *proxyConn) sendConnectReq(streamID uint32, targetAddress string) {
	p.mu.Lock()
	p.conns[streamID] = targetAddress
	p.mu.Unlock()
	var hpackBuf bytes.Buffer
	hpackEncoder := hpack.NewEncoder(&hpackBuf)
	connectHeaders := []hpack.HeaderField{
		{Name: ":method", Value: "CONNECT"},
		{Name: ":authority", Value: targetAddress},
	}

	for _, hf := range connectHeaders {
		if err := hpackEncoder.WriteField(hf); err != nil {
			log.Fatalf("HPACK encoding failed for %q: %v", hf.Name, err)
		}
	}

	err := p.framer.WriteHeaders(http2.HeadersFrameParam{
		StreamID:      streamID,
		EndHeaders:    true,
		EndStream:     false,
		BlockFragment: hpackBuf.Bytes(),
	})
	if err != nil {
		slog.Info(fmt.Sprintf("Error initiating CONNECT: %s", err))
	} else {
		slog.Debug(fmt.Sprintf("Sent CONNECT for %s", targetAddress))
	}

}

func (p *proxyConn) handleProxyConn(http2readyChan chan struct{}, resultChan chan proberesult, rxChan chan []byte) {
	defer close(http2readyChan)
	defer close(resultChan)

	p.framer.ReadMetaHeaders = hpack.NewDecoder(4096, nil)
	err := p.framer.WriteSettings(http2.Setting{})
	if err != nil {
		slog.Debug(fmt.Sprintf("Error writing http2 settings frame: %s", err))
	}

	for {
		f, err := p.framer.ReadFrame()
		if err != nil {
			slog.Debug(fmt.Sprintf("Error reading frame: %s", err))
			break
		}

		switch f := f.(type) {
		case *http2.DataFrame:
			slog.Debug(fmt.Sprintf("Received frame: %v\n", f.FrameHeader.Type))
			//log.Debug(fmt.Sprintf("%s", f.Data()))
			data := f.Data()
			if bytes.HasPrefix(data, []byte("no healthy upstream")) {
				slog.Info(fmt.Sprintln("Aborting: unhealthy envoy"))
				os.Exit(1)
			}
			if rxChan != nil {
				rxChan <- data
			}
		case *http2.PingFrame:
			slog.Debug(fmt.Sprintf("Received frame: %v\n", f.FrameHeader.Type))
			//p.framer.WritePing(true)
		case *http2.MetaHeadersFrame:
			slog.Debug(fmt.Sprintf("Received frame: %v\n", f.FrameHeader.Type))
			for _, v := range f.Fields {
				slog.Debug(fmt.Sprintf("\t%s: %s\n", v.Name, v.Value))
				// CONNECT tunnel established
				if v.Name == ":status" && v.Value == "200" {
					p.mu.RLock()
					address, ok := p.conns[f.StreamID]
					p.mu.RUnlock()
					if ok {
						resultChan <- proberesult{address: address, status: "connected"}
					}
				}
			}

		case *http2.GoAwayFrame:
			slog.Info(fmt.Sprintln("Aborting: received GoAwayFrame"))
			os.Exit(1)
		case *http2.SettingsFrame:
			slog.Debug(fmt.Sprintf("Received frame: %v, ACK: %t\n", f.FrameHeader.Type, f.IsAck()))
			if !f.IsAck() {
				err := p.framer.WriteSettingsAck()
				if err != nil {
					log.Fatalf("Error acknowledging settings frame: %s", err)
				}
			} else {
				http2readyChan <- struct{}{}
			}
		case *http2.WindowUpdateFrame:
			slog.Debug(fmt.Sprintf("Received frame: %v\n", f.FrameHeader.Type))
		case *http2.RSTStreamFrame:
			p.mu.RLock()
			address, ok := p.conns[f.StreamID]
			p.mu.RUnlock()
			if ok {
				resultChan <- proberesult{address: address, status: "rejected"}
			}
			slog.Debug(fmt.Sprintf("Received RST_STREAM frame for stream %d with error: %s", f.StreamID, f.ErrCode))
		default:
			slog.Debug(fmt.Sprintf("Transport: unhandled response frame type %T", f))
		}

	}
}

func (p *proxyConn) processPorts(ports []int, resultChan chan proberesult, target string) {
	for _, port := range ports {
		p.sendConnectReq(p.nextStreamID, fmt.Sprintf("%s:%d", target, port))
		p.nextStreamID += 2 // Streams initiated by a client MUST use odd-numbered stream identifiers
	}

	count := len(ports)
	for result := range resultChan {
		if result.status == "connected" {
			slog.Info(fmt.Sprintf("Found open port: %s", result.address))
		}
		if result.status == "rejected" {
			slog.Debug(fmt.Sprintf("Found closed port: %s", result.address))
		}
		count -= 1
		if count == 0 {
			return
		}
	}
}

func (p *proxyConn) getTunnelConn(ports []int, resultChan chan proberesult, target string, rxChan chan []byte) (*tunnelConn, error) {
	target = fmt.Sprintf("%s:%d", target, ports[0])
	p.sendConnectReq(p.nextStreamID, target)
	select {
	case result := <-resultChan:
		if result.status == "connected" && result.address == target {
			return &tunnelConn{proxyConn: p, rxChan: rxChan, streamId: p.nextStreamID}, nil
		}
	case <-time.After(5 * time.Second):
		return nil, errors.New("timeout waiting to establish connect tunnel")
	}
	return nil, errors.New("failed to establish connect tunnel")
}

func exampleTunnelConnUsage(t *tunnelConn, useTls bool) {
	var client net.Conn

	t.proxyConn.mu.RLock()
	target := t.proxyConn.conns[t.streamId]
	t.proxyConn.mu.RUnlock()

	if useTls {
		client = tls.Client(t, &tls.Config{InsecureSkipVerify: true})
		_, err := fmt.Fprintf(client, "GET / HTTP/1.1\r\nHost: %s\r\n\r\n", target) // port is usually omitted for 80/443 but this is still correct
		if err != nil {
			log.Fatalf("Error writing to TLS connection: %s", err)
		}
	} else {
		_, err := fmt.Fprintf(t, "GET / HTTP/1.1\r\nHost: %s\r\n\r\n", target) // port is usually omitted for 80/443 but this is still correct
		if err != nil {
			log.Fatalf("Error writing to plain connection: %s", err)
		}
		client = t
	}

	log.Println("successfully written data")
	b := make([]byte, 512)
	for {
		n, err := client.Read(b)
		if n > 0 {
			fmt.Print(string(b[:n]))
			if strings.HasSuffix(string(b[:n]), "\r\n") {
				break
			}
		}
		if err != nil {
			if err != io.EOF {
				log.Printf("Error reading from proxy connection: %s", err)
			}
			break
		}
	}
}

func main() {
	proxyFlag := flag.String("x", "http://172.17.0.2:10001", "Proxy address to connect to e.g. \"http://172.17.0.2:8080\"")
	targetFlag := flag.String("u", "example.com", "Target host(s) e.g. \"example.com\" or \"1.2.3.4,10.0.1/24\"")
	portsFlag := flag.String("p", "", "Port(s) to scan or connect to, format similar to nmap e.g. 80,443,1000-2000")
	connectModeFlag := flag.Bool("c", false, "POC mode to create a single CONNECT tunnel, issue a HTTP/1 GET request and print the result")
	connectModeTLSFlag := flag.Bool("k", false, "Use TLS in POC mode")
	verboseFlag := flag.Bool("v", false, "Enable verbose logging")
	batchSizeFlag := flag.Int("b", 100, "Batch size for port scanning")
	pauseTimeFlag := flag.String("t", "1s", "Pause time in between batches for port scanning e.g. \"1s\"")
	flag.Parse()

	if *verboseFlag {
		slog.SetLogLoggerLevel(slog.LevelDebug)
	}

	timeout, err := time.ParseDuration(*pauseTimeFlag)
	if err != nil {
		log.Fatalf("Invalid timout: %s", err)
	}

	ports, err := ParsePorts(*portsFlag)
	if err != nil {
		log.Fatalf("Invalid port specification: %s", err)
	}

	targets, err := parseTargets(*targetFlag)
	if err != nil {
		log.Fatalf("Error parsing input: %v", err)
	}

	framer, localAddr, conn := getHTTP2Conn(*proxyFlag)
	defer conn.Close()
	http2readyChan := make(chan struct{})
	resultChan := make(chan proberesult)
	conns := make(map[uint32]string)
	proxyConn := &proxyConn{framer: framer, localAddr: localAddr, proxyAddress: *proxyFlag, conns: conns, nextStreamID: 1}

	var rxChan chan []byte
	if *connectModeFlag {
		if len(ports) > 1 {
			log.Fatalln("Only provide a single port in connect mode")
		}
		if len(targets) > 1 {
			log.Fatalln("Only provide a single target in connect mode")
		}
		rxChan = make(chan []byte)
		go proxyConn.handleProxyConn(http2readyChan, resultChan, rxChan)
	} else {
		go proxyConn.handleProxyConn(http2readyChan, resultChan, nil)
	}

	select {
	case _, ok := <-http2readyChan:
		if !ok {
			log.Println("Failed to connect to proxy")
			return
		}
		slog.Info(fmt.Sprintln("Connected to proxy"))
	case <-time.After(5 * time.Second):
		log.Println("Timeout waiting for proxy connection")
		return
	}

	if *connectModeFlag {
		tunnelConn, err := proxyConn.getTunnelConn(ports, resultChan, targets[0], rxChan)
		if err != nil {
			log.Fatalf("%s", err)
		}
		exampleTunnelConnUsage(tunnelConn, *connectModeTLSFlag)

	} else {
		// batch requests for port scanning
		for _, target := range targets {
			for {
				if len(ports) <= *batchSizeFlag {
					proxyConn.processPorts(ports, resultChan, target)
					break
				} else {
					proxyConn.processPorts(ports[0:*batchSizeFlag], resultChan, target)
					ports = ports[*batchSizeFlag:]
				}
				//give some time to resolve connections
				time.Sleep(timeout)
			}
		}
	}
}
