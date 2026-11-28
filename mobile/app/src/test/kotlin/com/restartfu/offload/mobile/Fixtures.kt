package com.restartfu.offload.mobile

import java.time.Instant
import kotlin.math.PI
import kotlin.math.sin
import kotlinx.coroutines.awaitCancellation
import org.json.JSONArray
import org.json.JSONObject

/** A believable fleet of two runners, as `GET /api/fleet` returns it, at [NOW]. */
object Fixtures {
    val NOW: Instant = Instant.parse("2026-11-27T15:42:00Z")
    private const val G = 1L shl 30

    private fun at(secondsAgo: Long) = NOW.minusSeconds(secondsAgo).toString()
    private fun obj(vararg kv: Pair<String, Any?>) = JSONObject().apply { kv.forEach { (k, v) -> if (v != null) put(k, v) } }
    private fun arr(vararg v: Any) = JSONArray(v.toList())

    private fun job(
        id: String, state: String, argv: List<String>, key: String, cpu: Double, mem: Long, createdAgo: Long,
        startedAgo: Long? = null, finishedAgo: Long? = null, exit: Int? = null, reason: String? = null,
        github: JSONObject? = null, caller: String = "token", cpus: String? = null, peak: Long? = null, cpuSec: Double? = null,
    ) = obj(
        "id" to id, "state" to state,
        "spec" to obj("argv" to JSONArray(argv), "workspace_key" to key, "client" to "claude-code", "github" to github?.optString("owner")),
        "resources" to obj("cpu" to cpu, "mem" to mem), "created_at" to at(createdAgo),
        "started_at" to startedAgo?.let(::at), "finished_at" to finishedAgo?.let(::at), "exit_code" to exit, "reason" to reason,
        "github" to github, "caller" to caller, "runtime" to "container", "image" to "offload/env:3f9c2a71d0be",
        "allowed_cpus" to cpus, "peak_mem" to peak, "cpu_seconds" to cpuSec,
    )

    private fun cpus(n: Int, reserved: Int, seed: Int, owners: Map<Int, String>) = JSONArray((0 until n).map { i ->
        val busy = when {
            i >= n - reserved -> 0.08
            owners.containsKey(i) -> 0.55 + 0.4 * ((i * 37 + seed) % 10) / 10.0
            else -> ((i * 53 + seed * 7) % 10) / 40.0
        }
        obj("id" to i, "busy" to busy, "reserved" to (i >= n - reserved).takeIf { it }, "owners" to owners[i]?.let { arr(it) })
    })

    private fun pool(owner: String, running: Int, starting: Int, capacity: Int) = obj(
        "owner" to owner, "name" to "Acme Inc", "url" to "https://github.com/$owner", "scale_set" to "offload-x",
        "labels" to arr("self-hosted", "linux", "x64", "offload"), "state" to "listening", "capacity" to capacity,
        "running" to running, "starting" to starting, "runs" to 214, "last_run" to at(420), "shared" to false, "created_at" to at(86400 * 9),
    )

    fun fleet(paused: Boolean = false, github: Boolean = true, offline: Boolean = false): JSONObject {
        val ghJob = obj("owner" to "acme", "repo" to "acme/proxy", "workflow" to "ci.yml", "job" to "test (ubuntu-latest)",
            "url" to "https://github.com/acme/proxy/actions/runs/1", "runner" to "offload-fsn-build-7f1-14")
        val ghDone = obj("owner" to "acme", "repo" to "acme/proxy", "workflow" to "release.yml", "job" to "publish image",
            "result" to "succeeded", "runner" to "offload-fsn-build-7f1-13")
        val zenoJobs = JSONArray(listOf(
            job("j-4be1c07a2f", "running", listOf("cargo", "build", "--release", "-p", "bedrock-proxy"), "proxy-8d2e0f--b1c2", 6.0, 12 * G, 260, 248, cpus = "0-5"),
            job("j-91d03e55aa", "running", listOf("go", "test", "./..."), "offload-778b03--b9a1", 4.0, 6 * G, 95, 93, cpus = "6-9"),
            job("j-2c7d8e10b4", "running", listOf("/actions-runner/run.sh", "--jitconfig"), "", 2.0, 8 * G, 180, 176, github = ghJob, caller = "github"),
            job("j-77aa01f3c9", "exited", listOf("cargo", "clippy", "--all-targets"), "proxy-8d2e0f--b1c2", 4.0, 6 * G, 1900, 1898, 1720, exit = 101, peak = 3 * G, cpuSec = 410.0),
            job("j-0de4f6a811", "exited", listOf("npm", "run", "build"), "dashboard-site-11ab0c--b9a1", 2.0, 4 * G, 2400, 2399, 2280, exit = 0, peak = 2 * G, cpuSec = 190.0),
            job("j-5f2e9d0c61", "exited", listOf("/actions-runner/run.sh", "--jitconfig"), "", 2.0, 8 * G, 3000, 2990, 2600, exit = 0, github = ghDone, caller = "github", cpuSec = 520.0, peak = 5 * G),
        ))
        val twoJobs = JSONArray(listOf(
            job("j-c3a90b7e12", "queued", listOf("cargo", "test", "--workspace"), "proxy-8d2e0f--b1c2", 4.0, 8 * G, 42,
                reason = "waiting: cpu: 6.0 of 6.0 allocated and only 0.4 cores idle"),
            job("j-e81f42d903", "running", listOf("make", "-j8", "check"), "engine-5a10de--b9a1", 3.0, 5 * G, 610, 600, cpus = "0-2"),
            job("j-9a01b2c3d4", "failed", listOf("cargo", "test", "-p", "renderer"), "proxy-8d2e0f--b1c2", 2.0, 4 * G, 3300, 3290, 3100, exit = 137,
                reason = "out of memory (limit 4.0 GiB); retried with 8.0 GiB", peak = 4 * G, cpuSec = 300.0),
        ))
        val session = obj("id" to "s-6f0e1d2c3b", "profile" to "minecraft-bedrock", "state" to "running",
            "resources" to obj("cpu" to 2.0, "mem" to 6 * G), "created_at" to at(1500), "last_seen" to at(3), "attached" to 1,
            "caller" to "token", "client" to "claude-code", "runtime" to "container", "image" to "offload/env:a01f77c3e2d9")
        val zeno = obj(
            "name" to "fsn-build", "url" to "http://100.64.0.11:7420", "online" to !offline, "current" to !offline, "last_seen" to at(if (offline) 340 else 1),
            "status" to obj(
                "id" to "rz", "name" to "fsn-build", "version" to "v0.9.3", "hostname" to "fsn1-ax52", "os" to "linux", "arch" to "amd64",
                "runtime" to "container", "capacity" to obj("cpu" to 22.0, "mem" to 54 * G), "allocated" to obj("cpu" to 14.0, "mem" to 32 * G),
                "mem_available" to 31 * G, "mem_reserve" to 3 * G, "load1" to 11.2, "running" to 3, "queued" to 0, "sessions" to 1,
                "accepting" to true, "paused" to paused.takeIf { it }, "proto" to 8, "started" to at(86400 * 3 + 7200),
                "host_cpus" to 24, "metrics" to true, "warm_keys" to arr("a", "b", "c", "d"), "images" to 7,
            ),
            "metrics" to obj(
                "at" to at(1), "cpus" to cpus(24, 2, 3, (0..5).associateWith { "j-4be1c07a2f" } + (6..9).associateWith { "j-91d03e55aa" }),
                "mem_total" to 62 * G,
                "workloads" to arr(
                    obj("id" to "j-4be1c07a2f", "kind" to "job", "cpu" to 5.4, "mem" to 9 * G),
                    obj("id" to "j-91d03e55aa", "kind" to "job", "cpu" to 2.1, "mem" to (2.6 * G).toLong()),
                    obj("id" to "j-2c7d8e10b4", "kind" to "job", "cpu" to 1.3, "mem" to (3.1 * G).toLong()),
                    obj("id" to "s-6f0e1d2c3b", "kind" to "session", "cpu" to 0.9, "mem" to (5.5 * G).toLong()),
                ),
                "disk" to arr(obj("name" to "state", "total" to 400 * G, "free" to 212 * G), obj("name" to "containers", "total" to 400 * G, "free" to 212 * G)),
            ),
            "jobs" to zenoJobs, "sessions" to arr(session),
            "github" to if (github) arr(pool("acme", 1, 0, 3)) else JSONArray(),
        )
        val two = obj(
            "name" to "hel-build", "url" to "http://100.64.0.12:7420", "online" to true, "current" to true, "last_seen" to at(1),
            "status" to obj(
                "id" to "r2", "name" to "hel-build", "version" to "v0.9.3", "hostname" to "hel1-cx42", "os" to "linux", "arch" to "amd64",
                "runtime" to "container", "capacity" to obj("cpu" to 6.0, "mem" to 27 * G), "allocated" to obj("cpu" to 6.0, "mem" to (15.5 * G).toLong()),
                "mem_available" to (27.3 * G).toLong(), "mem_reserve" to 2 * G, "load1" to 4.3, "running" to 1, "queued" to 1, "sessions" to 0,
                "accepting" to true, "proto" to 8, "started" to at(86400 * 11), "host_cpus" to 8, "metrics" to true,
                "warm_keys" to arr("a", "b"), "images" to 3,
            ),
            "metrics" to obj(
                "at" to at(1), "cpus" to cpus(8, 2, 5, (0..2).associateWith { "j-e81f42d903" }), "mem_total" to 31 * G,
                "workloads" to arr(obj("id" to "j-e81f42d903", "kind" to "job", "cpu" to 2.8, "mem" to (3.9 * G).toLong())),
                "disk" to arr(obj("name" to "state", "total" to 160 * G, "free" to 22 * G)),
            ),
            "jobs" to twoJobs, "sessions" to JSONArray(),
            "github" to if (github) arr(pool("acme", 0, 0, 1)) else JSONArray(),
        )
        return obj("version" to "v0.9.3", "at" to at(0), "runners" to arr(zeno, two))
    }

    fun series(runner: String, range: String): JSONObject {
        val span = RANGES.first { it.first == range }.second
        val step = if (span <= 3600) 5L else maxOf(60L, span / 700)
        val n = (span / step).toInt()
        val big = runner == "fsn-build"
        val cap = if (big) 22.0 else 6.0
        val pts = JSONArray()
        for (i in 0 until n) {
            val t = NOW.epochSecond - (n - i) * step
            val x = i / n.toDouble()
            val wave = 0.5 + 0.35 * sin(x * 2 * PI * 2.3 + if (big) 0.0 else 1.7) + 0.1 * sin(x * 2 * PI * 11)
            val busy = cap * (0.25 + 0.55 * wave)
            pts.put(obj(
                "t" to t, "cpu_busy" to busy + 0.6, "cpu_used" to busy * 0.82, "cpu_alloc" to (cap * (0.4 + 0.45 * wave)).coerceAtMost(cap),
                "mem_used" to ((if (big) 30.0 else 14.0) + 6 * wave) * G, "mem_work" to ((if (big) 18.0 else 8.0) + 5 * wave) * G,
                "mem_alloc" to ((if (big) 26.0 else 12.0) + 5 * wave) * G, "n" to if (step > 5) 12 else 1,
            ))
        }
        return obj("step" to step, "points" to pts)
    }

    fun ledger(): JSONObject {
        val rows = JSONArray()
        val cmds = listOf(
            Triple("cargo build", 6.0, 12L), Triple("cargo test", 4.0, 8L), Triple("go test", 4.0, 6L), Triple("cargo clippy", 4.0, 6L),
            Triple("npm run", 2.0, 4L), Triple("make check", 8.0, 8L), Triple("github", 2.0, 8L),
        )
        var k = 0
        for ((ci, c) in cmds.withIndex()) {
            val runs = 18 - ci * 2
            for (r in 0 until runs) {
                k++
                val dur = 40.0 + (k * 37 % 400) + ci * 60
                val eff = if (c.first == "make check") 0.22 else 0.55 + (k % 5) * 0.08
                val failed = k % 11 == 0
                val oom = c.first == "cargo test" && r == 3
                rows.put(obj(
                    "runner" to if (k % 3 == 0) "hel-build" else "fsn-build", "id" to "j-$k", "kind" to "job", "command" to c.first,
                    "state" to if (oom) "failed" else "exited", "exit_code" to if (failed) 1 else 0, "oom" to oom.takeIf { it },
                    "created_at" to at(8000L - k * 40), "started_at" to at(7990L - k * 40), "finished_at" to at(7990L - k * 40 - dur.toLong()),
                    "queue_wait" to (k % 7) * 0.8 + 0.2, "duration" to dur, "resources" to obj("cpu" to c.second, "mem" to c.third * G),
                    "peak_mem" to (c.third * G * (if (c.first == "npm run") 0.95 else 0.6)).toLong(), "cpu_seconds" to dur * c.second * eff,
                    "sync_files" to 1400, "uploaded" to (k % 9) * 3,
                ))
            }
        }
        rows.put(obj("runner" to "fsn-build", "id" to "s-1", "kind" to "session", "command" to "minecraft-bedrock", "state" to "exited",
            "created_at" to at(5000), "finished_at" to at(1000), "duration" to 4000.0, "resources" to obj("cpu" to 2.0, "mem" to 6 * G)))
        return obj("rows" to rows)
    }

    const val OUT = """running 214 tests
test protocol::varint::roundtrip ... ok
test protocol::login::decodes_chain ... ok
test raknet::ack::coalesces_ranges ... ok
test world::chunk::subchunk_v9 ... ok
test proxy::session::forwards_transfer ... ok
test proxy::session::rewrites_start_game ... ok
test proxy::auth::rejects_expired_token ... ok
"""

    const val LOG = """   Compiling proc-macro2 v1.0.92
   Compiling unicode-ident v1.0.14
   Compiling libc v0.2.169
   Compiling serde v1.0.217
   Compiling bytes v1.9.0
   Compiling tokio v1.43.0
   Compiling bedrock-protocol v0.31.0 (/work/crates/protocol)
   Compiling raknet v0.8.2 (/work/crates/raknet)
   Compiling bedrock-world v0.12.0 (/work/crates/world)
   Compiling bedrock-proxy v0.4.0 (/work/crates/proxy)
warning: unused variable: `chunk_radius`
   --> crates/proxy/src/session.rs:214:13
    |
214 |         let chunk_radius = packet.radius;
    |             ^^^^^^^^^^^^ help: if this is intentional, prefix it with an underscore: `_chunk_radius`
    |
    = note: `#[warn(unused_variables)]` on by default

   Compiling bedrock-cli v0.4.0 (/work/crates/cli)
    Building [=======================>   ] 412/447: bedrock-proxy, bedrock-cli
"""
}

/** Answers like a dashboard would, from [Fixtures]. */
class FakeDashboard(
    var fleetJson: () -> JSONObject = { Fixtures.fleet() },
    var failWith: String? = null,
    var pairError: String? = null,
) : Dashboard {
    val stopped = mutableListOf<String>()
    var forgotten = false

    var apiLevel = API_LEVEL
    var infoCalls = 0
    override suspend fun info(): Info {
        failWith?.let { throw java.io.IOException(it) }
        infoCalls++
        return Info("laptop", "v0.9.3", apiLevel)
    }
    override suspend fun fleet(): Fleet {
        failWith?.let { throw java.io.IOException(it) }
        return parseFleet(fleetJson())
    }
    override suspend fun series(runner: String, range: String) = parseSeries(Fixtures.series(runner, range))
    override suspend fun ledger(sinceRfc3339: String) = parseLedger(Fixtures.ledger())
    override suspend fun stop(runner: String, kind: String, id: String) { stopped += "$runner/$id" }
    override suspend fun pause(runner: String, paused: Boolean) {}
    override suspend fun githubRemove(owner: String, forget: Boolean) = listOf("fsn-build" to "", "hel-build" to "")
    override suspend fun githubAdd(add: GitHubAdd) = add.on.map { it to "" }
    override suspend fun forgetThisDevice() {
        failWith?.let { throw java.io.IOException(it) }
        forgotten = true
    }
    override suspend fun logs(runner: String, kind: String, id: String, stream: String, onText: (String) -> Unit) {
        onText(if (kind == "session") "[mcp] listening on stdio\n[mcp] tools: launch, join, screenshot, chat\n[client] joined play.example.net:19132\n" else if (stream == "stderr") Fixtures.LOG else Fixtures.OUT)
        awaitCancellation()
    }
}
