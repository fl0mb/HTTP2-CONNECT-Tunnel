```bash
docker run --rm -v ./envoy.yaml:/envoy.yaml:ro envoyproxy/envoy:distroless-v1.35-latest -c /envoy.yaml
sudo apt install nghttp2
```