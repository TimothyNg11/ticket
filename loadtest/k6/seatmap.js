// Seat-map read throughput: how many seat-map reads per second the deployment
// serves, and at what latency. Requests arrive at a fixed rate per stage
// (constant-arrival-rate), so a slow server can't hide by slowing the client
// down: if responses lag, k6 starts more virtual users to keep the rate.
//
// Run in-cluster via loadtest/k6-job.sh. EVENT_ID must be a published event.
import http from "k6/http";
import { check } from "k6";

const BASE = __ENV.BASE || "http://traefik.traefik.svc.cluster.local";
const EVENT = __ENV.EVENT_ID;
const STEP = __ENV.STEP || "45s";

export const options = {
  discardResponseBodies: true,
  scenarios: Object.fromEntries(
    [200, 500, 1000, 1500].map((rate, i) => [
      `r${rate}`,
      {
        executor: "constant-arrival-rate",
        rate,
        timeUnit: "1s",
        duration: STEP,
        startTime: `${i * (parseInt(STEP) + 5)}s`,
        preAllocatedVUs: Math.max(50, rate / 4),
        maxVUs: rate * 2,
        tags: { stage: `${rate}rps` },
      },
    ]),
  ),
  // One threshold per stage so the summary shows p99 at every load level; the
  // 300 ms target is the spec's example, adopted after the first baseline.
  thresholds: Object.fromEntries(
    [200, 500, 1000, 1500].flatMap((r) => [
      [`http_req_duration{stage:${r}rps}`, ["p(99)<300"]],
      [`http_req_failed{stage:${r}rps}`, ["rate<0.01"]],
    ]),
  ),
  summaryTrendStats: ["p(50)", "p(95)", "p(99)", "max"],
};

export default function () {
  const res = http.get(`${BASE}/v1/events/${EVENT}/seatmap`, {
    headers: { "Accept-Encoding": "gzip" },
  });
  check(res, { "status 200": (r) => r.status === 200 });
}
