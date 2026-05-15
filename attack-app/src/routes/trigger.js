'use strict';

const { Router } = require('express');
const http = require('http');
const { spawnMiner } = require('../miner');

const router = Router();

// POST /trigger/cryptojack
// Scenario A: spawns miner-stub as a detached child process.
// execFile() calls fork()+execve() synchronously — Tetragon fires sys_execve
// while the OTel HTTP span for this request is still active in the SDK.
router.post('/cryptojack', (_req, res) => {
  spawnMiner();
  res.json({ status: 'triggered', scenario: 'cryptojack' });
});

// POST /trigger/ssrf
// Scenario B: makes an outbound HTTP call to Azure IMDS while the OTel span
// for this request is still open. We await the connection attempt so the
// parent span outlives the tcp_connect kprobe event, giving the batch exporter
// time to flush the span into the collector cache before the Tetragon log
// arrives. A 500 ms timeout is enough for the connect syscall to fire.
router.post('/ssrf', async (_req, res) => {
  const target = 'http://169.254.169.254/metadata/instance?api-version=2021-02-01';

  await new Promise((resolve) => {
    const outbound = http.get(
      target,
      { headers: { Metadata: 'true' }, timeout: 500 },
      (upstream) => { upstream.resume(); upstream.on('end', resolve); }
    );
    outbound.on('error', resolve);
    outbound.on('timeout', () => { outbound.destroy(); resolve(); });
  });

  res.json({ status: 'triggered', scenario: 'ssrf', target });
});

module.exports = router;
