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
	"sort"
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

// ParsePorts takes a string of ports and returns a sorted slice of unique integers.
// It supports formats like "80", "80,443", "80-90", "22,80,443,1000-2000".
func ParsePorts(portStr string) ([]int, error) {
	ports := make(map[int]struct{})
	if portStr == "" {
		return []int{}, errors.New("no port specified")
	}
	parts := strings.Split(portStr, ",")

	for _, part := range parts {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}

		// Check if the part is a range (e.g., "80-90").
		if strings.Contains(part, "-") {
			rangeParts := strings.Split(part, "-")
			if len(rangeParts) != 2 {
				return nil, fmt.Errorf("invalid port range: %s", part)
			}

			startStr := strings.TrimSpace(rangeParts[0])
			endStr := strings.TrimSpace(rangeParts[1])

			start, err := strconv.Atoi(startStr)
			if err != nil {
				return nil, fmt.Errorf("invalid start port in range '%s': %w", part, err)
			}

			end, err := strconv.Atoi(endStr)
			if err != nil {
				return nil, fmt.Errorf("invalid end port in range '%s': %w", part, err)
			}

			if start > end {
				return nil, fmt.Errorf("invalid range: start port %d is greater than end port %d", start, end)
			}

			if start < 1 || end > 65535 {
				return nil, errors.New("port numbers must be between 1 and 65535")
			}

			for i := start; i <= end; i++ {
				ports[i] = struct{}{}
			}
		} else {
			port, err := strconv.Atoi(part)
			if err != nil {
				return nil, fmt.Errorf("invalid port number: %s", part)
			}

			if port < 1 || port > 65535 {
				return nil, fmt.Errorf("port number %d is out of valid range (1-65535)", port)
			}
			ports[port] = struct{}{}
		}
	}

	result := make([]int, 0, len(ports))
	for port := range ports {
		result = append(result, port)
	}

	sort.Ints(result)
	return result, nil
}

type proberesult struct {
	address string
	status  string
}

type proxyConn struct {
	//rxChan       chan []byte
	//rxBuff       bytes.Buffer
	framer       *http2.Framer
	proxyAddress string
	localAddr    string
	mu           sync.RWMutex      //mutex for conns
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
	// If our internal buffer is empty, we need to block and wait for next data
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
	s := strings.Split(t.proxyConn.proxyAddress, ":")
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
		conn, err = tls.Dial("tcp", url.Host, &tls.Config{InsecureSkipVerify: true})
		//conn, err = net.Dial("tcp", url.Host)
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

// http2 works now initiate CONNECT tunnel
func (p *proxyConn) connect(streamID uint32, targetAddress string) {
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

func (p *proxyConn) connectProxy(http2readyChan chan struct{}, resultChan chan proberesult, rxChan chan []byte) { //rxChan chan<- []byte
	defer close(rxChan)
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
		p.connect(p.nextStreamID, fmt.Sprintf("%s:%d", target, port))
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
	p.connect(p.nextStreamID, target)
	for result := range resultChan {
		if result.status == "connected" && result.address == target {
			return &tunnelConn{proxyConn: p, rxChan: rxChan, streamId: p.nextStreamID}, nil
		}
	}

	return nil, errors.New("failed to establish connect tunnel")
}

func exampleTunnelConnUsage(t *tunnelConn) {
	var client net.Conn

	t.proxyConn.mu.RLock()
	target := t.proxyConn.conns[t.streamId]
	t.proxyConn.mu.RUnlock()

	// _, err := fmt.Fprintf(t, "GET / HTTP/1.1\r\nHost: %s\r\n\r\n", target) // port is usually omitted for 80/443 but this is still correct
	// if err != nil {
	// 	log.Fatalf("Error writing to plain connection: %s", err)
	// }
	// client = t

	client = tls.Client(t, &tls.Config{InsecureSkipVerify: true})
	_, err := fmt.Fprintf(client, "GET / HTTP/1.1\r\nHost: %s\r\n\r\n", target) // port is usually omitted for 80/443 but this is still correct
	if err != nil {
		log.Fatalf("Error writing to TLS connection: %s", err)
	}

	log.Println("successfully written data")
	b := make([]byte, 512)
	for {
		n, err := client.Read(b)
		if n > 0 {
			fmt.Print(string(b[:n])) // IMPORTANT: Only process the sub-slice b[:n] which contains the valid data.
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
	proxyFlag := flag.String("x", "http://172.17.0.2:10001", "proxy address e.g. \"http://172.17.0.2:10001\"")
	targetFlag := flag.String("t", "google.de", "target address e.g. \"example.com\"")
	portsFlag := flag.String("p", "", "port(s) to scan or connect to, format similar to nmap e.g. 80,443,1000-2000")
	connectModeFlag := flag.Bool("c", false, "create a single CONNECT tunnel")
	verbose := flag.Bool("v", false, "Enable verbose logging")

	flag.Parse()
	if *verbose {
		slog.SetLogLoggerLevel(slog.LevelDebug)
	}

	framer, localAddr, conn := getHTTP2Conn(*proxyFlag)
	defer conn.Close()
	http2readyChan := make(chan struct{})
	resultChan := make(chan proberesult)
	conns := make(map[uint32]string)
	proxyConn := &proxyConn{framer: framer, localAddr: localAddr, proxyAddress: *proxyFlag, conns: conns, nextStreamID: 1}

	ports, err := ParsePorts(*portsFlag)
	if err != nil {
		log.Fatalf("Invalid port specification: %s", err)
	}
	var rxChan chan []byte
	if *connectModeFlag {
		if len(ports) > 1 {
			log.Fatalln("Only provide a sinlge port in connect mode")
		}
		rxChan = make(chan []byte) // make buffered just in case?
		go proxyConn.connectProxy(http2readyChan, resultChan, rxChan)
	} else {
		go proxyConn.connectProxy(http2readyChan, resultChan, nil)
	}

	select {
	case _, ok := <-http2readyChan:
		if !ok {
			log.Println("Failed to connect to proxy")
			return
		}
	case <-time.After(5 * time.Second):
		log.Println("Timeout waiting for proxy connection")
		return
	}
	slog.Info(fmt.Sprintln("Connected to proxy"))

	if *connectModeFlag {
		tunnelConn, err := proxyConn.getTunnelConn(ports, resultChan, *targetFlag, rxChan)
		if err != nil {
			log.Fatalf("%s", err)
		}
		exampleTunnelConnUsage(tunnelConn)

	} else {
		// batch requests for port scanning
		for {
			if len(ports) <= 100 {
				proxyConn.processPorts(ports, resultChan, *targetFlag)
				return
			} else {
				proxyConn.processPorts(ports[0:100], resultChan, *targetFlag)
				ports = ports[100:]
			}
			//give some time to resolve connections
			time.Sleep(time.Second)
		}

	}

	// strg+c handler
	// specify how to use connect mode example e.g. tls or plain
	// support not just h2c but tls too - take it from proxy specification e.g. https://proxy.org

}
