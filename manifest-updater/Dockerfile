FROM golang:1.15-alpine AS builder
# Get latest ca certs
RUN apk --update add ca-certificates

WORKDIR /workspace
# Copy go modules first
COPY go.mod .
COPY go.sum .
# cache deps before building and copying source so that we don't need to re-download as much
# and so that source changes don't invalidate our downloaded layer
RUN go mod download
# Copy entire directory over
COPY . .

# Build
RUN CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -o updater main.go

FROM scratch
WORKDIR /

# Bring ca certs
COPY --from=builder /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/ca-certificates.crt
COPY --from=builder /workspace/updater .

ENTRYPOINT ["/updater"]

