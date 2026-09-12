import http from 'k6/http';

const targetURL = __ENV.TARGET_URL || 'http://demo-api.optiscale-demo.svc.cluster.local:8080/';

export const options = {
  discardResponseBodies: true,
  scenarios: {
    predictable_open_arrivals: {
      executor: 'ramping-arrival-rate',
      startRate: 200,
      timeUnit: '1s',
      // At 480 arrivals/s, 400 VUs cover up to ~0.83s mean iteration latency before drops.
      preAllocatedVUs: 400,
      maxVUs: 400,
      stages: [
        // Baseline validation occurs after the first 65 seconds with OptiScale stopped.
        { duration: '65s', target: 200 },
        // Keep the calibrated healthy rate while the restarted controller collects samples.
        { duration: '45s', target: 200 },
        // Cross the measured ~447-450 RPS safe-capacity boundary smoothly before the SLO edge.
        { duration: '180s', target: 480 },
        // Preserve a post-prescale observation window at the final arrival rate.
        { duration: '45s', target: 480 },
      ],
      gracefulStop: '5s',
    },
  },
  thresholds: {
    dropped_iterations: ['count==0'],
    http_req_failed: ['rate<0.01'],
  },
};

console.log(`Open-arrival profile: 200 RPS for 110s, ramp 200->480 RPS over 180s, hold 480 RPS for 45s; URL=${targetURL}`);

export default function () {
  http.get(targetURL, { timeout: '5s' });
}
