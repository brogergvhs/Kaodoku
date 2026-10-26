import http from 'k6/http';
import { check, sleep } from 'k6';

const BASE_URL = __ENV.BASE_URL || 'http://127.0.0.1:8090';
const ENDPOINTS = ['GET /library', 'GET /reader/titles/:id', 'GET /manifest', 'GET /pages/:n', 'POST /progress/batch'];

export const options = {
  discardResponseBodies: true,
  scenarios: {
    readers: {
      executor: 'ramping-vus',
      stages: [
        { duration: __ENV.RAMP_UP || '30s', target: Number(__ENV.VUS || 50) },
        { duration: __ENV.DURATION || '5m', target: Number(__ENV.VUS || 50) },
        { duration: '15s', target: 0 },
      ],
    },
  },
  thresholds: {
    http_req_duration: ['p(95)<500', 'p(99)<1000'],
    http_req_failed: ['rate<0.01'],
    ...Object.fromEntries(ENDPOINTS.map((n) => [`http_req_duration{name:${n}}`, ['p(95)<500', 'p(99)<1000']])),
  },
  summaryTrendStats: ['avg', 'med', 'p(95)', 'p(99)', 'max'],
};

const between = (lo, hi) => lo + Math.random() * (hi - lo);
const pick = (a) => a[Math.floor(Math.random() * a.length)];

export function setup() {
  if (!__ENV.PASSWORD) return {};
  const body = JSON.stringify({ username: __ENV.USERNAME, password: __ENV.PASSWORD, device_name: 'k6', device_id: 'k6' });
  const r = http.post(`${BASE_URL}/api/v1/auth/login`, body, { headers: { 'Content-Type': 'application/json' }, responseType: 'text' });
  if (r.status !== 200) throw new Error(`login ${r.status}: ${r.body}`);
  return { Authorization: `Bearer ${r.json('token')}` };
}

export default function (headers) {
  const json = (path, name) => http.get(`${BASE_URL}${path}`, { headers, tags: { name }, responseType: 'text' });

  const lib = json('/api/v1/library?limit=50', 'GET /library');
  if (!check(lib, { 'library 200': (r) => r.status === 200 })) return sleep(between(3, 7));
  const title = pick(lib.json('items'));
  sleep(between(2, 5));

  const detail = json(`/api/v1/reader/titles/${title.id}`, 'GET /reader/titles/:id');
  if (!check(detail, { 'title 200': (r) => r.status === 200 })) return sleep(between(3, 7));
  const chapterID = pick(detail.json('chapters')).id;
  sleep(between(1, 3));

  const manifest = json(`/api/v1/reader/titles/${title.id}/manifest?chapter=${chapterID}`, 'GET /manifest');
  if (!check(manifest, { 'manifest 200': (r) => r.status === 200 })) return sleep(between(3, 7));
  const pages = manifest.json('chapters').find((c) => c.id === chapterID).pages;

  for (const p of pages) {
    check(http.get(`${BASE_URL}${p.url}`, { headers, tags: { name: 'GET /pages/:n' } }), { 'page 200': (r) => r.status === 200 });
    sleep(between(2, 6));
  }

  const entries = pages.map((p) => ({ chapter_id: chapterID, page: p.page, total_pages: pages.length }));
  const marked = http.post(`${BASE_URL}/api/v1/reader/progress/batch`, JSON.stringify({ entries }), {
    headers: { ...headers, 'Content-Type': 'application/json' },
    tags: { name: 'POST /progress/batch' },
    responseType: 'text',
  });
  check(marked, { 'progress applied': (r) => r.status === 200 && r.json('applied') === entries.length });
  sleep(between(5, 15));
}
