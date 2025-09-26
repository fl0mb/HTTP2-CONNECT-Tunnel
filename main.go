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

var timeout time.Duration
var ErrNotImplemented = errors.New("not implemented")

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
	// fatal is fine here as this is only used in connect mode which is limited to a single connection
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

func (p *tunnelConn) SetDeadline(t time.Time) error {
	return ErrNotImplemented
}

func (p *tunnelConn) SetReadDeadline(t time.Time) error {
	return ErrNotImplemented
}

func (p *tunnelConn) SetWriteDeadline(t time.Time) error {
	return ErrNotImplemented
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
		// rfc 8441 websocket with http2
		// {Name: ":protocol", Value: "websocket"},
		// {Name: ":scheme", Value: "http"}, // https=wss
		// {Name: ":path", Value: "/ws"},    // http2 always require at least /
		// {Name: "sec-websocket-version", Value: "13"},
		// optional values
		// Testserver wss://echo.websocket.org
		// sec-websocket-protocol = chat, superchat
		// sec-websocket-extensions = permessage-deflate
		// origin
	}

	for _, hf := range connectHeaders {
		if err := hpackEncoder.WriteField(hf); err != nil {
			log.Printf("HPACK encoding failed for %q: %v", hf.Name, err)
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
				p.mu.RLock()
				address, ok := p.conns[f.StreamID]
				p.mu.RUnlock()
				if ok {
					resultChan <- proberesult{address: address, status: "rejected"}
				}
				slog.Info(fmt.Sprintln("Aborting: unhealthy envoy proxy"))
			}
			if rxChan != nil {
				rxChan <- data
			}
		case *http2.PingFrame:
			slog.Debug(fmt.Sprintf("Received frame: %v\n", f.FrameHeader.Type))
			p.framer.WritePing(true, f.Data)
		case *http2.MetaHeadersFrame:
			slog.Debug(fmt.Sprintf("Received frame: %v\n", f.FrameHeader.Type))

			var status200, contentType bool

			for _, field := range f.Fields {
				slog.Debug(fmt.Sprintf("\t%s: %s\n", field.Name, field.Value))

				// CONNECT tunnel established
				if field.Name == ":status" && field.Value == "200" {
					status200 = true
				}

				// Filter false positives that replay with normal data to every request method
				if field.Name == "content-type" || field.Name == "content-length" {
					contentType = true
				}
			}

			if status200 && !contentType {
				p.mu.RLock()
				address, ok := p.conns[f.StreamID]
				p.mu.RUnlock()
				if ok {
					resultChan <- proberesult{address: address, status: "connected"}
				}
			}

		case *http2.GoAwayFrame:
			p.mu.RLock()
			address, ok := p.conns[f.StreamID]
			p.mu.RUnlock()
			if ok {
				resultChan <- proberesult{address: address, status: "rejected"}
			}
			slog.Info(fmt.Sprintf("Received GoAwayFrame, closing connection to %s", p.proxyAddress))
		case *http2.SettingsFrame:
			slog.Debug(fmt.Sprintf("Received frame: %v, ACK: %t\n", f.FrameHeader.Type, f.IsAck()))
			if !f.IsAck() {
				err := p.framer.WriteSettingsAck()
				if err != nil {
					log.Printf("Error acknowledging settings frame: %s for proxy %s", err, p.proxyAddress)
					// Not ideal
					// return
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
			slog.Info(fmt.Sprintf("Found accessible target: %s for proxy %s", result.address, p.proxyAddress))
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
	case <-time.After(timeout):
		return nil, errors.New("timeout waiting to establish connect tunnel")
	}
	return nil, errors.New("failed to establish connect tunnel")
}

// batch requests for port scanning
func (p *proxyConn) batchProcess(targets []string, ports []int, resultChan chan proberesult, batchsize int, waitTime time.Duration) {
	for _, target := range targets {
		for {
			if len(ports) <= batchsize {
				p.processPorts(ports, resultChan, target)
				break
			} else {
				p.processPorts(ports[0:batchsize], resultChan, target)
				ports = ports[batchsize:]
			}
			//give some time to resolve connections
			time.Sleep(waitTime)
		}
	}
}

func getHTTP2Conn(proxyAddress string) (*http2.Framer, string, net.Conn, error) {
	url, err := url.Parse(proxyAddress)
	if err != nil {
		return nil, "", nil, fmt.Errorf("error creating proxy url %s", err)
	}
	var conn net.Conn
	if url.Scheme == "http" {
		conn, err = net.DialTimeout("tcp", url.Host, timeout)
		if err != nil {
			return nil, "", nil, fmt.Errorf("error connecting: %s", err)
		}
	}
	if url.Scheme == "https" {
		conn, err = tls.Dial("tcp", url.Host, &tls.Config{InsecureSkipVerify: true, NextProtos: []string{"h2"}})
		if err != nil {
			return nil, "", nil, fmt.Errorf("error connecting: %s", err)
		}
		conn.SetDeadline(time.Now().Add(timeout))
	}
	_, err = conn.Write([]byte(http2.ClientPreface))
	if err != nil {
		conn.Close()
		log.Printf("Error writing http2 client preface: %s", err)
	}
	return http2.NewFramer(conn, conn), conn.LocalAddr().String(), conn, nil

}

func NewProxyConn(proxyAddress string, connectMode bool) (*proxyConn, chan proberesult, chan []byte, error) {
	framer, localAddr, conn, err := getHTTP2Conn(proxyAddress)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("error connecting to proxy %s: %s", proxyAddress, err)
	}
	http2readyChan := make(chan struct{})
	resultChan := make(chan proberesult)
	conns := make(map[uint32]string)
	proxyConn := &proxyConn{framer: framer, localAddr: localAddr, proxyAddress: proxyAddress, conns: conns, nextStreamID: 1}

	var rxChan chan []byte
	if connectMode {
		rxChan = make(chan []byte)
		go proxyConn.handleProxyConn(http2readyChan, resultChan, rxChan)
	} else {
		go proxyConn.handleProxyConn(http2readyChan, resultChan, nil)
	}

	select {
	case _, ok := <-http2readyChan:
		if !ok {
			conn.Close() // Ideally it should also be possible to call close for Error conditions outside of this function
			return nil, nil, nil, fmt.Errorf("failed to connect to proxy %s", proxyAddress)
		}
		slog.Info(fmt.Sprintf("Connected to proxy %s", proxyAddress))
	case <-time.After(timeout):
		conn.Close()
		return nil, nil, nil, fmt.Errorf("timeout waiting for proxy connection to %s", proxyAddress)
	}

	return proxyConn, resultChan, rxChan, nil
}

func localTCPForwarder(tunnel *tunnelConn, l net.Listener) {
	for {
		conn, err := l.Accept()
		if err != nil {
			log.Fatal(err)
		}
		go forward(tunnel, conn)
		go forward(conn, tunnel)
	}
}

func forward(dst io.Writer, src io.Reader) {
	_, err := io.Copy(dst, src)
	if err != nil {
		log.Fatalf("Error forwarding: %s", err)
		return
	}

}

func worker(jobs <-chan func(), wg *sync.WaitGroup) {
	for job := range jobs {
		job()
	}
	defer wg.Done()
}

func NoRedirect(req *http.Request, via []*http.Request) error {
	return http.ErrUseLastResponse
}

// func exampleTunnelConnUsage(t *tunnelConn, useTls bool) {
// 	var client net.Conn

// 	t.proxyConn.mu.RLock()
// 	target := t.proxyConn.conns[t.streamId]
// 	t.proxyConn.mu.RUnlock()

// 	if useTls {
// 		client = tls.Client(t, &tls.Config{InsecureSkipVerify: true})
// 		_, err := fmt.Fprintf(client, "GET / HTTP/1.1\r\nHost: %s\r\n\r\n", target) // port is usually omitted for 80/443 but this is still correct
// 		if err != nil {
// 			log.Fatalf("Error writing to TLS connection: %s", err)
// 		}
// 	} else {
// 		_, err := fmt.Fprintf(t, "GET / HTTP/1.1\r\nHost: %s\r\n\r\n", target) // port is usually omitted for 80/443 but this is still correct
// 		if err != nil {
// 			log.Fatalf("Error writing to plain connection: %s", err)
// 		}
// 		client = t
// 	}

// 	log.Println("successfully written data")
// 	b := make([]byte, 512)
// 	for {
// 		n, err := client.Read(b)
// 		if n > 0 {
// 			fmt.Print(string(b[:n]))
// 			if strings.HasSuffix(string(b[:n]), "\r\n") {
// 				break
// 			}
// 		}
// 		if err != nil {
// 			if err != io.EOF {
// 				log.Printf("Error reading from proxy connection: %s", err)
// 			}
// 			break
// 		}
// 	}
// }

func main() {
	targetFlag := flag.String("u", "", "Target host(s) e.g. \"example.com\" or \"1.2.3.4,10.0.1/24\"")
	proxyFlag := flag.String("x", "", "Proxy address to connect to e.g. \"http://172.17.0.2:8080\"")
	proxyFileFlag := flag.String("proxy-file", "", "File containing line separated proxy addresses to connect to e.g. \"http://172.17.0.2:8080\"")
	portsFlag := flag.String("p", "", "Port(s) to scan or connect to, format similar to nmap e.g. 80,443,1000-2000")
	connectModeFlag := flag.Bool("c", false, "enable connection mode, where a local listener forwards traffic into and out of the CONNECT tunnel")
	localListenerFlag := flag.String("l", "127.0.0.1:8080", "Address of local listener for connect mode")
	verboseFlag := flag.Bool("v", false, "Enable verbose logging")
	batchSizeFlag := flag.Int("b", 100, "Batch size for port scanning")
	waitTimeFlag := flag.String("w", "1s", "Wait time in between batches for port scanning e.g. \"1s\"")
	timeoutFlag := flag.String("t", "5s", "Timeout for proxy connection e.g. \"1s\"")
	threadsFlag := flag.Int("threads", 20, "Number of threads e.g. parallel proxy connections that are being worked on")
	flag.Parse()

	if *proxyFileFlag != "" && *proxyFlag != "" {
		log.Println("Specify proxy addresses either in the cli (-x) or as a file (--proxy-file)")
		flag.Usage()
		os.Exit(1)
	}

	log.SetOutput(os.Stdout)
	if *verboseFlag {
		slog.SetLogLoggerLevel(slog.LevelDebug)
	}

	waitTime, err := time.ParseDuration(*waitTimeFlag)
	if err != nil {
		log.Printf("Invalid wait time: %s", err)
		flag.Usage()
		os.Exit(1)
	}

	timeout, err = time.ParseDuration(*timeoutFlag)
	if err != nil {
		log.Printf("Invalid timout: %s", err)
		flag.Usage()
		os.Exit(1)
	}

	ports, err := ParsePorts(*portsFlag)
	if err != nil {
		log.Printf("Invalid port specification: %s", err)
		flag.Usage()
		os.Exit(1)
	}

	var targets []string
	if *targetFlag != "" {
		targets, err = parseAddresses(*targetFlag)
		if err != nil {
			log.Printf("Error parsing input: %v", err)
			flag.Usage()
			os.Exit(1)
		}
	} else {
		log.Println("No targets specified")
		flag.Usage()
		os.Exit(1)
	}

	var proxies []string
	switch {
	case *proxyFlag != "" && strings.Contains(*proxyFlag, ","):
		proxies = strings.Split(*proxyFlag, ",")
	case *proxyFlag != "":
		proxies = append(proxies, *proxyFlag)
	case *proxyFileFlag != "":
		proxies = append(proxies, readProxies(*proxyFileFlag)...)
	default:
		log.Println("No proxies specified")
		flag.Usage()
		os.Exit(1)
	}

	if *connectModeFlag {
		if len(ports) > 1 {
			log.Println("Only provide a single port in connect mode")
			flag.Usage()
			os.Exit(1)
		}
		if len(targets) > 1 {
			log.Println("Only provide a single target in connect mode")
			flag.Usage()
			os.Exit(1)
		}
		if len(proxies) > 1 {
			log.Println("Only provide a single proxy in connect mode")
			flag.Usage()
			os.Exit(1)
		}

	}

	if *connectModeFlag {
		l, err := net.Listen("tcp", *localListenerFlag)
		if err != nil {
			log.Fatalf("Failed to create local listener: %s", err)
		}
		defer l.Close()

		proxyConn, resultChan, rxChan, err := NewProxyConn(proxies[0], *connectModeFlag)
		if err != nil {
			log.Fatalf("%s", err)
		}
		tunnelConn, err := proxyConn.getTunnelConn(ports, resultChan, targets[0], rxChan)
		if err != nil {
			log.Fatalf("%s", err)
		}
		//exampleTunnelConnUsage(tunnelConn, *connectModeTLSFlag)

		localTCPForwarder(tunnelConn, l)
		os.Exit(0)
	}

	if len(proxies) == 1 {
		proxyConn, resultChan, _, err := NewProxyConn(proxies[0], *connectModeFlag)
		if err != nil {
			log.Fatalf("%s", err)
		}
		proxyConn.batchProcess(targets, ports, resultChan, *batchSizeFlag, waitTime)
	} else {
		var wgs int
		if len(proxies) < *threadsFlag {
			wgs = len(proxies)
		} else {
			wgs = *threadsFlag
		}
		var wg sync.WaitGroup
		jobs := make(chan func())
		for w := 1; w <= wgs; w++ {
			wg.Add(1)
			go worker(jobs, &wg)
		}
		for _, proxy := range proxies {
			proxyConn, resultChan, _, err := NewProxyConn(proxy, *connectModeFlag)
			if err != nil {
				log.Println(err)
				continue
			}
			jobs <- func() {
				proxyConn.batchProcess(targets, ports, resultChan, *batchSizeFlag, waitTime)
			}
		}
		close(jobs) //By now all jobs are scheduled and can thus be closed
		wg.Wait()   // wait until workers are done so that no more matchingEntries can be created
	}

}
