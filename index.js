const proxy = require('./build/Release/ws_tcp_proxy');

// Helper to encode TCP address to base64
function encodeTcpAddress(host, port) {
    return Buffer.from(`${host}:${port}`).toString('base64');
}

// Start proxy server
function startProxy(host = '0.0.0.0', port = 8080, threads = 0) {
    try {
        proxy.start(host, port, threads);
        console.log(`WebSocket proxy started on ${host}:${port}`);
        console.log(`Threads: ${threads || 'auto-scaled'}`);
        console.log('\nUsage:');
        console.log('  Connect WebSocket to: ws://<host>:<port>/<base64_tcp_address>');
        console.log('  Example: ws://localhost:8080/' + encodeTcpAddress('example.com', 80));
    } catch (err) {
        console.error('Failed to start proxy:', err.message);
        process.exit(1);
    }
}

// Stop proxy server
function stopProxy() {
    proxy.stop();
    console.log('Proxy stopped');
}

// Graceful shutdown
process.on('SIGINT', () => {
    console.log('\nShutting down...');
    stopProxy();
    process.exit(0);
});

process.on('SIGTERM', () => {
    stopProxy();
    process.exit(0);
});

// Example usage
if (require.main === module) {
    const args = process.argv.slice(2);
    const host = '0.0.0.0';
    const port = parseInt(args[1]) || 8000;
    const threads = parseInt(args[2]) || 0;
    
    startProxy(host, port, threads);
    
    // Keep process alive
    setInterval(() => {
        if (!proxy.isRunning()) {
            console.error('Proxy stopped unexpectedly');
            process.exit(1);
        }
    }, 5000);
}

// Export for use as module
module.exports = {
    start: startProxy,
    stop: stopProxy,
    isRunning: () => proxy.isRunning(),
    encodeTcpAddress
};
