const http = require('http');
const cluster = require('cluster');
const os = require('os');
const { Pool } = require('pg');
const Redis = require('ioredis');

const PORT = parseInt(process.env.PORT || '8082', 10);
const METRICS_PORT = parseInt(process.env.METRICS_PORT || '6061', 10);
const NUM_CORES = os.cpus().length;

const DATABASE_URL = process.env.DATABASE_URL || 'postgres://postgres:postgrespassword@127.0.0.1:5435/1Million?sslmode=disable';
const REDIS_URL = process.env.REDIS_URL || 'redis://127.0.0.1:6379';

if (cluster.isPrimary) {
	console.log(`[Node Baseline Primary] Forking across ${NUM_CORES} CPU cores on :${PORT}`);

	let acceptedWrites = 0;
	let rejectedWrites = 0;

	for (let i = 0; i < NUM_CORES; i++) {
		const worker = cluster.fork();
		worker.on('message', (msg) => {
			if (msg.type === 'metric') {
				acceptedWrites += msg.accepted || 0;
				rejectedWrites += msg.rejected || 0;
			}
		});
	}

	http.createServer((req, res) => {
		if (req.url === '/metrics') {
			res.writeHead(200, { 'Content-Type': 'application/json' });
			res.end(JSON.stringify({
				accepted_writes: acceptedWrites,
				rejected_writes: rejectedWrites,
			}));
		} else {
			res.writeHead(404);
			res.end('{}');
		}
	}).listen(METRICS_PORT, '127.0.0.1', () => {
		console.log(`[Node Baseline Primary] Metrics listening on 127.0.0.1:${METRICS_PORT}/metrics`);
	});

	cluster.on('exit', (worker, code, signal) => {
		console.log(`Worker ${worker.process.pid} died, restarting...`);
		cluster.fork();
	});

} else {
	const dbPool = new Pool({
		connectionString: DATABASE_URL,
		max: Math.max(2, Math.floor(30 / NUM_CORES)),
	});

	const redis = new Redis(REDIS_URL, {
		maxRetriesPerRequest: 1,
		lazyConnect: true,
	});
	redis.connect().catch(() => {});

	const defaultPayload = "abcdef0123456789".repeat(30);
	const memoryTable = new Map();
	for (let i = 1; i <= 100000; i++) {
		memoryTable.set(i, defaultPayload);
	}

	const writeQueue = [];
	const MAX_QUEUE = 200000 / NUM_CORES;
	let workerTotalWrites = 0;

	setInterval(async () => {
		if (writeQueue.length === 0) return;
		const batch = writeQueue.splice(0, 5000);
		try {
			if (batch.length > 0) {
				const values = [];
				const placeholders = batch.map((item, idx) => {
					values.push(item.id, item.code);
					return `($${idx * 2 + 1}, $${idx * 2 + 2})`;
				}).join(',');
				await dbPool.query(`INSERT INTO codes (id, code) VALUES ${placeholders} ON CONFLICT (id) DO NOTHING`, values);
			}
		} catch (err) {
		}
	}, 100);

	const server = http.createServer(async (req, res) => {
		const url = new URL(req.url, `http://${req.headers.host}`);
		const pathname = url.pathname;

		if (req.method === 'GET') {
			if (pathname === '/simple') {
				res.writeHead(200, { 'Content-Type': 'text/plain; charset=utf-8' });
				res.end('OK');
				return;
			}

			if (pathname === '/livez') {
				res.writeHead(200, { 'Content-Type': 'application/json' });
				res.end('{"status":"alive"}');
				return;
			}

			if (pathname === '/readyz') {
				res.writeHead(200, { 'Content-Type': 'application/json' });
				res.end('{"status":"ready"}');
				return;
			}

			if (pathname === '/code-fast') {
				const id = parseInt(url.searchParams.get('id'), 10);
				if (isNaN(id) || id <= 0) {
					res.writeHead(400, { 'Content-Type': 'application/json' });
					res.end('{"error":"invalid_id"}');
					return;
				}
				const val = memoryTable.get(id) || defaultPayload;
				res.writeHead(200, { 'Content-Type': 'application/json' });
				res.end(`{"id":${id},"code":"${val}"}`);
				return;
			}

			if (pathname === '/code-redis') {
				const id = parseInt(url.searchParams.get('id'), 10);
				if (isNaN(id) || id <= 0) {
					res.writeHead(400, { 'Content-Type': 'application/json' });
					res.end('{"error":"invalid_id"}');
					return;
				}
				try {
					const val = await redis.get(`code:${id}`);
					if (!val) {
						res.writeHead(404, { 'Content-Type': 'application/json' });
						res.end('{"error":"record_not_found"}');
						return;
					}
					res.writeHead(200, { 'Content-Type': 'application/json' });
					res.end(`{"id":${id},"code":"${val}","source":"redis"}`);
				} catch (err) {
					res.writeHead(502, { 'Content-Type': 'application/json' });
					res.end('{"error":"redis_unavailable"}');
				}
				return;
			}

			if (pathname === '/code-db') {
				const id = parseInt(url.searchParams.get('id'), 10);
				if (isNaN(id) || id <= 0) {
					res.writeHead(400, { 'Content-Type': 'application/json' });
					res.end('{"error":"invalid_id"}');
					return;
				}
				try {
					const result = await dbPool.query('SELECT code FROM codes WHERE id = $1', [id]);
					if (result.rows.length === 0) {
						res.writeHead(404, { 'Content-Type': 'application/json' });
						res.end('{"error":"record_not_found"}');
						return;
					}
					res.writeHead(200, { 'Content-Type': 'application/json' });
					res.end(`{"id":${id},"code":"${result.rows[0].code}","source":"postgresql"}`);
				} catch (err) {
					res.writeHead(500, { 'Content-Type': 'application/json' });
					res.end('{"error":"database_query_failed"}');
				}
				return;
			}
		}

		if (req.method === 'POST' && pathname === '/code-write') {
			if (writeQueue.length >= MAX_QUEUE) {
				process.send({ type: 'metric', rejected: 1 });
				res.writeHead(503, { 'Content-Type': 'application/json' });
				res.end('{"error":"write_queue_full"}');
				return;
			}

			let body = '';
			req.on('data', chunk => {
				body += chunk;
				if (body.length > 4096) {
					res.writeHead(413, { 'Content-Type': 'application/json' });
					res.end('{"error":"payload_too_large"}');
					req.destroy();
				}
			});

			req.on('end', () => {
				workerTotalWrites++;
				const id = workerTotalWrites;
				writeQueue.push({ id, code: body || defaultPayload });
				process.send({ type: 'metric', accepted: 1 });
				res.writeHead(202, { 'Content-Type': 'application/json' });
				res.end(`{"status":"queued","id":${id}}`);
			});
			return;
		}

		res.writeHead(404, { 'Content-Type': 'application/json' });
		res.end('{"error":"not_found"}');
	});

	server.listen(PORT, '0.0.0.0');
}
