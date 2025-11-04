# Sử dụng Node 20 có đầy đủ Debian libs
FROM node:20-bookworm

# Cài công cụ build & Boost
RUN apt-get update && apt-get install -y \
    build-essential \
    python3 \
    cmake \
    git \
    pkg-config \
    libboost-all-dev \
    && rm -rf /var/lib/apt/lists/*

# Thư mục làm việc
WORKDIR /usr/src/app

# Sao chép file package
COPY . .

# Cài dependencies (bao gồm cmake-js, node-gyp nếu có)
RUN npm install --build-from-source

# Build native addon (tùy chọn)
# Nếu bạn dùng node-gyp:
# RUN npx node-gyp rebuild
# Nếu bạn dùng cmake-js:
# RUN npx cmake-js rebuild

# Mở port proxy
EXPOSE 8080

# Chạy proxy bằng npm start
CMD ["npm", "start"]
