# # Sử dụng Node 20 có đầy đủ Debian libs
# FROM node:20

# # Cài công cụ build & Boost
# RUN apt-get update && apt-get install -y \
#     build-essential \
#     python3 \
#     cmake \
#     git \
#     pkg-config \
#     && rm -rf /var/lib/apt/lists/*

# # Thư mục làm việc
# WORKDIR /usr/src/app

# # Sao chép file package
# COPY . .

# # Cài dependencies (bao gồm cmake-js, node-gyp nếu có)
# RUN npm install

# # Mở port proxy
# EXPOSE 8000

# # Chạy proxy bằng npm start
# CMD ["npm", "start"]

# Multi-stage build for optimal size and security

# Stage 1: Build stage
FROM golang:1.21-alpine AS builder

# Install build dependencies
RUN apk add --no-cache git ca-certificates tzdata

# Set working directory
WORKDIR /build

# Copy go mod files
COPY go.mod go.sum ./

# Download dependencies
RUN go mod download

# Copy source code
COPY main.go ./

# Build with optimizations
RUN CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build \
    -a \
    -installsuffix cgo \
    -ldflags='-s -w -extldflags "-static"' \
    -gcflags="all=-l -B" \
    -trimpath \
    -o app main.go

# Stage 2: Runtime stage (minimal)
FROM scratch

# Copy timezone data
COPY --from=builder /usr/share/zoneinfo /usr/share/zoneinfo

# Copy SSL certificates
COPY --from=builder /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/

# Copy binary from builder
COPY --from=builder /build/app /app

# Expose port
EXPOSE 8000

# Run as non-root (optional metadata)
USER 65534:65534

# Set entrypoint
ENTRYPOINT ["/app"]
