// k6 load test: simulates viewers watching live channels like a real HLS player.
// Each virtual user refreshes the playlist every 4 s and downloads only NEW segments.
// Run on the cicd VM:
//   docker run --rm -i -e VUS=100 grafana/k6:1.0.0 run - < loadtest/hls-viewers.js
import http from 'k6/http';
import { check, sleep } from 'k6';
import { Rate, Trend, Counter } from 'k6/metrics';

const BASE = __ENV.BASE || 'http://10.20.30.220';
const HOST = __ENV.HOST_HEADER || 'tv.lab.local';
const CHANNELS = (__ENV.CHANNELS || 'live,bunny,tears').split(',');
const VUS = Number(__ENV.VUS || 100);
const params = { headers: { Host: HOST } };

const cacheHit = new Rate('edge_cache_hit');
const segTime = new Trend('segment_download_ms', true);
const videoBytes = new Counter('video_bytes');

export const options = {
  stages: [
    { duration: '1m', target: Math.ceil(VUS / 4) }, // warm up
    { duration: '2m', target: VUS },                // ramp to full load
    { duration: '3m', target: VUS },                // hold
    { duration: '1m', target: 0 },                  // ramp down
  ],
  thresholds: {
    http_req_failed: ['rate<0.01'],          // < 1% errors
    segment_download_ms: ['p(95)<2000'],     // 95% of segments in < 2 s
  },
};

export default function () {
  const ch = CHANNELS[(__VU - 1) % CHANNELS.length];
  const base = `${BASE}/live/${ch}/`;
  const seen = {};

  for (let i = 0; i < 15; i++) {                  // ~1 minute of watching
    const pl = http.get(base + 'index.m3u8', Object.assign({ tags: { kind: 'playlist' } }, params));
    check(pl, { 'playlist 200': (r) => r.status === 200 });

    if (pl.status === 200 && pl.body) {
      const segs = pl.body.split('\n').map((l) => l.trim()).filter((l) => l.endsWith('.ts'));
      for (const s of segs.slice(-2)) {            // a player only fetches the newest pieces
        if (seen[s]) continue;
        seen[s] = true;
        const r = http.get(base + s, Object.assign({ responseType: 'none', tags: { kind: 'segment' } }, params));
        check(r, { 'segment 200': (x) => x.status === 200 });
        segTime.add(r.timings.duration);
        cacheHit.add(r.headers['X-Cache-Status'] === 'HIT');
        videoBytes.add(Number(r.headers['Content-Length'] || 0));
      }
    }
    sleep(4);
  }
}
