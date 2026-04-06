/**
 * Dependencies
 */
var net        = require('net');
var mes        = require('./message');

/**
 * Constructor
 */
var Proxy = function Constructor(ws) {
	const to = 'MTI3LjAuMC4xOjQ1Njc=';
	this._tcp;
	this._from = ws.upgradeReq.connection.remoteAddress;
	this._to   = Buffer.from(to, 'base64').toString();
	this._ws   = ws;

	// Bind data
	this._ws.on('message', this.clientData.bind(this) );
	this._ws.on('close', this.close.bind(this) );
	this._ws.on('error', (error) => {
		console.log(error);
	});

	// Initialize proxy
	var args = this._to.split(':');

	// Connect to server
	mes.info("Requested connection from '%s' to '%s' [ACCEPTED].", this._from, this._to);
	this._tcp = net.connect( args[1], args[0] );

	// Disable nagle algorithm
	this._tcp.setTimeout(0)
	this._tcp.setNoDelay(true)

	this._tcp.on('data', this.serverData.bind(this) );
	this._tcp.on('close', this.close.bind(this) );
	this._tcp.on('error', function(error) {
		console.log(error);
	});
	
	this._tcp.on('connect', this.connectAccept.bind(this) );
}


/**
 * OnClientData
 * Client -> Server
 */
Proxy.prototype.clientData = function OnServerData(data) {
	if (!this._tcp) {
		// wth ? Not initialized yet ?
		return;
	}

	try {
		const msg = data.toString('utf-8');
        const message = msg.endsWith('\n') ? msg : msg + '\n';
		this._tcp.write(message);
	}
	catch(e) {}
}


/**
 * OnServerData
 * Server -> Client
 */
Proxy.prototype.serverData = function OnClientData(data) {
	this._ws.send(data.toString(), function(error){
		/*
		if (error !== null) {
			OnClose();
		}
		*/
	});
}


/**
 * OnClose
 * Clean up events/sockets
 */
Proxy.prototype.close = function OnClose() {
	mes.info("Connection closed from '%s' -> '%s'.", this._from, this._to);
	if (this._tcp) {
		this._tcp.removeListener('close', this.close.bind(this) );
		this._tcp.removeListener('error', this.close.bind(this) );
		this._tcp.removeListener('data',  this.serverData.bind(this) );
		this._tcp.end();
	}

	if (this._ws) {
		this._ws.removeListener('close',   this.close.bind(this) );
		this._ws.removeListener('error',   this.close.bind(this) );
		this._ws.removeListener('message', this.clientData.bind(this) );
		this._ws.close();
	}
}


/**
 * On server accepts connection
 */
Proxy.prototype.connectAccept = function OnConnectAccept() {
	mes.status("Connection accepted from '%s'.", this._to);
}

/**
 * Exports
 */
module.exports = Proxy;
