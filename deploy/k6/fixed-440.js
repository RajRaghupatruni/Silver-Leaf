import http from "k6/http";

export const options = {
  scenarios: {
    fixed: {
      executor: "constant-arrival-rate",
      rate: 440,
      timeUnit: "1s",
      duration: "90s",
      preAllocatedVUs: 300,
      maxVUs: 600,
    },
  },
  thresholds: {
    http_req_failed: ["rate<0.01"],
    dropped_iterations: ["count==0"],
  },
};

export default function () {
  http.get("http://demo-api.optiscale-demo.svc.cluster.local:8080/");
}
