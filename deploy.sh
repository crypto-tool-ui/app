#!/bin/bash

set -e

ulimit -n 100000

echo "🚀 Mining Proxy Multi-Node Deployment"
echo "======================================"

# Configuration
NODES=${NODES:-4}
MAX_CONN_PER_NODE=${MAX_CONN_PER_NODE:-25000}

# Colors
GREEN='\033[0;32m'
YELLOW='\033[1;33m'
RED='\033[0;31m'
NC='\033[0m' # No Color

# Functions
log_info() {
    echo -e "${GREEN}[INFO]${NC} $1"
}

log_warn() {
    echo -e "${YELLOW}[WARN]${NC} $1"
}

log_error() {
    echo -e "${RED}[ERROR]${NC} $1"
}

# Check prerequisites
check_prerequisites() {
    log_info "Checking prerequisites..."
    
    if ! command -v docker &> /dev/null; then
        log_error "Docker is not installed"
        exit 1
    fi
    
    if ! command -v docker-compose &> /dev/null; then
        log_error "Docker Compose is not installed"
        exit 1
    fi
    
    log_info "Prerequisites OK"
}

# Build images
build_images() {
    log_info "Building mining proxy image..."
    docker build -t mining-proxy:latest .
    log_info "Build complete"
}

# Deploy stack
deploy_stack() {
    log_info "Deploying $NODES proxy nodes with HAProxy..."
    docker-compose up -d --build
    log_info "Stack deployed"
}

# Wait for services to be healthy
wait_for_health() {
    log_info "Waiting for services to be healthy..."
    
    local max_wait=60
    local waited=0
    
    while [ $waited -lt $max_wait ]; do
        if curl -f -s http://localhost/health > /dev/null 2>&1; then
            log_info "Services are healthy"
            return 0
        fi
        sleep 2
        waited=$((waited + 2))
        echo -n "."
    done
    
    echo ""
    log_warn "Services health check timeout"
}

# Show status
show_status() {
    log_info "Deployment Status:"
    echo ""
    docker-compose ps
    echo ""
    
    log_info "HAProxy Stats: http://localhost:8404/stats"
    log_info "Health Check: http://localhost/health"
    log_info "WebSocket: ws://localhost/BASE64_ENCODED_ADDRESS"
    echo ""
    
    log_info "Current connections per node:"
    for i in $(seq 1 $NODES); do
        if curl -f -s http://localhost/health > /dev/null 2>&1; then
            echo "  Node $i: $(docker logs mining-proxy-$i 2>&1 | grep 'Online:' | tail -1 | grep -oP '\[Online: \K[0-9]+')/25000"
        fi
    done
}

# Show logs
show_logs() {
    log_info "Showing logs (Ctrl+C to exit)..."
    docker-compose logs -f
}

# Scale nodes
scale_nodes() {
    local count=$1
    log_info "Scaling to $count nodes..."
    
    # Update docker-compose.yml dynamically or use docker-compose scale
    docker-compose up -d --scale proxy=$count
    log_info "Scaled to $count nodes"
}

# Stop all services
stop_all() {
    log_info "Stopping all services..."
    docker-compose down
    log_info "Services stopped"
}

# Clean everything
clean_all() {
    log_info "Cleaning up mining-proxy stack..."

    # Stop & remove compose stack + orphan containers
    docker-compose down -v --remove-orphans

    # Remove containers using mining-proxy image
    docker ps -a --filter "ancestor=mining-proxy:latest" -q | xargs -r docker rm -f

    # Remove mining-proxy image
    docker rmi mining-proxy:latest 2>/dev/null || true

    # Remove unused networks created by docker-compose
    docker network prune -f

    # Remove dangling images
    docker image prune -f

    # Remove unused volumes
    docker volume prune -f

    log_info "Cleanup complete ✅"
}


# Monitor
monitor() {
    log_info "Starting monitor (refresh every 5s, Ctrl+C to exit)..."
    
    while true; do
        clear
        echo "=== Mining Proxy Cluster Status ==="
        echo ""
        
        # HAProxy stats
        echo "HAProxy Status:"
        curl -s http://localhost:8404/stats | grep -E "proxy[1-4]" | awk '{print $1}' || echo "  Not available"
        echo ""
        
        # Individual node stats
        echo "Node Statistics:"
        for i in $(seq 1 $NODES); do
            health=$(curl -s http://localhost/health 2>/dev/null || echo '{"connections":0}')
            echo "  Node $i: $health"
        done
        echo ""
        
        # Docker stats
        echo "Resource Usage:"
        docker stats --no-stream --format "table {{.Name}}\t{{.CPUPerc}}\t{{.MemUsage}}" | grep -E "mining-proxy|haproxy"
        
        sleep 5
    done
}

# Main script
case "${1:-deploy}" in
    check)
        check_prerequisites
        ;;
    build)
        check_prerequisites
        build_images
        ;;
    deploy)
        check_prerequisites
        build_images
        deploy_stack
        wait_for_health
        show_status
        ;;
    status)
        show_status
        ;;
    logs)
        show_logs
        ;;
    monitor)
        monitor
        ;;
    scale)
        scale_nodes ${2:-4}
        ;;
    stop)
        stop_all
        ;;
    clean)
        clean_all
        ;;
    restart)
        stop_all
        sleep 2
        deploy_stack
        wait_for_health
        show_status
        ;;
    *)
        echo "Usage: $0 {check|build|deploy|status|logs|monitor|scale|stop|clean|restart}"
        echo ""
        echo "Commands:"
        echo "  check    - Check prerequisites"
        echo "  build    - Build Docker images"
        echo "  deploy   - Deploy full stack (default)"
        echo "  status   - Show deployment status"
        echo "  logs     - Show logs (follow)"
        echo "  monitor  - Real-time monitoring"
        echo "  scale N  - Scale to N nodes"
        echo "  stop     - Stop all services"
        echo "  clean    - Remove everything"
        echo "  restart  - Restart all services"
        exit 1
        ;;
esac
