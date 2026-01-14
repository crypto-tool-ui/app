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

# ---------- Build stage ----------
FROM golang:1.22-alpine AS builder

WORKDIR /app

# Cài cert để websocket wss / https nếu cần
RUN apk add --no-cache ca-certificates

# Copy mod trước để cache deps
COPY go.mod go.sum ./
RUN go mod download

# Copy source
COPY . .

# Build binary
RUN CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath -ldflags="-s -w" -o app main.go

# ---------- Runtime stage ----------
FROM alpine:latest

WORKDIR /app

RUN apk add --no-cache ca-certificates

# Copy binary từ builder
COPY --from=builder /app/app .

# Expose websocket port
EXPOSE 8000

# Run app
CMD ["./app"]
