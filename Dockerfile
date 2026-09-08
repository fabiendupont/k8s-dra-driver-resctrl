FROM --platform=$BUILDPLATFORM registry.access.redhat.com/ubi10/go-toolset:1.26 AS builder
ARG TARGETARCH

COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 GOOS=linux GOARCH=$TARGETARCH go build -buildvcs=false -o dra-resctrl ./cmd/driver

FROM registry.access.redhat.com/ubi10/ubi-micro:latest
COPY --from=builder /opt/app-root/src/dra-resctrl /usr/bin/dra-resctrl
ENTRYPOINT ["/usr/bin/dra-resctrl"]
