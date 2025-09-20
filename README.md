Tool to play with the HTTP/2 `CONNECT` method. It allows scanning for misconfigured proxies and their accessible internal services. There is also a proof of concept mode to establish a `CONNECT` tunnel that could be used with `net.Conn`.

For details, see the [blog post](https://blog.flomb.net/posts/http2connect/)

```
Usage of ./http2ConnTun:
  -b int
    	Batch size for port scanning (default 100)
  -c	enable connection mode, where a local listener forwards traffic into and out of the CONNECT tunnel
  -l string
    	Address of local listener for connect mode (default "127.0.0.1:8080")
  -p string
    	Port(s) to scan or connect to, format similar to nmap e.g. 80,443,1000-2000
  -proxy-file string
    	File containing line separated proxy addresses to connect to e.g. "http://172.17.0.2:8080"
  -t string
    	Timeout for proxy connection e.g. "1s" (default "5s")
  -u string
    	Target host(s) e.g. "example.com" or "1.2.3.4,10.0.1/24"
  -v	Enable verbose logging
  -w string
    	Wait time in between batches for port scanning e.g. "1s" (default "1s")
  -x string
    	Proxy address to connect to e.g. "http://172.17.0.2:8080"
```

Start an example envoy or apache httpd server with the provided configurations:
```bash
docker run --rm -v ./envoy.yaml:/envoy.yaml:ro envoyproxy/envoy:distroless-v1.35-latest -c /envoy.yaml
docker run --rm -p 8080:8080 -v ./httpd.conf:/usr/local/apache2/conf/httpd.conf httpd:2.4.65
```


Probe a single potential proxy for a address and a range of ports:
```
└─$ ./http2ConnTun -x http://172.17.0.2:10001 -u 172.17.0.1 -p 8000-9000
2025/09/13 10:42:53 INFO Connected to proxy
2025/09/13 10:43:01 INFO Found accessible target: 172.17.0.1:8888 for proxy http://172.17.0.2:10001
```


Probe multiple potential proxies for multiple addresses and ports:
```
└─$ ./http2ConnTun -proxy-file proxies.txt -u 127.0.0.1,google.de -p 80,10001
2025/09/13 10:43:37 error connteting to proxy http://127.0.0.1:80: error connecting: dial tcp 127.0.0.1:80: connect: connection refused
2025/09/13 10:43:37 INFO Connected to proxy
2025/09/13 10:43:37 INFO Found accessible target: 127.0.0.1:10001 for proxy http://172.17.0.2:10001
2025/09/13 10:43:37 INFO Found accessible target: google.de:80 for proxy http://172.17.0.2:10001

└─$ cat proxies.txt 
http://127.0.0.1:80
http://172.17.0.2:10001
```

In connect mode establish a `CONNECT` tunnel and forward everything received on a local listener into the tunnel:
```
└─$ ./http2ConnTun -x http://172.17.0.2:8080 -u google.de -p 80 -c -l 127.0.0.1:8000
2025/09/20 20:56:02 INFO Connected to proxy


└─$ curl http://127.0.0.1:8000
<HTML><HEAD><meta http-equiv="content-type" content="text/html;charset=utf-8">
<TITLE>301 Moved</TITLE></HEAD><BODY>
<H1>301 Moved</H1>
The document has moved
<A HREF="http://www.google.com:8000/">here</A>.
</BODY></HTML>
```