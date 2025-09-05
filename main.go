package main

import (
	"bytes"
	"context"
	"crypto/tls"
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
	rxChan       chan []byte
	rxBuff       bytes.Buffer
	framer       *http2.Framer
	proxyAddress string
	localAddr    string
}

func (p *proxyConn) Write(b []byte) (n int, err error) {
	err = p.framer.WriteData(1, false, b)
	n = len(b)
	return
}

func (p *proxyConn) Read(b []byte) (n int, err error) {
	// If our internal buffer is empty, we need to block and wait for next data
	if p.rxBuff.Len() == 0 {
		data, ok := <-p.rxChan
		if !ok {
			return 0, io.EOF
		}
		p.rxBuff.Write(data)
	}
	// Always read from buffer
	return p.rxBuff.Read(b)

}

func (p *proxyConn) Close() error {
	log.Fatalln("Proxy connection closed")
	return nil
}

func (p *proxyConn) LocalAddr() net.Addr {
	s := strings.Split(p.localAddr, ":")
	port, err := strconv.Atoi(s[1])
	if err != nil {
		log.Printf("Error getting remote address: %s", err)
	}
	return &net.TCPAddr{IP: net.IP(s[0]), Port: port}
}

func (p *proxyConn) RemoteAddr() net.Addr {
	s := strings.Split(p.proxyAddress, ":")
	port, err := strconv.Atoi(s[1])
	if err != nil {
		log.Printf("Error getting remote address: %s", err)
	}
	return &net.TCPAddr{IP: net.IP(s[0]), Port: port}
}

var ErrNotImplemented = errors.New("not implemented")

func (p *proxyConn) SetDeadline(t time.Time) error {
	return ErrNotImplemented
}

func (p *proxyConn) SetReadDeadline(t time.Time) error {
	return ErrNotImplemented
}

func (p *proxyConn) SetWriteDeadline(t time.Time) error {
	return ErrNotImplemented
}

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

func getProxyConn(framer *http2.Framer, targetAddress string, rxChan chan<- []byte, readyChan chan struct{}, ctx context.Context) {
	defer close(rxChan) // Ensure the channel is closed on exit to signal EOF.

	framer.ReadMetaHeaders = hpack.NewDecoder(4096, nil)
	err := framer.WriteSettings(http2.Setting{})
	if err != nil {
		log.Printf("Error writing http2 settings frame: %s", err)
	}

	var readyToInitiate bool      // true when http2 connection was successful
	var proxyConnAcknowleged bool // true when CONNECT was confirmed by a headers frame with status 200

	//loop:
	for {
		if ctx.Err() == nil {
			f, err := framer.ReadFrame()
			if err != nil {
				//log.Fatalf("Error reading frame: %s", err)
				log.Printf("Error reading frame: %s", err)
				break
			}

			switch f := f.(type) {
			case *http2.DataFrame:
				log.Printf("Received frame: %v\n", f.FrameHeader.Type)
				//log.Printf("%s", f.Data())
				rxChan <- f.Data()
			case *http2.PingFrame:
				log.Printf("Received frame: %v\n", f.FrameHeader.Type)
				//framer.WritePing(true)
			case *http2.MetaHeadersFrame:
				log.Printf("Received frame: %v\n", f.FrameHeader.Type)
				for _, v := range f.Fields {
					if v.Name == ":status" && v.Value != "200" {
						log.Printf("Aborting, received :status %s", v.Value)
						close(readyChan)
						//break loop
						return
					}
				}
				if readyToInitiate && !proxyConnAcknowleged && f.FrameHeader.StreamID == 1 {
					for _, v := range f.Fields {
						log.Printf("\t%s: %s\n", v.Name, v.Value)
						if v.Name == ":status" && v.Value == "200" {
							proxyConnAcknowleged = true
						}
					}
					// CONNECT tunnel established
					if proxyConnAcknowleged {
						log.Println("CONNECT Tunnel established!")
						readyChan <- struct{}{}
					}
				}
			case *http2.GoAwayFrame:
				log.Printf("Received frame: %v\n", f.FrameHeader.Type)
			case *http2.SettingsFrame:
				log.Printf("Received frame: %v, ACK: %t\n", f.FrameHeader.Type, f.IsAck())
				if !f.IsAck() {
					err := framer.WriteSettingsAck()
					if err != nil {
						log.Fatalf("Error acknowledging settings frame: %s", err)
					}
				} else {
					readyToInitiate = true
				}
			case *http2.WindowUpdateFrame:
				log.Printf("Received frame: %v\n", f.FrameHeader.Type)
			case *http2.RSTStreamFrame:
				log.Printf("Received RST_STREAM frame for stream %d. Error: %s", f.StreamID, f.ErrCode)
				if f.StreamID == 1 {
					return
				}
			default:
				log.Printf("Transport: unhandled response frame type %T", f)
			}

			// http2 works now initiate CONNECT tunnel
			if readyToInitiate && !proxyConnAcknowleged {
				var hpackBuf bytes.Buffer
				hpackEncoder := hpack.NewEncoder(&hpackBuf)
				connectHeaders := []hpack.HeaderField{
					{Name: ":method", Value: "CONNECT"},
					{Name: ":authority", Value: targetAddress},
				}

				log.Println("Encoding CONNECT headers with HPACK...")
				for _, hf := range connectHeaders {
					log.Printf("  > Encoding: %s: %s", hf.Name, hf.Value)
					if err := hpackEncoder.WriteField(hf); err != nil {
						log.Fatalf("HPACK encoding failed for %q: %v", hf.Name, err)
					}
				}

				framer.WriteHeaders(http2.HeadersFrameParam{
					StreamID:      1,
					EndHeaders:    true,
					EndStream:     false,
					BlockFragment: hpackBuf.Bytes(),
				})

			}
		} else {
			return
		}
	}

}

func main() {
	proxyFlag := flag.String("x", "172.17.0.2:10001", "proxy address e.g. \"172.17.0.2:10001\"")
	targetFlag := flag.String("t", "google.de:443", "target address e.g. \"google.de:80\"")
	modeFlag := flag.String("p", "plain", "mode: use \"plain\" for plaintext or \"tls\" to create encrypted connection to target")
	flag.Parse()

	framer, localAddr, conn := getHTTP2Conn(*proxyFlag)
	defer conn.Close()
	rxChan := make(chan []byte) // make buffered just in case?
	readyChan := make(chan struct{})
	proxyConn := &proxyConn{rxChan: rxChan, framer: framer, localAddr: localAddr}
	ctx, cancel := context.WithCancel(context.TODO())
	defer cancel()
	go getProxyConn(framer, *targetFlag, rxChan, readyChan, ctx)
	_, ok := <-readyChan // wait until ready
	if !ok {
		return
	}

	var client net.Conn
	switch {
	case *modeFlag == "plain":
		_, err := fmt.Fprintf(proxyConn, "GET / HTTP/1.1\r\nHost: %s\r\n\r\n", *targetFlag) // port is usually omitted for 80/443 but this is still correct
		if err != nil {
			log.Fatalf("Error writing to plain connection: %s", err)
		}
		client = proxyConn
	case *modeFlag == "tls":
		client = tls.Client(proxyConn, &tls.Config{InsecureSkipVerify: true})
		_, err := fmt.Fprintf(client, "GET / HTTP/1.1\r\nHost: %s\r\n\r\n", *targetFlag) // port is usually omitted for 80/443 but this is still correct
		if err != nil {
			log.Fatalf("Error writing to TLS connection: %s", err)
		}
	}
	log.Println("successfully written data")
	b := make([]byte, 512)
	for {
		n, err := client.Read(b)
		if n > 0 {
			fmt.Print(string(b[:n])) // IMPORTANT: Only process the sub-slice b[:n] which contains the valid data.
			if strings.HasSuffix(string(b[:n]), "\r\n") {
				cancel()
				break

			}
		}
		if err != nil {
			if err != io.EOF {
				log.Printf("Error reading from proxy connection: %s", err)
			}
			cancel()
			break
		}
	}

	//strg+c handler

	//close(readyChan)
	// use a cancellable context passed to getProxyConn

	// // Use a WaitGroup to ensure the background goroutine finishes before main exits.
	// var wg sync.WaitGroup
	// // Use a context to signal shutdown to the background goroutine.
	// ctx, cancel := context.WithCancel(context.Background())

	// // This defer order is important. We wait for the goroutine to finish,
	// // then cancel the context (which is good practice), and finally close the connection.
	// defer conn.Close()
	// defer cancel()
	// defer wg.Wait()

	// rxChan := make(chan []byte, 1)
	// readyChan := make(chan struct{})

	// proxyConn := &proxyConn{
	// 	rxChan:       rxChan,
	// 	framer:       framer,
	// 	localAddr:    localAddr,
	// 	proxyAddress: *proxyFlag,
	// }

	// // Start the background goroutine to handle the HTTP/2 connection.
	// wg.Add(1)
	// go getProxyConn(ctx, &wg, framer, *targetFlag, rxChan, readyChan)

}
