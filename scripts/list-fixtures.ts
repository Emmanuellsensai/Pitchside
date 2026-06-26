/**
 * Touchline fixtures lister (devnet).
 *
 * Loads the proxy environment (proxy/.env), calls the TxLINE fixtures snapshot
 * endpoint on the devnet base with our guest JWT and API token, and prints recent
 * World Cup fixtureIds with team names, competition, and kickoff time, sorted
 * newest first. Use it to pick a fixtureId that has already finished and so still
 * sits inside the historical replay window.
 *
 * Run: npx ts-node list-fixtures.ts            (all bundle fixtures)
 *      npx ts-node list-fixtures.ts --all      (do not filter to World Cup)
 *
 * The snapshot path is not pinned in CLAUDE.md, so we try the documented data
 * paths first and fall back through a short list of candidates, using whichever
 * returns a fixtures array. Field names follow the IDL Fixture struct, which uses
 * snake_case, but we also accept camelCase in case the HTTP layer renames them.
 */

import * as fs from "fs";
import * as path from "path";

// Verified devnet default (see CLAUDE.md). Overridable via proxy/.env TXLINE_BASE.
const DEFAULT_BASE = "https://txline-dev.txodds.com";

// Candidate snapshot paths, tried in order. The first that returns a non-empty
// fixtures array wins. CLAUDE.md only documents /api/scores/stream and
// /api/scores/historical/{id}, so the snapshot path is a best effort.
const CANDIDATE_PATHS = [
  "/api/scores/snapshot",
  "/api/scores/fixtures",
  "/api/fixtures",
  "/api/fixtures/snapshot",
  "/api/scores/fixtures/snapshot",
];

const ENV_PATH = path.join(__dirname, "..", "proxy", ".env");

/** Minimal .env reader: KEY=VALUE per line, ignores blanks and # comments. */
function loadEnv(file: string): Record<string, string> {
  if (!fs.existsSync(file)) {
    throw new Error(
      `No env file at ${file}. Copy proxy/.env.example to proxy/.env and fill ` +
        `in TXLINE_JWT and TXLINE_API_TOKEN (printed by scripts/subscribe.ts).`
    );
  }
  const env: Record<string, string> = {};
  for (const line of fs.readFileSync(file, "utf8").split("\n")) {
    const trimmed = line.trim();
    if (!trimmed || trimmed.startsWith("#")) continue;
    const eq = trimmed.indexOf("=");
    if (eq === -1) continue;
    const key = trimmed.slice(0, eq).trim();
    let val = trimmed.slice(eq + 1).trim();
    if (
      (val.startsWith('"') && val.endsWith('"')) ||
      (val.startsWith("'") && val.endsWith("'"))
    ) {
      val = val.slice(1, -1);
    }
    env[key] = val;
  }
  return env;
}

/** Pull a value off an object trying several key spellings. */
function pick(obj: any, keys: string[]): any {
  for (const k of keys) {
    if (obj != null && obj[k] != null) return obj[k];
  }
  return undefined;
}

/** Find the fixtures array inside whatever shape the endpoint returns. */
function extractFixtures(body: any): any[] {
  if (Array.isArray(body)) return body;
  const candidates = [
    body?.fixtures,
    body?.data?.fixtures,
    body?.data,
    body?.snapshot?.fixtures,
    body?.result,
    body?.results,
  ];
  for (const c of candidates) {
    if (Array.isArray(c)) return c;
  }
  return [];
}

/** Normalise epoch seconds or milliseconds to a Date. */
function toDate(raw: any): Date | null {
  if (raw == null) return null;
  let n = typeof raw === "string" ? Number(raw) : raw;
  if (typeof n !== "number" || !isFinite(n)) {
    const d = new Date(raw);
    return isNaN(d.getTime()) ? null : d;
  }
  // i64 epoch. Below ~1e12 it is seconds, otherwise milliseconds.
  if (n < 1e12) n *= 1000;
  return new Date(n);
}

interface Row {
  fixtureId: string;
  home: string;
  away: string;
  competition: string;
  kickoff: Date | null;
}

function normalise(f: any): Row {
  const fixtureId = pick(f, ["FixtureId", "fixture_id", "fixtureId", "id"]);
  const competition = pick(f, [
    "Competition",
    "competition",
    "competition_name",
    "competitionName",
  ]);
  const start = pick(f, [
    "StartTime",
    "start_time",
    "startTime",
    "Ts",
    "ts",
    "kickoff",
    "kick_off",
  ]);
  const p1 = pick(f, ["Participant1", "participant1", "participant1_name", "home", "homeName"]);
  const p2 = pick(f, ["Participant2", "participant2", "participant2_name", "away", "awayName"]);
  const p1Home = pick(f, ["Participant1IsHome", "participant1_is_home", "participant1IsHome"]);

  // participant1 is home by the brief's convention, but honour the flag if present.
  const home = p1Home === false ? p2 : p1;
  const away = p1Home === false ? p1 : p2;

  return {
    fixtureId: fixtureId != null ? String(fixtureId) : "(unknown)",
    home: home != null ? String(home) : "?",
    away: away != null ? String(away) : "?",
    competition: competition != null ? String(competition) : "?",
    kickoff: toDate(start),
  };
}

function isWorldCup(row: Row): boolean {
  return /world\s*cup/i.test(row.competition);
}

async function tryFetch(base: string, p: string, jwt: string, apiToken: string) {
  const url = base + p;
  const res = await fetch(url, {
    method: "GET",
    headers: {
      Authorization: `Bearer ${jwt}`,
      "X-Api-Token": apiToken,
      Accept: "application/json",
    },
  });
  const text = await res.text();
  let body: any;
  try {
    body = text ? JSON.parse(text) : undefined;
  } catch {
    body = text;
  }
  return { status: res.status, ok: res.ok, body, raw: text };
}

function pad(s: string, n: number): string {
  return s.length >= n ? s : s + " ".repeat(n - s.length);
}

/** Find the events array inside the historical endpoint's response shape. */
function extractEvents(body: any): any[] {
  if (Array.isArray(body)) return body;
  const candidates = [body?.data, body?.updates, body?.scores, body?.history, body?.events];
  for (const c of candidates) {
    if (Array.isArray(c)) return c;
  }
  return [];
}

interface HistorySummary {
  events: number;
  withStats: number;
  homeCorners: number;
  awayCorners: number;
}

/**
 * Summarise one fixture's history. Counts events, and tallies corners using the
 * same rule the game uses: strip the period (key % 1000), base 7 is a home
 * corner and base 8 an away corner, and a corner resolves each time that running
 * total increases. Tracking the max per side lands on the match total even
 * though the feed also carries per-period counters. Stats is an object map.
 */
function summariseHistory(events: any[]): HistorySummary {
  let withStats = 0;
  let maxHome = 0;
  let maxAway = 0;
  let homeCorners = 0;
  let awayCorners = 0;
  for (const e of events) {
    const stats = e?.Stats ?? e?.stats;
    if (!stats || typeof stats !== "object") continue;
    let any = false;
    for (const k in stats) {
      const raw = stats[k];
      const value =
        raw != null && typeof raw === "object" ? raw.value ?? raw.Value ?? raw.v : raw;
      const num = Number(value);
      if (!isFinite(num)) continue;
      any = true;
      const base = Number(k) % 1000;
      if (base === 7 && num > maxHome) {
        homeCorners += num - maxHome;
        maxHome = num;
      } else if (base === 8 && num > maxAway) {
        awayCorners += num - maxAway;
        maxAway = num;
      }
    }
    if (any) withStats++;
  }
  return { events: events.length, withStats, homeCorners, awayCorners };
}

async function fetchHistory(
  base: string,
  fixtureId: string,
  jwt: string,
  apiToken: string
): Promise<any[]> {
  const url = `${base}/api/scores/historical/${encodeURIComponent(fixtureId)}`;
  const res = await fetch(url, {
    method: "GET",
    headers: {
      Authorization: `Bearer ${jwt}`,
      "X-Api-Token": apiToken,
      Accept: "application/json",
    },
  });
  if (!res.ok) throw new Error(`HTTP ${res.status}`);
  const text = await res.text();
  let body: any;
  try {
    body = text ? JSON.parse(text) : undefined;
  } catch {
    return [];
  }
  return extractEvents(body);
}

async function main() {
  const showAll = process.argv.includes("--all");
  const checkHistory = process.argv.includes("--check");

  const env = loadEnv(ENV_PATH);
  const base = (env.TXLINE_BASE || DEFAULT_BASE).replace(/\/+$/, "");
  const jwt = env.TXLINE_JWT;
  const apiToken = env.TXLINE_API_TOKEN;

  if (!jwt || jwt.startsWith("<") || !apiToken || apiToken.startsWith("<")) {
    throw new Error(
      "proxy/.env is missing TXLINE_JWT or TXLINE_API_TOKEN. Run " +
        "scripts/subscribe.ts and paste the two printed values in."
    );
  }

  console.log("Base:", base);
  console.log("Looking for the fixtures snapshot endpoint...\n");

  let fixtures: any[] = [];
  let usedPath = "";
  for (const p of CANDIDATE_PATHS) {
    let resp;
    try {
      resp = await tryFetch(base, p, jwt, apiToken);
    } catch (e: any) {
      console.log(`  ${pad(p, 34)} request error: ${e?.message ?? e}`);
      continue;
    }
    const found = resp.ok ? extractFixtures(resp.body) : [];
    console.log(
      `  ${pad(p, 34)} ${resp.status} ${
        resp.ok ? `(${found.length} fixtures)` : resp.raw.slice(0, 80)
      }`
    );
    if (resp.ok && found.length) {
      fixtures = found;
      usedPath = p;
      break;
    }
  }

  if (!fixtures.length) {
    throw new Error(
      "No fixtures returned from any candidate snapshot path. The endpoint may " +
        "differ on this deployment, or the bundle currently has no fixtures. " +
        "Check the TxLINE docs for the snapshot route and add it to CANDIDATE_PATHS."
    );
  }

  console.log(`\nUsing ${usedPath}, got ${fixtures.length} fixtures.\n`);

  let rows = fixtures.map(normalise);
  if (!showAll) {
    const wc = rows.filter(isWorldCup);
    // Only narrow if the filter actually matched something, otherwise show all
    // so an unexpected competition label does not hide everything.
    if (wc.length) {
      rows = wc;
    } else {
      console.log("No competition matched /world cup/i, showing the full bundle.\n");
    }
  }

  // Newest first by kickoff. Unknown kickoffs sink to the bottom.
  rows.sort((a, b) => {
    const ta = a.kickoff ? a.kickoff.getTime() : -Infinity;
    const tb = b.kickoff ? b.kickoff.getTime() : -Infinity;
    return tb - ta;
  });

  const now = Date.now();
  const idW = Math.max(9, ...rows.map((r) => r.fixtureId.length));
  console.log(
    `${pad("fixtureId", idW)}  ${pad("kickoff (UTC)", 20)}  ${pad("status", 8)}  match / competition`
  );
  console.log("-".repeat(idW + 2 + 20 + 2 + 8 + 2 + 40));
  for (const r of rows) {
    const ko = r.kickoff ? r.kickoff.toISOString().slice(0, 16).replace("T", " ") : "?";
    const status = r.kickoff
      ? r.kickoff.getTime() < now
        ? "finished"
        : "upcoming"
      : "?";
    console.log(
      `${pad(r.fixtureId, idW)}  ${pad(ko, 20)}  ${pad(status, 8)}  ` +
        `${r.home} v ${r.away}  (${r.competition})`
    );
  }

  console.log(
    `\n${rows.length} fixtures shown. Pick a "finished" fixtureId for replay, then run:\n` +
      `  open http://localhost:8080/?fixtureId=<id>  (replay mode)`
  );

  if (!checkHistory) {
    console.log(`\nAdd --check to probe which finished fixtures have replay data.`);
    return;
  }

  // Probe the historical endpoint for each finished fixture and report counts
  // only, so we can pick one with enough corners to demo. No payloads printed.
  const finished = rows.filter((r) => r.kickoff && r.kickoff.getTime() < now);
  console.log(`\nChecking historical replay data for ${finished.length} finished fixtures...\n`);
  if (!finished.length) {
    console.log("None finished yet, nothing to probe.");
    return;
  }

  console.log(
    `${pad("fixtureId", idW)}  ${pad("events", 7)}  ${pad("withStats", 9)}  ${pad("corners H/A", 11)}  match`
  );
  console.log("-".repeat(idW + 2 + 7 + 2 + 9 + 2 + 11 + 2 + 30));
  for (const r of finished) {
    let events: any[] = [];
    let err = "";
    try {
      events = await fetchHistory(base, r.fixtureId, jwt, apiToken);
    } catch (e: any) {
      err = e?.message ?? String(e);
    }
    if (err) {
      console.log(`${pad(r.fixtureId, idW)}  ${pad("ERR", 7)}  ${err}`);
      continue;
    }
    const s = summariseHistory(events);
    const cornersTotal = s.homeCorners + s.awayCorners;
    const note = events.length === 0 ? "(no replay data)" : "";
    console.log(
      `${pad(r.fixtureId, idW)}  ${pad(String(s.events), 7)}  ${pad(String(s.withStats), 9)}  ` +
        `${pad(`${s.homeCorners}/${s.awayCorners} (${cornersTotal})`, 11)}  ${r.home} v ${r.away} ${note}`
    );
  }

  console.log(
    `\nPick a fixtureId with the most corners (H/A total) for the demo, then run:\n` +
      `  open http://localhost:8080/?fixtureId=<id>  (replay mode)`
  );
}

main().catch((err) => {
  console.error("\nlist-fixtures failed:", err?.message ?? err);
  process.exit(1);
});
