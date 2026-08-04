// k6 scenario for private-isu: simulates a typical logged-in user journey
// (login -> timeline -> post detail -> user page). Run against the target:
//
//   k6 run -e BASE=http://localhost --vus 20 --duration 60s examples/k6-private-isu.js
//
// k6 handles cookies like a browser, so the login session persists across
// the scenario without extra code. Combine with isutools:
//   curl -X POST $ADMIN/reset && k6 run ... && curl -X POST "$ADMIN/save?score=-"
// The dashboard then shows the server-side view (SQL/HTTP/flows) of exactly
// this scenario, and the User Flow section shows the simulated journeys.
import http from "k6/http";
import { check, sleep } from "k6";

const BASE = __ENV.BASE || "http://localhost";

export const options = {
  scenarios: {
    browsing: {
      executor: "constant-vus",
      vus: Number(__ENV.VUS || 10),
      duration: __ENV.DURATION || "60s",
    },
  },
};

export default function () {
  // 1. login (accounts from the initial dataset)
  const n = 1 + (__VU % 999);
  const account = `user${String(n).padStart(4, "0")}`;
  // Synthetic, non-secret story labels. One ID per iteration keeps repeated
  // VU journeys separate. The authentication cookie is deliberately not logged.
  const params = { headers: {
    "X-Isutools-Session": `k6-vu-${__VU}-iter-${__ITER}`,
    "X-Isutools-Scenario": "login_and_browse",
  } };
  let res = http.post(`${BASE}/login`, {
    account_name: account,
    password: account,
  }, params);
  check(res, { "login succeeded": (r) => r.status === 200 });

  // 2. timeline
  res = http.get(`${BASE}/`, params);
  check(res, { "timeline 200": (r) => r.status === 200 });
  sleep(0.5);

  // 3. one post detail (follow a link the way a reader would)
  const m = res.body && res.body.match(/href="\/posts\/(\d+)"/);
  if (m) {
    res = http.get(`${BASE}/posts/${m[1]}`, params);
    check(res, { "post detail 200": (r) => r.status === 200 });
    sleep(0.5);
  }

  // 4. an author page
  const u = res.body && res.body.match(/href="\/@([0-9a-zA-Z_]+)"/);
  if (u) {
    res = http.get(`${BASE}/@${u[1]}`, params);
    check(res, { "user page 200": (r) => r.status === 200 });
  }
  sleep(1);
}
