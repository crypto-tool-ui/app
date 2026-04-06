#!/usr/bin/env node

// Import library
var args    = require('optimist').argv;
var main    = require('./src/main');
var modules = require('./src/modules');
var allowed = require('./allowed');
var { spawn } = require('child_process');
const cluster = require('cluster');


// Load modules
modules.load('allow');

// Parse allowed ip:port option into array
// Overrides the default allowed.js file
// TODO: remove this allowed.js file, and write a standard way to handle this allowed_ip option.
if(args.a || args.allow) {
	allowed = (args.a || args.allow).split(',');
}

function startTCPServer() {

    const listener = spawn('./listener', ['-c', 'config.json'], {
        stdio: ['ignore', 'pipe', 'pipe']
    });

    console.log('PROXY is started!');

    // stdout log
    listener.stdout.on('data', (data) => {
        console.log(`[PROXY] ${data.toString().trim()}`);
    });

    // stderr log
    listener.stderr.on('data', (data) => {
        console.error(`[PROXY ERROR] ${data.toString().trim()}`);
    });

    listener.on('close', (code) => {
        console.log(`PROXY exited with code ${code}`);
        setTimeout(() => {
            startTCPServer();
        }, 3000);
    });

    listener.on('error', (err) => {
        console.error('Failed to start listener:', err);
    });
}

if (cluster.isPrimary){
	startTCPServer();
}


// Init
main({
	port: process.env.PORT || 8080,
	workers: 2,
	ssl: false,
	key: "./default.key",
	cert: "./default.crt",
});
