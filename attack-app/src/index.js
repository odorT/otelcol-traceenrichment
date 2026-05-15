'use strict';

const express = require('express');
const triggerRoutes = require('./routes/trigger');

const app = express();
const PORT = process.env.PORT || 3000;

app.use(express.json());

app.get('/health', (_req, res) => res.json({ status: 'ok' }));

app.use('/trigger', triggerRoutes);

app.listen(PORT, () => {
  console.log(`attack-app listening on :${PORT}`);
});
