# Sử dụng Node 20 có đầy đủ Debian libs
FROM node:20

# Cài công cụ build & Boost
RUN apt-get update && apt-get install -y \
    build-essential \
    python3 \
    cmake \
    git \
    pkg-config \
    && rm -rf /var/lib/apt/lists/*

# Thư mục làm việc
WORKDIR /usr/src/app

# Sao chép file package
COPY . .

# Cài dependencies (bao gồm cmake-js, node-gyp nếu có)
RUN npm install

# Mở port proxy
EXPOSE 8000

# Chạy proxy bằng npm start
CMD ["npm", "prod"]
