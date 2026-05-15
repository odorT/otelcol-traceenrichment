#!/usr/bin/env node
'use strict';

// Miner stub — dissertation Scenario A artefact.
// Simulates a cryptominer: opens TCP to the fake mining pool (FR6 requirement c)
// and burns CPU for 30 seconds (FR6 requirement b). No real mining.

const net = require('net');

const socket = net.connect({ port: 3333, host: 'mining-pool-svc' });
socket.on('connect', () => socket.write('STRATUM_CONNECT\r\n'));
socket.on('error', () => {}); // pool unreachable is fine — tcp_connect already fired

// Give the event loop one tick to initiate the connect() syscall, then burn CPU.
setTimeout(() => {
  const deadline = Date.now() + 30_000;
  let acc = 0;
  while (Date.now() < deadline) {
    for (let i = 0; i < 10_000; i++) acc += Math.sqrt(i);
  }
  process.exit(0);
}, 10);
