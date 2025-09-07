# Stage 1: Build the Go application
FROM golang:1.22-alpine AS builder

WORKDIR /app

# Copy go.mod and go.sum files to download dependencies
COPY go.* ./
RUN go mod download

# Copy the rest of the application source code
COPY *.go ./

# Build the application
RUN CGO_ENABLED=0 GOOS=linux go build -o /go-app

# Stage 2: Create a minimal final image
FROM alpine:latest

WORKDIR /

# Copy the built binary from the builder stage
COPY --from=builder /go-app /go-app

EXPOSE 8080

# Run the binary
ENTRYPOINT ["/go-app"]
