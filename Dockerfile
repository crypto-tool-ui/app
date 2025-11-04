FROM node:20-bullseye as build

# Install build dependencies
RUN apt-get update && apt-get install -y \
    build-essential \
    libboost-all-dev \
    cmake \
    python3 \
    git \
    && rm -rf /var/lib/apt/lists/*

# Set work directory
WORKDIR /usr/src/app

# Copy source
COPY . .

# Build addon using node-gyp or cmake-js
# Uncomment the one that applies to your setup
RUN if [ -f binding.gyp ]; then \
      npx node-gyp rebuild; \
    elif [ -f CMakeLists.txt ]; then \
      npx cmake-js build; \
    else \
      echo "No build config found (binding.gyp or CMakeLists.txt)" && exit 1; \
    fi

# Runtime stage
FROM node:20-slim
WORKDIR /usr/src/app

# Copy built addon and JS files from builder
COPY --from=build /usr/src/app .

# Expose WebSocket proxy port
EXPOSE 8080

# Default command runs Node script that starts the proxy
CMD ["node", "-e", "const proxy=require('./build/Release/ws_tcp_proxy.node'); proxy.start('0.0.0.0',8080,4); setInterval(()=>{},1e6);"]
