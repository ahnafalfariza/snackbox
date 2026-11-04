# SnackBox: promo and sales sync slice

A small runnable slice of the [SnackBox fleet design](https://claude.ai/artifact/YUoQtjmUL9DogVd781L5tN). It runs a Go cloud service and a simulator of 30 vending machines in one process. Machines go offline for minutes to days. The demo shows a timed promo reaching them, and sales syncing back with no double counting.

**Live demo:** https://snackbox-bmng.onrender.com. It is on a free tier that sleeps after 15 minutes idle, so the first load can take about a minute. Each wake-up starts a fresh simulation.

## What it shows

| Hard part | What happens in the demo | Where in the code |
|---|---|---|
| **Promos work offline** | The cloud compiles each promo into a signed 14-day price timeline per machine, in that machine's timezone. The machine only looks up a price from its own clock and never needs a connection at vend time. "20% off drinks, Fri 15:00–17:00" starts at 15:00 local time: an hour apart in Jakarta (WIB), Makassar (WITA) and Jayapura (WIT). | `internal/pricing` (compiler), `internal/pricebook` (device lookup) |
| **Desired state, not commands** | The shadow carries `desired.pricebook = {ver, sha256}`. A machine offline for 2 days jumps straight to the latest version when it reconnects. The dashboard shows coverage per promo ("27 / 30 machines hold it"). | `cloud.Store.Shadow`, `agent.syncPricebook` |
| **Exactly-once sales** | Each sale gets a per-machine `seq` and waits in a local outbox until the cloud acks `{"upto_seq": N}`. The simulated network drops 5% of requests and 8% of responses *after* the cloud has stored them, so machines retry. The cloud dedupes on `(machine_id, seq)`. The simulator keeps the ground truth and checks the cloud's counts and revenue against it every 2 s. | `cloud.Store.IngestEvents`, `agent.uploadEvents`, `sim.Fleet.Check` |
| **Rollout safety (pricebooks)** | **Publish bad pricebook** ships a validly signed pricebook that sells A1 for 0 IDR, as if the compiler had a bug. Every machine checks the signed per-SKU price bounds, refuses it, reports why, and keeps selling on its last good pricebook. **Roll forward** publishes a new, higher version (machines never accept a lower one). | `pricebook.Verify`, `pricebook.CheckBounds` |

Other details from the design that are built in:

- Ed25519 signature and sha256 checked before parsing. A machine refuses a pricebook meant for another machine, or one that is not newer than its current version.
- If the RTC has not synced for more than 7 days, or the machine is past `horizon_end`, it charges base price. Every sale records `pricebook_ver` and `promo_id`.
- Stock snapshots carry the seq they reflect, so a delayed heartbeat cannot overwrite a newer one. Offline machines show *estimated* stock: the last snapshot minus forecast sales.
- The compiler rejects discounts over 50%. Overlapping promos resolve by priority (exclusive stacking). Windows can wrap past midnight.

### Try it

1. Open the dashboard. The sim starts **Fri 13:40 WIB**, and 1 real second is 1 sim minute. Jayapura machines are already inside the promo window, Makassar enters it in about 20 s, Jakarta in about 80 s. Watch "firing now" climb.
2. `SB-ID-0004` starts offline and holds the factory pricebook, so it sells at base price. It reconnects around 15:20 WIB, jumps to the latest pricebook and starts firing the promo. `SB-ID-0009` stays offline for about 2 sim days (50 real minutes). Its outbox grows the whole time, and on reconnect "Longest upload lag" shows the backlog arriving.
3. Click any machine tile to take it offline or bring it back.
4. Click **Publish bad pricebook**, then **Roll forward**.
5. Click **Run drain test**. Sales stop, every machine comes online, and within a few seconds "Vends at machines" equals "Counted in cloud" to the rupiah. The duplicate-drop counter shows how many retries were absorbed.

## Run it

```bash
make run          # or: go run ./cmd/snackbox
```

```bash
docker compose up --build
```

Then open http://localhost:8080. Tests: `make test`. The fleet test runs 20 machines at 600× speed with 10% of requests and 20% of responses lost, drains them, and asserts the cloud total equals ground truth and that every machine converged on the latest pricebook.

Settings (environment variables): `PORT` (8080), `MACHINES` (30), `SPEED` (60).

### Deploy

`render.yaml` deploys the Dockerfile as a free Render web service: New → Blueprint → pick this repo. Any host that runs a Dockerfile and sets `PORT` works too.

## What is faked

| In this slice | In the design |
|---|---|
| Device traffic is HTTP polling every ~3 s (`/v1/machines/{id}/events`, `/heartbeat`, `/shadow`) | MQTT over mTLS on AWS IoT Core: `sb/{id}/events`, `sb/{id}/ack`, device shadow |
| In-memory store behind one mutex | Postgres, `INSERT ... ON CONFLICT (machine_id, seq, ts_device) DO NOTHING`, SQS in front |
| The machine's outbox and seq live in memory | SQLite WAL, fsynced before the vend is confirmed |
| Ed25519 key generated at boot, fetched by the simulator | KMS signing key, public key baked into the agent image |
| Machines registered by the simulator, no auth on any endpoint | X.509 per device, OIDC + roles + second approver for operators |
| Pricebooks sent as JSON | CBOR from CloudFront |
| One process runs cloud and simulator; the simulator still uses real HTTP to the cloud. Sim time is shared, and RTC skew is ±1 min per machine | Separate systems with NTP |
| The simulator posts its ground truth to the cloud (`/internal/sim-report`) so the dashboard can show the check | Real machines only report what they observe |
| Network loss, outage lengths, vend rate and restock trucks are random draws | Real modems |

## Not in this slice

- **OTA firmware.** A/B slots, trial boot, rollout rings and auto-halt are in the design but not built here. The bad-pricebook demo shows the same rule on the pricing path: the device validates and refuses, keeps last-known-good, and recovery always rolls forward.
- Planograms, the restock planner, the operator console's preview, approvals and audit log.

## What I would do next

1. **Swap the transport for MQTT + shadow on IoT Core**, keeping the message shapes. Move ingest to Postgres with the composite key and partitions from the design.
2. **Write the outbox to SQLite with fsync**, and add a power-pull test on the bench rig: kill -9 mid-vend, reboot, and check no seq is lost or reused.
3. **Coverage alerting**: page if a promo published at least 72 h ahead reaches less than 99.5% of machines by its start.
4. **Firmware rings** on IoT Jobs, reusing the halt and roll-forward rules shown here.
5. **Event-lag SLO**: alert on `ts_received - ts_device` p95 > 5 min for online machines.
6. **Settle the open questions** from the design, first of all whether a promo window should still apply on a machine that was offline when the window started.
