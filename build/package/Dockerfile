# syntax=docker/dockerfile:1
FROM golang:1.27.2 AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/tcp-proxy ./cmd/tcp-proxy

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/tcp-proxy /tcp-proxy
COPY configs/proxy.yaml /etc/tcp-proxy/proxy.yaml
EXPOSE 3307 6380 9090
ENTRYPOINT ["/tcp-proxy", "-config", "/etc/tcp-proxy/proxy.yaml"]
