package com.restartfu.offload.mobile

import java.time.Instant
import java.time.OffsetDateTime
import org.json.JSONArray
import org.json.JSONObject

data class Fleet(val version: String, val at: Instant?, val runners: List<Runner>)

data class Runner(
    val name: String,
    val url: String,
    val online: Boolean,
    val current: Boolean,
    val error: String,
    val lastSeen: Instant?,
    val status: Status?,
    val metrics: Metrics?,
    val jobs: List<Job>,
    val sessions: List<Session>,
    val github: List<Pool>,
)

data class Status(
    val id: String,
    val version: String,
    val hostname: String,
    val os: String,
    val arch: String,
    val runtime: String,
    val capCpu: Double,
    val capMem: Long,
    val allocCpu: Double,
    val allocMem: Long,
    val memAvailable: Long,
    val memReserve: Long,
    val load1: Double,
    val running: Int,
    val queued: Int,
    val sessions: Int,
    val accepting: Boolean,
    val paused: Boolean,
    val reason: String,
    val started: Instant?,
    val hostCpus: Int,
    val metrics: Boolean,
    val warm: Int,
    val images: Int,
)

data class Metrics(
    val cpus: List<Cpu>,
    val memTotal: Long,
    val workloads: List<Usage>,
    val disks: List<Disk>,
)

data class Cpu(val id: Int, val busy: Double, val reserved: Boolean, val owners: List<String>)
data class Usage(val id: String, val cpu: Double, val mem: Long)
data class Disk(val name: String, val total: Long, val free: Long)

data class GitHubJob(
    val owner: String,
    val repo: String,
    val workflow: String,
    val job: String,
    val url: String,
    val result: String,
    val runner: String,
)

data class Job(
    val id: String,
    val state: String,
    val argv: List<String>,
    val workspace: String,
    val forGitHub: Boolean,
    val github: GitHubJob?,
    val cpu: Double,
    val mem: Long,
    val created: Instant?,
    val started: Instant?,
    val finished: Instant?,
    val exitCode: Int?,
    val reason: String,
    val peakMem: Long,
    val cpuSeconds: Double,
    val caller: String,
    val runtime: String,
    val image: String,
    val cpus: String,
)

data class Session(
    val id: String,
    val profile: String,
    val state: String,
    val cpu: Double,
    val mem: Long,
    val created: Instant?,
    val finished: Instant?,
    val exitCode: Int?,
    val reason: String,
    val caller: String,
    val attached: Int,
    val runtime: String,
    val image: String,
    val peakMem: Long,
    val cpuSeconds: Double,
)

data class Pool(
    val owner: String,
    val name: String,
    val url: String,
    val labels: List<String>,
    val systemLabels: List<String>,
    val state: String,
    val detail: String,
    val capacity: Int,
    val running: Int,
    val starting: Int,
    val runs: Int,
    val lastRun: Instant?,
    val shared: Boolean,
)

data class Point(
    val t: Long,
    val cpuBusy: Double,
    val cpuUsed: Double,
    val cpuAlloc: Double,
    val memUsed: Double,
    val memWork: Double,
    val memAlloc: Double,
    val n: Int,
)

data class Series(val step: Int, val points: List<Point>)

data class LedgerRow(
    val runner: String,
    val kind: String,
    val command: String,
    val state: String,
    val exitCode: Int?,
    val oom: Boolean,
    val started: Boolean,
    val queueWait: Double,
    val duration: Double,
    val cpu: Double,
    val mem: Long,
    val peakMem: Long,
    val cpuSeconds: Double,
    val syncFiles: Int,
    val uploaded: Int?,
)

data class Ledger(val rows: List<LedgerRow>, val errors: Map<String, String>, val truncated: List<String>)

data class Info(val name: String, val version: String, val apiLevel: Int)

/** One job or session as the lists show it, with the runner it is on. */
data class Workload(
    val runner: Runner,
    val id: String,
    val kind: Kind,
    val state: String,
    val reason: String,
    val exitCode: Int?,
    val title: String,
    val repo: String,
    val caller: String,
    val cpu: Double,
    val mem: Long,
    val created: Instant?,
    val started: Instant?,
    val finished: Instant?,
    val use: Usage?,
    val cpuSeconds: Double,
    val peakMem: Long,
    val github: GitHubJob?,
    val runtime: String,
    val image: String,
    val cpus: String,
) {
    enum class Kind { JOB, SESSION, GITHUB }

    val active: Boolean get() = state !in TERMINAL
    /** Still listed, but its runner stopped answering: this is what it last said. */
    val stale: Boolean get() = active && !runner.online
    val live: Boolean get() = active && runner.online
    val apiKind: String get() = if (kind == Kind.SESSION) "session" else "job"
}

val TERMINAL = setOf("exited", "failed", "cancelled")

fun Fleet.workloads(): List<Workload> = runners.flatMap { r ->
    val use = r.metrics?.workloads.orEmpty().associateBy { it.id }
    val owners = mutableMapOf<String, MutableList<Int>>()
    r.metrics?.cpus.orEmpty().forEach { c -> c.owners.forEach { owners.getOrPut(it) { mutableListOf() }.add(c.id) } }
    r.jobs.map { j ->
        Workload(
            runner = r, id = j.id, kind = if (j.forGitHub) Workload.Kind.GITHUB else Workload.Kind.JOB,
            state = j.state, reason = j.reason, exitCode = j.exitCode,
            title = if (j.forGitHub) j.github?.job?.ifEmpty { null } ?: "Waiting for a GitHub job" else commandLine(j.argv),
            repo = if (j.forGitHub) j.github?.repo.orEmpty() else shortKey(j.workspace),
            caller = j.caller, cpu = j.cpu, mem = j.mem, created = j.created, started = j.started, finished = j.finished,
            use = use[j.id], cpuSeconds = j.cpuSeconds, peakMem = j.peakMem, github = j.github,
            runtime = j.runtime, image = j.image, cpus = j.cpus,
        )
    } + r.sessions.map { s ->
        Workload(
            runner = r, id = s.id, kind = Workload.Kind.SESSION, state = s.state, reason = s.reason, exitCode = s.exitCode,
            title = s.profile, repo = "", caller = s.caller, cpu = s.cpu, mem = s.mem,
            created = s.created, started = s.created, finished = s.finished, use = use[s.id],
            cpuSeconds = s.cpuSeconds, peakMem = s.peakMem, github = null, runtime = s.runtime, image = s.image,
            cpus = compactCpus(owners[s.id].orEmpty()),
        )
    }
}

fun shortKey(k: String): String = k.replace(Regex("--b[0-9a-f]+$"), "")

fun commandLine(argv: List<String>): String = argv.joinToString(" ") { a ->
    if (a.matches(Regex("^[\\w@%+=:,./-]+$"))) a else "'" + a.replace("'", "'\\''") + "'"
}

fun compactCpus(list: List<Int>): String {
    if (list.isEmpty()) return ""
    val a = list.sorted()
    val parts = mutableListOf<String>()
    var i = 0
    while (i < a.size) {
        var j = i
        while (j + 1 < a.size && a[j + 1] == a[j] + 1) j++
        parts += if (i == j) "${a[i]}" else "${a[i]}-${a[j]}"
        i = j + 1
    }
    return parts.joinToString(",")
}

// ---------- parsing ----------

private fun JSONObject.str(k: String) = optString(k, "").let { if (it == "null") "" else it }
private fun JSONObject.time(k: String): Instant? =
    str(k).takeIf { it.isNotEmpty() && !it.startsWith("0001-") }?.let { runCatching { OffsetDateTime.parse(it).toInstant() }.getOrNull() }
private fun JSONObject.intOrNull(k: String): Int? = if (has(k) && !isNull(k)) optInt(k) else null
private fun JSONObject.obj(k: String): JSONObject? = optJSONObject(k)
private fun JSONObject.arr(k: String): JSONArray = optJSONArray(k) ?: JSONArray()
private inline fun <T> JSONArray.map(f: (JSONObject) -> T): List<T> = (0 until length()).mapNotNull { optJSONObject(it)?.let(f) }
private fun JSONArray.strings(): List<String> = (0 until length()).map { optString(it) }

fun parseInfo(o: JSONObject) = Info(o.str("name"), o.str("version"), o.optInt("api_level"))

fun parseFleet(o: JSONObject) = Fleet(o.str("version"), o.time("at"), o.arr("runners").map(::parseRunner))

private fun parseRunner(o: JSONObject) = Runner(
    name = o.str("name"), url = o.str("url"), online = o.optBoolean("online"), current = o.optBoolean("current"),
    error = o.str("error"), lastSeen = o.time("last_seen"),
    status = o.obj("status")?.let(::parseStatus), metrics = o.obj("metrics")?.let(::parseMetrics),
    jobs = o.arr("jobs").map(::parseJob), sessions = o.arr("sessions").map(::parseSession),
    github = o.arr("github").map(::parsePool),
)

private fun parseStatus(o: JSONObject): Status {
    val cap = o.obj("capacity") ?: JSONObject()
    val alloc = o.obj("allocated") ?: JSONObject()
    return Status(
        id = o.str("id"), version = o.str("version"), hostname = o.str("hostname"), os = o.str("os"), arch = o.str("arch"),
        runtime = o.str("runtime"), capCpu = cap.optDouble("cpu", 0.0), capMem = cap.optLong("mem"),
        allocCpu = alloc.optDouble("cpu", 0.0), allocMem = alloc.optLong("mem"),
        memAvailable = o.optLong("mem_available"), memReserve = o.optLong("mem_reserve"), load1 = o.optDouble("load1", 0.0),
        running = o.optInt("running"), queued = o.optInt("queued"), sessions = o.optInt("sessions"),
        accepting = o.optBoolean("accepting"), paused = o.optBoolean("paused"), reason = o.str("reason"),
        started = o.time("started"), hostCpus = o.optInt("host_cpus"), metrics = o.optBoolean("metrics"),
        warm = o.arr("warm_keys").length(), images = o.optInt("images"),
    )
}

private fun parseMetrics(o: JSONObject) = Metrics(
    cpus = o.arr("cpus").map { Cpu(it.optInt("id"), it.optDouble("busy", 0.0), it.optBoolean("reserved"), it.arr("owners").strings()) },
    memTotal = o.optLong("mem_total"),
    workloads = o.arr("workloads").map { Usage(it.str("id"), it.optDouble("cpu", 0.0), it.optLong("mem")) },
    disks = o.arr("disk").map { Disk(it.str("name"), it.optLong("total"), it.optLong("free")) },
)

private fun parseGitHubJob(o: JSONObject) = GitHubJob(
    owner = o.str("owner"), repo = o.str("repo"), workflow = o.str("workflow"), job = o.str("job"),
    url = o.str("url"), result = o.str("result"), runner = o.str("runner"),
)

private fun parseJob(o: JSONObject): Job {
    val spec = o.obj("spec") ?: JSONObject()
    val res = o.obj("resources") ?: JSONObject()
    return Job(
        id = o.str("id"), state = o.str("state"), argv = spec.arr("argv").strings(), workspace = spec.str("workspace_key"),
        forGitHub = spec.str("github").isNotEmpty(), github = o.obj("github")?.let(::parseGitHubJob),
        cpu = res.optDouble("cpu", 0.0), mem = res.optLong("mem"),
        created = o.time("created_at"), started = o.time("started_at"), finished = o.time("finished_at"),
        exitCode = o.intOrNull("exit_code"), reason = o.str("reason"), peakMem = o.optLong("peak_mem"),
        cpuSeconds = o.optDouble("cpu_seconds", 0.0), caller = o.str("caller").ifEmpty { spec.str("client") },
        runtime = o.str("runtime"), image = o.str("image"), cpus = o.str("allowed_cpus"),
    )
}

private fun parseSession(o: JSONObject): Session {
    val res = o.obj("resources") ?: JSONObject()
    return Session(
        id = o.str("id"), profile = o.str("profile"), state = o.str("state"), cpu = res.optDouble("cpu", 0.0), mem = res.optLong("mem"),
        created = o.time("created_at"), finished = o.time("finished_at"), exitCode = o.intOrNull("exit_code"), reason = o.str("reason"),
        caller = o.str("caller").ifEmpty { o.str("client") }, attached = o.optInt("attached"), runtime = o.str("runtime"),
        image = o.str("image"), peakMem = o.optLong("peak_mem"), cpuSeconds = o.optDouble("cpu_seconds", 0.0),
    )
}

private fun parsePool(o: JSONObject) = Pool(
    owner = o.str("owner"), name = o.str("name"), url = o.str("url"), labels = o.arr("labels").strings(),
    systemLabels = o.arr("system_labels").strings(), state = o.str("state"), detail = o.str("detail"),
    capacity = o.optInt("capacity"), running = o.optInt("running"), starting = o.optInt("starting"),
    runs = o.optInt("runs"), lastRun = o.time("last_run"), shared = o.optBoolean("shared"),
)

fun parseSeries(o: JSONObject) = Series(
    step = o.optInt("step", 5),
    points = o.arr("points").map {
        Point(
            it.optLong("t"), it.optDouble("cpu_busy", 0.0), it.optDouble("cpu_used", 0.0), it.optDouble("cpu_alloc", 0.0),
            it.optDouble("mem_used", 0.0), it.optDouble("mem_work", 0.0), it.optDouble("mem_alloc", 0.0), it.optInt("n", 1).coerceAtLeast(1),
        )
    },
)

fun parseLedger(o: JSONObject): Ledger {
    val errors = o.obj("errors")?.let { e -> e.keys().asSequence().associateWith { e.str(it) } }.orEmpty()
    return Ledger(
        rows = o.arr("rows").map {
            val res = it.obj("resources") ?: JSONObject()
            LedgerRow(
                runner = it.str("runner"), kind = it.str("kind"), command = it.str("command"), state = it.str("state"),
                exitCode = it.intOrNull("exit_code"), oom = it.optBoolean("oom"), started = it.str("started_at").isNotEmpty(),
                queueWait = it.optDouble("queue_wait", 0.0), duration = it.optDouble("duration", 0.0),
                cpu = res.optDouble("cpu", 0.0), mem = res.optLong("mem"), peakMem = it.optLong("peak_mem"),
                cpuSeconds = it.optDouble("cpu_seconds", 0.0), syncFiles = it.optInt("sync_files"), uploaded = it.intOrNull("uploaded"),
            )
        },
        errors = errors,
        truncated = o.arr("truncated").strings(),
    )
}

/**
 * Sums every runner's series into one on shared step buckets. Within a runner, points in a
 * bucket are averaged weighted by how many samples each stands for. As the dashboard's combine().
 */
fun combine(series: Collection<Series>): Series? {
    val all = series.filter { it.points.isNotEmpty() }
    if (all.isEmpty()) return null
    val step = all.maxOf { it.step }.coerceAtLeast(1)
    val buckets = sortedMapOf<Long, DoubleArray>()
    for (s in all) {
        val per = mutableMapOf<Long, Pair<Int, DoubleArray>>()
        for (p in s.points) {
            val b = Math.floorDiv(p.t, step.toLong()) * step
            val (w, acc) = per[b] ?: (0 to DoubleArray(6))
            val v = doubleArrayOf(p.cpuBusy, p.cpuUsed, p.cpuAlloc, p.memUsed, p.memWork, p.memAlloc)
            for (i in 0 until 6) acc[i] += p.n * v[i]
            per[b] = (w + p.n) to acc
        }
        for ((b, wa) in per) {
            val tot = buckets.getOrPut(b) { DoubleArray(6) }
            for (i in 0 until 6) tot[i] += wa.second[i] / wa.first
        }
    }
    return Series(step, buckets.map { (t, v) -> Point(t, v[0], v[1], v[2], v[3], v[4], v[5], 1) })
}
