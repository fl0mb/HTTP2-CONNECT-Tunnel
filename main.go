package main

import (
	"bytes"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"strconv"
	"strings"
	"time"

	"golang.org/x/net/http2"
	"golang.org/x/net/http2/hpack"
)

func NoRedirect(req *http.Request, via []*http.Request) error {
	return http.ErrUseLastResponse
}

type proxyConn struct {
	//rxChan       chan []byte
	//rxBuff       bytes.Buffer
	framer       *http2.Framer
	proxyAddress string
	localAddr    string
	conns        map[uint32]string // map streamID to address
}

type tunnelConn struct {
	streamId     uint32
	rxChan       chan []byte
	rxBuff       bytes.Buffer
	framer       *http2.Framer
	proxyAddress string
	localAddr    string
}

func (t *tunnelConn) Write(b []byte) (n int, err error) {
	err = t.framer.WriteData(t.streamId, false, b)
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
	s := strings.Split(t.localAddr, ":")
	port, err := strconv.Atoi(s[1])
	if err != nil {
		log.Printf("Error getting remote address: %s", err)
	}
	return &net.TCPAddr{IP: net.IP(s[0]), Port: port}
}

func (t *tunnelConn) RemoteAddr() net.Addr {
	s := strings.Split(t.proxyAddress, ":")
	port, err := strconv.Atoi(s[1])
	if err != nil {
		log.Printf("Error getting remote address: %s", err)
	}
	return &net.TCPAddr{IP: net.IP(s[0]), Port: port}
}

// func (p *proxyConn) Write(b []byte) (n int, err error) {
// 	err = p.framer.WriteData(1, false, b)
// 	n = len(b)
// 	return
// }

// func (p *proxyConn) Read(b []byte) (n int, err error) {
// 	// If our internal buffer is empty, we need to block and wait for next data
// 	if p.rxBuff.Len() == 0 {
// 		data, ok := <-p.rxChan
// 		if !ok {
// 			return 0, io.EOF
// 		}
// 		p.rxBuff.Write(data)
// 	}
// 	// Always read from buffer
// 	return p.rxBuff.Read(b)

// }

// func (p *proxyConn) Close() error {
// 	log.Fatalln("Proxy connection closed")
// 	return nil
// }

// func (p *proxyConn) LocalAddr() net.Addr {
// 	s := strings.Split(p.localAddr, ":")
// 	port, err := strconv.Atoi(s[1])
// 	if err != nil {
// 		log.Printf("Error getting remote address: %s", err)
// 	}
// 	return &net.TCPAddr{IP: net.IP(s[0]), Port: port}
// }

// func (p *proxyConn) RemoteAddr() net.Addr {
// 	s := strings.Split(p.proxyAddress, ":")
// 	port, err := strconv.Atoi(s[1])
// 	if err != nil {
// 		log.Printf("Error getting remote address: %s", err)
// 	}
// 	return &net.TCPAddr{IP: net.IP(s[0]), Port: port}
// }

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

// func (p *proxyConn) SetDeadline(t time.Time) error {
// 	return ErrNotImplemented
// }

// func (p *proxyConn) SetReadDeadline(t time.Time) error {
// 	return ErrNotImplemented
// }

// func (p *proxyConn) SetWriteDeadline(t time.Time) error {
// 	return ErrNotImplemented
// }

func getHTTP2Conn(proxyAddress string) (*http2.Framer, string, net.Conn) {
	conn, err := net.Dial("tcp", proxyAddress)
	if err != nil {
		log.Fatalln("Error connecting:", err)
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
	p.conns[streamID] = targetAddress
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
		log.Printf("Error initiating CONNECT: %s", err)
	} else {
		log.Printf("Sent CONNECT for %s", targetAddress)
	}

}

func (p *proxyConn) connectProxy(http2readyChan chan struct{}, resultChan chan string) { //rxChan chan<- []byte
	//defer close(rxChan) // Ensure the channel is closed on exit to signal EOF.
	defer close(http2readyChan)
	defer close(resultChan)

	p.framer.ReadMetaHeaders = hpack.NewDecoder(4096, nil)
	err := p.framer.WriteSettings(http2.Setting{})
	if err != nil {
		log.Printf("Error writing http2 settings frame: %s", err)
	}

	//loop:
	for {
		f, err := p.framer.ReadFrame()
		if err != nil {
			log.Printf("Error reading frame: %s", err)
			break
		}

		switch f := f.(type) {
		case *http2.DataFrame:
			log.Printf("Received frame: %v\n", f.FrameHeader.Type)
			log.Printf("%s", f.Data())
			//rxChan <- f.Data()
		case *http2.PingFrame:
			log.Printf("Received frame: %v\n", f.FrameHeader.Type)
			//p.framer.WritePing(true)
		case *http2.MetaHeadersFrame:
			log.Printf("Received frame: %v\n", f.FrameHeader.Type)
			// for _, v := range f.Fields {
			// 	//break loop
			// 	if v.Name == ":status" && v.Value != "200" {
			// 		log.Printf("Aborting, received :status %s", v.Value)
			// 		close(readyChan)
			// 		return
			// 	}
			// }

			for _, v := range f.Fields {
				log.Printf("\t%s: %s\n", v.Name, v.Value)
				// CONNECT tunnel established
				if v.Name == ":status" && v.Value == "200" {
					address, ok := p.conns[f.StreamID]
					if ok {
						resultChan <- address
					}
				}
			}

		case *http2.GoAwayFrame:
			log.Println("Aborting: received GoAwayFrame")
			return
		case *http2.SettingsFrame:
			log.Printf("Received frame: %v, ACK: %t\n", f.FrameHeader.Type, f.IsAck())
			if !f.IsAck() {
				err := p.framer.WriteSettingsAck()
				if err != nil {
					log.Fatalf("Error acknowledging settings frame: %s", err)
				}
			} else {
				http2readyChan <- struct{}{}
			}
		case *http2.WindowUpdateFrame:
			log.Printf("Received frame: %v\n", f.FrameHeader.Type)
		case *http2.RSTStreamFrame:
			// handle!
			log.Printf("Received RST_STREAM frame for stream %d with error: %s", f.StreamID, f.ErrCode)
		default:
			log.Printf("Transport: unhandled response frame type %T", f)
		}

	}
}

func main() {
	proxyFlag := flag.String("x", "172.17.0.2:10001", "proxy address e.g. \"172.17.0.2:10001\"")
	targetFlag := flag.String("t", "google.de", "target address e.g. \"example.com\"")
	//modeFlag := flag.String("p", "plain", "mode: use \"plain\" for plaintext or \"tls\" to create encrypted connection to target")
	flag.Parse()
	// abfangen dass es nicht zu viele auf einmal werden!
	ports := []int{10000, 10001, 10002}

	framer, localAddr, conn := getHTTP2Conn(*proxyFlag)
	defer conn.Close()
	//rxChan := make(chan []byte) // make buffered just in case?
	http2readyChan := make(chan struct{})
	resultChan := make(chan string)
	conns := make(map[uint32]string)
	proxyConn := &proxyConn{framer: framer, localAddr: localAddr, proxyAddress: *proxyFlag, conns: conns} //rxChan: rxChan,
	go proxyConn.connectProxy(http2readyChan, resultChan)

	// wait until ready
	select {
	case _, ok := <-http2readyChan:
		if !ok {
			log.Println("Failed to connect to proxy") //not ideal!
			return
		}
	case <-time.After(5 * time.Second):
		log.Println("Timeout waiting for proxy connection")
		return
	}

	//initiate connections
	var streamID uint32 = 1
	for _, port := range ports {
		proxyConn.connect(streamID, fmt.Sprintf("%s:%d", *targetFlag, port))
		streamID += 2 // Streams initiated by a client MUST use odd-numbered stream identifiers
	}

	//endet hier noch nicht von allein!
	for address := range resultChan {
		log.Printf("Found open port: %s", address)
	}

	// readin part here
	// var client net.Conn
	// switch {
	// case *modeFlag == "plain":
	// 	_, err := fmt.Fprintf(proxyConn, "GET / HTTP/1.1\r\nHost: %s\r\n\r\n", *targetFlag) // port is usually omitted for 80/443 but this is still correct
	// 	if err != nil {
	// 		log.Fatalf("Error writing to plain connection: %s", err)
	// 	}
	// 	client = proxyConn
	// case *modeFlag == "tls":
	// 	client = tls.Client(proxyConn, &tls.Config{InsecureSkipVerify: true})
	// 	_, err := fmt.Fprintf(client, "GET / HTTP/1.1\r\nHost: %s\r\n\r\n", *targetFlag) // port is usually omitted for 80/443 but this is still correct
	// 	if err != nil {
	// 		log.Fatalf("Error writing to TLS connection: %s", err)
	// 	}
	// }
	// log.Println("successfully written data")
	// b := make([]byte, 512)
	// for {
	// 	n, err := client.Read(b)
	// 	if n > 0 {
	// 		fmt.Print(string(b[:n])) // IMPORTANT: Only process the sub-slice b[:n] which contains the valid data.
	// 		if strings.HasSuffix(string(b[:n]), "\r\n") {
	// 			break

	// 		}
	// 	}
	// 	if err != nil {
	// 		if err != io.EOF {
	// 			log.Printf("Error reading from proxy connection: %s", err)
	// 		}
	// 		break
	// 	}
	// }

	//strg+c handler
	// support not just h2c but tls too - take it from proxy specification e.g. https://proxy.org

}
