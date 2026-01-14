# Mining Proxy Multi-Node Deployment Guide

## 🏗️ Architecture

```
                    ┌─────────────┐
                    │   HAProxy   │
                    │  Port: 80   │
                    │ Stats: 8404 │
                    └──────┬──────┘
                           │
            ┌──────────────┼──────────────┐
            │              │              │
       ┌────▼────┐    ┌────▼────┐   ┌────▼────┐
       │ Proxy 1 │    │ Proxy 2 │   │ Proxy 3 │ ...
       │ 25k max │    │ 25k max │   │ 25k max │
       └─────────┘    └─────────┘   └─────────┘
```

## 📁 File Structure

```
mining-proxy/
├── main.go              # Proxy source code
├── go.mod
├── go.sum
├── Dockerfile           # Proxy container
├── docker-compose.yml   # Multi-node orchestration
├── haproxy.cfg         # Load balancer config
├── deploy.sh           # Deployment script
└── DEPLOYMENT.md       # This file
```

## 🚀 Quick Start

### 1. Deploy Everything

```bash
chmod +x deploy.sh
./deploy.sh deploy
```

This will:
- ✅ Build mining-proxy image
- ✅ Start 4 proxy nodes (25k connections each = 100k total)
- ✅ Start HAProxy load balancer
- ✅ Verify health checks
- ✅ Show status

### 2. Access Services

| Service | URL | Description |
|---------|-----|-------------|
| WebSocket | `ws://YOUR_IP/BASE64_ADDRESS` | Mining proxy endpoint |
| HAProxy Stats | `http://YOUR_IP:8404/stats` | Real-time dashboard |
| Health Check | `http://YOUR_IP/health` | Cluster health |

### 3. Monitor Cluster

```bash
# Real-time monitoring
./deploy.sh monitor

# View logs
./deploy.sh logs

# Check status
./deploy.sh status
```

## 🔧 Configuration

### Scaling Nodes

**Edit `docker-compose.yml`** to add/remove nodes:

```yaml
# Add Node 5
proxy5:
  build:
    context: .
    dockerfile: Dockerfile
  image: mining-proxy:latest
  container_name: mining-proxy-5
  environment:
    - PORT=8000
    - MAX_CONNECTIONS=25000
  networks:
    - mining-network
```

**Update `haproxy.cfg`**:

```haproxy
backend mining_proxies
    server proxy5 proxy5:8000 check inter 5s rise 2 fall 3 maxconn 25000
```

Then restart:

```bash
./deploy.sh restart
```

### Adjusting Connection Limits

**Per Node:**
```yaml
environment:
  - MAX_CONNECTIONS=50000  # Increase to 50k per node
```

**HAProxy:**
```haproxy
server proxy1 proxy1:8000 check maxconn 50000
```

### Resource Limits

```yaml
deploy:
  resources:
    limits:
      cpus: '4'      # Increase CPU
      memory: 4G     # Increase memory
```

## 📊 HAProxy Stats Dashboard

Access `http://YOUR_IP:8404/stats` to see:
- ✅ Active connections per node
- ✅ Request rate
- ✅ Health status (UP/DOWN)
- ✅ Session count
- ✅ Error rates

## 🔍 Health Checks

HAProxy checks each node every 5 seconds:

```bash
# Check overall health
curl http://localhost/health

# Response example:
{"status":"healthy","connections":1234,"max":100000}
```

**Status codes:**
- `200 OK` - Cluster healthy
- `503 Service Unavailable` - At capacity

## 🎯 Load Balancing Strategy

HAProxy uses **source IP hashing**:
- Same miner → Same proxy node
- Maintains session affinity
- Sticky for 24 hours

**Algorithm:**
```haproxy
balance source
stick-table type ip size 1m expire 24h
stick on src
```

## 📈 Capacity Planning

| Nodes | Connections/Node | Total Capacity |
|-------|------------------|----------------|
| 2     | 50,000          | 100,000        |
| 4     | 25,000          | 100,000        |
| 8     | 12,500          | 100,000        |
| 10    | 50,000          | 500,000        |

## 🛠️ Common Operations

### View Container Stats
```bash
docker stats
```

### Check Logs for Specific Node
```bash
docker logs -f mining-proxy-1
```

### Restart Single Node
```bash
docker-compose restart proxy1
```

### Scale Horizontally
```bash
# Add more nodes
docker-compose up -d --scale proxy=8
```

### Update Configuration
```bash
# Edit haproxy.cfg
vim haproxy.cfg

# Restart HAProxy
docker-compose restart haproxy
```

## 🔥 Production Tips

### 1. Use Docker Swarm or Kubernetes

For large scale (1M+ connections):

```bash
# Docker Swarm
docker swarm init
docker stack deploy -c docker-compose.yml mining
```

### 2. External Load Balancer

Use cloud load balancers:
- AWS ALB/NLB
- GCP Load Balancer
- Cloudflare Load Balancing

### 3. Monitoring Stack

Add Prometheus + Grafana:

```yaml
prometheus:
  image: prom/prometheus
  volumes:
    - ./prometheus.yml:/etc/prometheus/prometheus.yml
    
grafana:
  image: grafana/grafana
  ports:
    - "3000:3000"
```

### 4. Log Aggregation

Use ELK stack or Loki for centralized logs.

### 5. Auto-Scaling

Set up auto-scaling based on:
- CPU usage > 70%
- Connection count > 80% capacity
- Response time > 500ms

## 🐛 Troubleshooting

### Issue: Node shows DOWN in HAProxy

```bash
# Check node health
curl http://proxy1:8000/health

# Check logs
docker logs mining-proxy-1
```

### Issue: High connection errors

```bash
# Check HAProxy stats
curl http://localhost:8404/stats

# Increase connection limits
# Edit MAX_CONNECTIONS in docker-compose.yml
```

### Issue: Slow response time

```bash
# Check resource usage
docker stats

# Add more nodes or increase resources
```

## 📞 Testing

### Test WebSocket Connection

```bash
# Install wscat
npm install -g wscat

# Encode pool address
echo -n "pool.example.com:3333" | base64
# Output: cG9vbC5leGFtcGxlLmNvbTozMzMz

# Connect
wscat -c ws://localhost/cG9vbC5leGFtcGxlLmNvbTozMzMz
```

### Load Testing

```bash
# Install k6
brew install k6  # macOS
# or download from k6.io

# Run load test
k6 run loadtest.js
```

## 🎉 Summary

You now have:
- ✅ 4 proxy nodes (100k total capacity)
- ✅ HAProxy load balancer with health checks
- ✅ Real-time monitoring dashboard
- ✅ Session affinity
- ✅ Auto-restart on failure
- ✅ Resource limits
- ✅ Centralized logging

**Total capacity: 100,000 concurrent mining connections!**
