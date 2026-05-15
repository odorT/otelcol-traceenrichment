'use strict';

const { execFile } = require('child_process');

function spawnMiner() {
  const child = execFile(
    '/usr/local/bin/miner-stub',
    [],
    { detached: true, stdio: 'ignore' }
  );
  child.unref();
}

module.exports = { spawnMiner };
