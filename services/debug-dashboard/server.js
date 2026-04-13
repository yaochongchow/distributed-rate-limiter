'use strict';

const express = require('express');
const http    = require('http');
const path    = require('path');
const axios   = require('axios');
const { WebSocketServer } = require('ws');
const Docker  = require('dockerode');

const app    = express();
const server = http.createServer(app);
const wss    = new WebSocketServer({ server, path: '/ws/alerts' });

const PORT             = process.env.PORT             || 4000;
const PROMETHEUS_URL   = process.env.PROMETHEUS_URL   || 'http://prometheus:9090';
const ALERTMANAGER_URL = process.env.ALERTMANAGER_URL || 'http://alertmanager:9093';
const LOKI_URL         = process.env.LOKI_URL         || 'http://loki:3100';
const COMPOSE_PROJECT  = process.env.COMPOSE_PROJECT  || 'distributed-rate-limiter';

// Docker socket — used for chaos engineering.
// Gracefully disabled if socket is unavailable.
let docker = null;
try {
  docker = new Docker({ socketPath: '/var/run/docker.sock' });
} catch (e) {
  console.warn('[chaos] Docker socket unavailable — chaos endpoints disabled');
}

app.use(express.json());
app.use(express.static(path.join(__dirname, 'public')));

// ---- Utility ----

async function promQuery(query) {
  const r = await axios.get(`${PROMETHEUS_URL}/api/v1/query`, {
    params: { query },
    timeout: 3000,
  });
  return r.data?.data?.result ?? [];
}

async function promQueryRange(query, start, end, step) {
  const r = await axios.get(`${PROMETHEUS_URL}/api/v1/query_range`, {
    params: { query, start, end, step },
    timeout: 5000,
  });
  return r.data?.data?.result ?? [];
}

async function lokiQuery(logQL, limit = 50) {
  const end   = Date.now() * 1e6;           // nanoseconds
  const start = end - 5 * 60 * 1e9;        // last 5 minutes
  const r = await axios.get(`${LOKI_URL}/loki/api/v1/query_range`, {
    params: { query: logQL, limit, start, end, direction: 'backward' },
    timeout: 4000,
  });
  return r.data?.data?.result ?? [];
}

// ---- Health endpoint ----

app.get('/api/health', async (req, res) => {
  const checks = await Promise.allSettled([
    promQuery('up{job="ratelimiter"}'),
    promQuery('up{job="prometheus"}'),
    axios.get(`${ALERTMANAGER_URL}/-/healthy`, { timeout: 2000 }),
    axios.get(`${LOKI_URL}/ready`,             { timeout: 2000 }),
  ]);

  const ratelimiterInstances = checks[0].status === 'fulfilled'
    ? checks[0].value.filter(r => r.value[1] === '1').length
    : 0;

  res.json({
    services: {
      ratelimiter: {
        status: ratelimiterInstances > 0 ? 'up' : 'down',
        instances: ratelimiterInstances,
      },
      prometheus: {
        status: checks[1].status === 'fulfilled' && checks[1].value.length > 0 ? 'up' : 'down',
      },
      alertmanager: {
        status: checks[2].status === 'fulfilled' ? 'up' : 'down',
      },
      loki: {
        status: checks[3].status === 'fulfilled' ? 'up' : 'down',
      },
    },
  });
});

// ---- Metrics summary ----

app.get('/api/metrics/summary', async (req, res) => {
  try {
    const [reqRate, allowRate, p50, p95, p99, instances] = await Promise.all([
      promQuery('sum(rate(ratelimiter_requests_total[1m]))'),
      promQuery('sum(rate(ratelimiter_requests_total{allowed="true"}[1m])) / sum(rate(ratelimiter_requests_total[1m]))'),
      promQuery('histogram_quantile(0.50, sum(rate(ratelimiter_allow_latency_seconds_bucket[5m])) by (le))'),
      promQuery('histogram_quantile(0.95, sum(rate(ratelimiter_allow_latency_seconds_bucket[5m])) by (le))'),
      promQuery('histogram_quantile(0.99, sum(rate(ratelimiter_allow_latency_seconds_bucket[5m])) by (le))'),
      promQuery('count(up{job="ratelimiter"} == 1)'),
    ]);

    const val = arr => parseFloat(arr[0]?.value?.[1] ?? 0);

    res.json({
      request_rate:    val(reqRate),
      allow_rate:      val(allowRate),
      latency_p50_ms:  val(p50)  * 1000,
      latency_p95_ms:  val(p95)  * 1000,
      latency_p99_ms:  val(p99)  * 1000,
      active_instances: val(instances),
    });
  } catch (err) {
    res.status(502).json({ error: err.message });
  }
});

// ---- Metrics time-series (last 5 min) ----

app.get('/api/metrics/timeseries', async (req, res) => {
  const end   = Math.floor(Date.now() / 1000);
  const start = end - 300;
  const step  = 10;
  try {
    const [allowed, denied] = await Promise.all([
      promQueryRange('sum(rate(ratelimiter_requests_total{allowed="true"}[30s]))', start, end, step),
      promQueryRange('sum(rate(ratelimiter_requests_total{allowed="false"}[30s]))', start, end, step),
    ]);
    res.json({ allowed, denied });
  } catch (err) {
    res.status(502).json({ error: err.message });
  }
});

// ---- Active alerts ----

app.get('/api/alerts', async (req, res) => {
  try {
    const r = await axios.get(`${ALERTMANAGER_URL}/api/v2/alerts`, { timeout: 3000 });
    res.json(r.data);
  } catch (err) {
    res.status(502).json({ error: err.message });
  }
});

// ---- Recent logs from Loki ----

app.get('/api/logs', async (req, res) => {
  const service = req.query.service || 'ratelimiter';
  const level   = req.query.level || '';
  let logQL = `{service="${service}"}`;
  if (level) logQL += ` | json | level="${level}"`;

  try {
    const streams = await lokiQuery(logQL, 100);
    const lines = [];
    for (const s of streams) {
      for (const [ts, line] of (s.values || [])) {
        lines.push({ ts: Number(ts), line, labels: s.stream });
      }
    }
    lines.sort((a, b) => b.ts - a.ts);
    res.json(lines.slice(0, 100));
  } catch (err) {
    res.status(502).json({ error: err.message });
  }
});

// ---- AlertManager webhook receiver ----
// AlertManager posts firing/resolved alerts here.

const activeAlerts = new Map();

app.post('/api/alertmanager/webhook', (req, res) => {
  const payload = req.body;
  for (const alert of payload.alerts || []) {
    const key = alert.labels?.alertname + ':' + JSON.stringify(alert.labels);
    if (alert.status === 'resolved') {
      activeAlerts.delete(key);
    } else {
      activeAlerts.set(key, { ...alert, receivedAt: Date.now() });
    }
  }

  // Fan-out to WebSocket subscribers
  const msg = JSON.stringify({ type: 'alerts', alerts: [...activeAlerts.values()] });
  for (const ws of wss.clients) {
    if (ws.readyState === 1 /* OPEN */) ws.send(msg);
  }

  res.sendStatus(200);
});

// ---- Chaos engineering ----

async function getContainers(nameFilter) {
  if (!docker) throw new Error('Docker socket unavailable');
  const all = await docker.listContainers({ all: false });
  return all.filter(c =>
    c.Labels?.['com.docker.compose.project'] &&
    c.Names.some(n => n.includes(nameFilter))
  );
}

app.post('/api/chaos/kill-ratelimiter', async (req, res) => {
  try {
    const containers = await getContainers('ratelimiter');
    if (containers.length === 0) return res.status(404).json({ error: 'No ratelimiter containers running' });
    const target = containers[Math.floor(Math.random() * containers.length)];
    await docker.getContainer(target.Id).stop({ t: 1 });
    res.json({ killed: target.Names[0], remaining: containers.length - 1 });
  } catch (err) {
    res.status(500).json({ error: err.message });
  }
});

app.post('/api/chaos/kill-redis', async (req, res) => {
  try {
    const containers = await getContainers('redis-');
    const eligible = containers.filter(c => c.Names.some(n => /redis-[1-6]/.test(n)));
    if (eligible.length === 0) return res.status(404).json({ error: 'No redis containers found' });
    const target = eligible[Math.floor(Math.random() * eligible.length)];
    await docker.getContainer(target.Id).stop({ t: 1 });
    res.json({ killed: target.Names[0] });
  } catch (err) {
    res.status(500).json({ error: err.message });
  }
});

app.post('/api/chaos/restore', async (req, res) => {
  try {
    if (!docker) throw new Error('Docker socket unavailable');
    const all = await docker.listContainers({ all: true });
    const stopped = all.filter(c =>
      c.State === 'exited' &&
      c.Labels?.['com.docker.compose.project']
    );
    const results = await Promise.allSettled(
      stopped.map(c => docker.getContainer(c.Id).start())
    );
    res.json({
      restored: stopped.map((c, i) => ({
        name: c.Names[0],
        ok: results[i].status === 'fulfilled',
      })),
    });
  } catch (err) {
    res.status(500).json({ error: err.message });
  }
});

app.get('/api/chaos/status', async (req, res) => {
  try {
    if (!docker) return res.json({ available: false });
    const all = await docker.listContainers({ all: true });
    const compose = all.filter(c => c.Labels?.['com.docker.compose.project']);
    res.json({
      available: true,
      containers: compose.map(c => ({
        name: c.Names[0],
        state: c.State,
        status: c.Status,
        image: c.Image,
      })),
    });
  } catch (err) {
    res.status(500).json({ error: err.message });
  }
});

// ---- WebSocket — real-time alert streaming ----

wss.on('connection', (ws) => {
  // Send current alert state on connect
  ws.send(JSON.stringify({ type: 'alerts', alerts: [...activeAlerts.values()] }));

  // Periodically push metrics
  const interval = setInterval(async () => {
    if (ws.readyState !== 1) { clearInterval(interval); return; }
    try {
      const [reqRate, allowRate] = await Promise.all([
        promQuery('sum(rate(ratelimiter_requests_total[30s]))'),
        promQuery('sum(rate(ratelimiter_requests_total{allowed="true"}[30s])) / sum(rate(ratelimiter_requests_total[30s]))'),
      ]);
      ws.send(JSON.stringify({
        type: 'metrics',
        request_rate: parseFloat(reqRate[0]?.value?.[1] ?? 0),
        allow_rate:   parseFloat(allowRate[0]?.value?.[1] ?? 0),
      }));
    } catch {}
  }, 3000);

  ws.on('close', () => clearInterval(interval));
});

// ---- Start ----

server.listen(PORT, () => {
  console.log(JSON.stringify({
    level: 'info',
    service: 'debug-dashboard',
    message: `listening on :${PORT}`,
    prometheus: PROMETHEUS_URL,
    alertmanager: ALERTMANAGER_URL,
    loki: LOKI_URL,
  }));
});
