package com.restartfu.offload.mobile.ui

import androidx.compose.foundation.Canvas
import androidx.compose.foundation.background
import androidx.compose.foundation.clickable
import androidx.compose.foundation.layout.Arrangement
import androidx.compose.foundation.layout.Box
import androidx.compose.foundation.layout.Column
import androidx.compose.foundation.layout.Row
import androidx.compose.foundation.layout.Spacer
import androidx.compose.foundation.layout.fillMaxSize
import androidx.compose.foundation.layout.fillMaxWidth
import androidx.compose.foundation.layout.height
import androidx.compose.foundation.layout.heightIn
import androidx.compose.foundation.layout.padding
import androidx.compose.foundation.layout.size
import androidx.compose.foundation.layout.width
import androidx.compose.foundation.lazy.LazyColumn
import androidx.compose.foundation.lazy.items
import androidx.compose.foundation.shape.RoundedCornerShape
import androidx.compose.material3.CircularProgressIndicator
import androidx.compose.material3.MaterialTheme
import androidx.compose.material3.OutlinedButton
import androidx.compose.material3.Text
import androidx.compose.runtime.Composable
import androidx.compose.runtime.LaunchedEffect
import androidx.compose.ui.Alignment
import androidx.compose.ui.Modifier
import androidx.compose.ui.geometry.CornerRadius
import androidx.compose.ui.geometry.Offset
import androidx.compose.ui.geometry.Size
import androidx.compose.ui.graphics.Color
import androidx.compose.ui.graphics.Path
import androidx.compose.ui.graphics.PathEffect
import androidx.compose.ui.graphics.StrokeCap
import androidx.compose.ui.graphics.StrokeJoin
import androidx.compose.ui.graphics.drawscope.Stroke
import androidx.compose.ui.semantics.contentDescription
import androidx.compose.ui.semantics.semantics
import androidx.compose.ui.text.TextStyle
import androidx.compose.ui.text.drawText
import androidx.compose.ui.text.font.FontWeight
import androidx.compose.ui.text.rememberTextMeasurer
import androidx.compose.ui.unit.dp
import androidx.compose.ui.unit.sp
import com.restartfu.offload.mobile.AppModel
import com.restartfu.offload.mobile.Fleet
import com.restartfu.offload.mobile.Link
import com.restartfu.offload.mobile.Page
import com.restartfu.offload.mobile.RANGES
import com.restartfu.offload.mobile.Runner
import com.restartfu.offload.mobile.Series
import com.restartfu.offload.mobile.bytes
import com.restartfu.offload.mobile.commandLine
import com.restartfu.offload.mobile.cores
import com.restartfu.offload.mobile.duration
import com.restartfu.offload.mobile.pct
import com.restartfu.offload.mobile.plural
import com.restartfu.offload.mobile.secondsSince
import java.time.Instant
import java.time.ZoneId
import java.time.format.DateTimeFormatter
import kotlin.math.ceil
import kotlin.math.floor
import kotlin.math.log10
import kotlin.math.pow
import kotlinx.coroutines.delay

val RANGE_LABELS = mapOf("1h" to "1 hour", "6h" to "6 hours", "24h" to "24 hours", "7d" to "7 days")

@Composable
fun OverviewPage(model: AppModel) {
    val fleet = model.fleet
    LaunchedEffect(model.range, fleet != null) {
        if (fleet == null) return@LaunchedEffect
        while (true) {
            model.loadSeries()
            delay(if (model.range == "1h") 5_000 else 30_000)
        }
    }
    if (fleet == null) {
        Waiting(model)
        return
    }
    if (fleet.runners.isEmpty()) {
        Empty("No runners yet", "The dashboard keeps looking on the tailnet. Install one on a server with offload runner install.")
        return
    }
    val now = rememberNow(model)
    val t = totals(fleet)
    LazyColumn(Modifier.fillMaxSize(), contentPadding = pagePadding(), verticalArrangement = Arrangement.spacedBy(12.dp)) {
        val alerts = alerts(fleet, model, now)
        if (alerts.isNotEmpty()) {
            item { Column { alerts.forEach { (m, text) -> Notice(text, m) } } }
        }
        item {
            Panel {
                Text("Free cores", style = MaterialTheme.typography.labelMedium, color = MaterialTheme.colorScheme.onSurfaceVariant)
                Row(verticalAlignment = Alignment.Bottom) {
                    Text(cores(t.freeCpu), style = MaterialTheme.typography.displayMedium.copy(fontWeight = FontWeight.SemiBold))
                    Text(" of ${cores(t.capCpu)}", style = MaterialTheme.typography.titleMedium, color = MaterialTheme.colorScheme.onSurfaceVariant, modifier = Modifier.padding(bottom = 8.dp))
                }
                Spacer(Modifier.height(8.dp))
                Meter(if (t.capCpu > 0) t.allocCpu / t.capCpu else 0.0)
                Spacer(Modifier.height(8.dp))
                Text(
                    "${cores(t.allocCpu)} cores guaranteed to running work" + (t.busy?.let { " · ${cores(it)} busy right now" } ?: ""),
                    style = MaterialTheme.typography.bodySmall, color = MaterialTheme.colorScheme.onSurfaceVariant,
                )
            }
        }
        item {
            Row(horizontalArrangement = Arrangement.spacedBy(12.dp)) {
                Stat("Memory free", bytes(t.freeMem), "of ${bytes(t.capMem)} not booked", Modifier.weight(1f))
                Stat("Running", "${t.running}", "${t.queued} queued · ${plural(t.sessions, "session")}", Modifier.weight(1f))
            }
        }
        item {
            Segmented(RANGES.map { it.first to it.first }, model.range, model::chooseRange, Modifier.padding(top = 8.dp))
        }
        item { UsageCharts(model, fleet, now) }
        item { SectionTitle("Runners", "${t.online} of ${t.runners} answering") }
        items(fleet.runners, key = { "r:" + it.name }) { RunnerCard(it, model, now) }
        val waiting = fleet.runners.filter { it.online }.flatMap { r -> r.jobs.filter { it.state == "queued" || it.state == "uploading" }.map { r to it } }
            .sortedBy { it.second.created }
        if (waiting.isNotEmpty()) {
            item { SectionTitle("Waiting", "${plural(waiting.size, "job")} not started yet") }
            items(waiting, key = { "w:" + it.first.name + it.second.id }) { (r, j) ->
                Panel(onClick = { model.open(Page.Detail(r.name, j.id)) }) {
                    Text(commandLine(j.argv), style = MaterialTheme.typography.bodyMedium.copy(fontFamily = Mono, fontSize = 13.sp), maxLines = 2)
                    Spacer(Modifier.height(6.dp))
                    StatusLabel(Mark.WAITING, "Waiting ${duration(secondsSince(j.created, now))} on ${r.name}")
                    if (j.reason.isNotEmpty()) {
                        Spacer(Modifier.height(4.dp))
                        Text(j.reason.removePrefix("waiting: "), style = MaterialTheme.typography.bodySmall, color = MaterialTheme.colorScheme.onSurfaceVariant)
                    }
                }
            }
        }
    }
}

/** Before the first answer: a spinner, or why there is none and a way to try again. */
@Composable
fun Waiting(model: AppModel) {
    if (model.link is Link.Down) {
        Empty(
            "Can't reach ${model.pairing?.name ?: "your computer"}",
            "Check that offload dashboard is running there and that Tailscale is on, on both devices.",
        ) {
            OutlinedButton(onClick = { model.retry() }, shape = RoundedCornerShape(12.dp), modifier = Modifier.heightIn(min = 48.dp)) { Text("Try again") }
        }
    } else {
        Loading()
    }
}

@Composable
fun Loading() {
    Box(Modifier.fillMaxSize(), contentAlignment = Alignment.Center) {
        CircularProgressIndicator(Modifier.size(28.dp), strokeWidth = 2.5.dp)
    }
}

data class Totals(
    val runners: Int, val online: Int, val capCpu: Double, val allocCpu: Double, val busy: Double?,
    val capMem: Long, val allocMem: Long, val running: Int, val queued: Int, val sessions: Int,
) {
    val freeCpu get() = (capCpu - allocCpu).coerceAtLeast(0.0)
    val freeMem get() = (capMem - allocMem).coerceAtLeast(0)
}

fun totals(f: Fleet): Totals {
    val on = f.runners.filter { it.online && it.status != null }
    val st = on.map { it.status!! }
    return Totals(
        runners = f.runners.size, online = on.size, capCpu = st.sumOf { it.capCpu }, allocCpu = st.sumOf { it.allocCpu },
        busy = if (on.any { it.metrics != null }) on.sumOf { r -> r.metrics?.cpus.orEmpty().sumOf { it.busy } } else null,
        capMem = st.sumOf { it.capMem }, allocMem = st.sumOf { it.allocMem },
        running = st.sumOf { it.running }, queued = st.sumOf { it.queued }, sessions = st.sumOf { it.sessions },
    )
}

private fun alerts(f: Fleet, model: AppModel, now: Long): List<Pair<Mark, String>> {
    val out = mutableListOf<Pair<Mark, String>>()
    for (r in f.runners) {
        if (!r.online) {
            out += Mark.FAILED to "${r.name} isn't answering" + (r.lastSeen?.let { " (last seen ${com.restartfu.offload.mobile.ago(it, now)})" } ?: "")
            continue
        }
        val st = r.status ?: continue
        model.pausing[r.name]?.let { out += Mark.FAILED to "${r.name}: $it" }
        if (r.error.isNotEmpty()) out += Mark.WAITING to "${r.name}: ${r.error}"
        if (!st.paused && !st.accepting) out += Mark.WAITING to "${r.name} isn't accepting work: ${st.reason.ifEmpty { "unknown reason" }}"
        if (st.memAvailable < st.memReserve * 1.25) out += Mark.WAITING to "${r.name} is close to its memory reserve (${bytes(st.memAvailable)} available)"
        for (d in r.metrics?.disks.orEmpty()) {
            val free = if (d.total > 0) d.free.toDouble() / d.total else 1.0
            if (free < 0.2) out += (if (free < 0.1) Mark.FAILED else Mark.WAITING) to "${r.name}: the ${d.name} disk is ${pct(1 - free)} full"
        }
        if (f.version.isNotEmpty() && f.version != "dev" && st.version.isNotEmpty() && st.version != f.version) {
            out += Mark.STOPPED to "${r.name} runs ${st.version}, the computer ${f.version}: offload host upgrade ${r.name}"
        }
    }
    return out
}

@Composable
private fun RunnerCard(r: Runner, model: AppModel, now: Long) {
    val st = r.status
    Panel {
        Row(verticalAlignment = Alignment.CenterVertically) {
            Column(Modifier.weight(1f)) {
                Text(r.name, style = MaterialTheme.typography.titleMedium)
                Spacer(Modifier.height(4.dp))
                when {
                    !r.online -> StatusLabel(Mark.FAILED, "Not answering")
                    st == null -> StatusLabel(Mark.WAITING, "No status yet")
                    st.paused -> StatusLabel(Mark.PAUSED, "Paused")
                    !st.accepting -> StatusLabel(Mark.WAITING, "Not accepting")
                    else -> StatusLabel(Mark.WORKING, "Accepting work")
                }
            }
            if (r.online && st != null) {
                val busy = model.pausing.containsKey(r.name) && model.pausing[r.name] == null
                IconAction(
                    if (st.paused) Icons.Play else Icons.Pause,
                    if (st.paused) "Resume ${r.name}" else "Pause ${r.name}",
                    { model.togglePause(r) }, enabled = !busy, tint = MaterialTheme.colorScheme.onSurface,
                )
            }
        }
        if (st == null) return@Panel
        Spacer(Modifier.height(6.dp))
        Text(
            listOfNotNull(st.hostname, "${st.os}/${st.arch}", st.version, if (st.runtime == "container") "containers" else "host runtime",
                st.started?.let { "up " + duration(secondsSince(it, now)) }).joinToString(" · "),
            style = MaterialTheme.typography.bodySmall, color = MaterialTheme.colorScheme.onSurfaceVariant,
        )
        val m = r.metrics
        if (m != null && m.cpus.isNotEmpty()) {
            Spacer(Modifier.height(14.dp))
            Row {
                Text("Cores", style = MaterialTheme.typography.labelMedium, modifier = Modifier.weight(1f))
                Text(
                    "${cores(m.cpus.sumOf { it.busy })} of ${m.cpus.size} busy",
                    style = MaterialTheme.typography.labelMedium, color = MaterialTheme.colorScheme.onSurfaceVariant,
                )
            }
            Spacer(Modifier.height(8.dp))
            CoreGrid(r)
        }
        Spacer(Modifier.height(14.dp))
        MeterRow("CPU", st.allocCpu / st.capCpu.coerceAtLeast(0.001), "${cores(st.allocCpu)} of ${cores(st.capCpu)} cores guaranteed · load ${"%.1f".format(st.load1)}")
        val mem = st.allocMem.toDouble() / st.capMem.coerceAtLeast(1)
        MeterRow("Memory", mem, "${bytes(st.allocMem)} of ${bytes(st.capMem)} booked · ${bytes(st.memAvailable)} free on the host")
        for (d in m?.disks.orEmpty()) {
            val used = if (d.total > 0) 1 - d.free.toDouble() / d.total else 0.0
            MeterRow(if (d.name == "state") "Disk" else "Images", used, "${bytes(d.free)} free of ${bytes(d.total)}", warnAt = 0.8)
        }
        Spacer(Modifier.height(6.dp))
        Text(
            "${st.running} running · ${st.queued} queued · ${plural(st.sessions, "session")} · ${plural(st.warm, "warm workspace")}",
            style = MaterialTheme.typography.bodySmall, color = MaterialTheme.colorScheme.onSurfaceVariant,
        )
    }
}

@Composable
private fun MeterRow(label: String, frac: Double, text: String, warnAt: Double = 0.9) {
    Column(Modifier.padding(vertical = 5.dp)) {
        Row {
            Text(label, style = MaterialTheme.typography.labelMedium, modifier = Modifier.width(64.dp))
            Text(pct(frac), style = MaterialTheme.typography.labelMedium, color = MaterialTheme.colorScheme.onSurfaceVariant)
        }
        Spacer(Modifier.height(5.dp))
        Meter(frac, color = meterColor(frac, warnAt))
        Spacer(Modifier.height(4.dp))
        Text(text, style = MaterialTheme.typography.bodySmall, color = MaterialTheme.colorScheme.onSurfaceVariant)
    }
}

/** Each CPU a square, shaded by how busy it is; pinned ones ringed, the system's hatched. */
@Composable
private fun CoreGrid(r: Runner) {
    val cpus = r.metrics?.cpus.orEmpty()
    val s = LocalSignals.current
    val ring = MaterialTheme.colorScheme.onSurface
    val cell = 15.dp
    val gap = 4.dp
    val desc = "${cpus.size} CPUs, ${cores(cpus.sumOf { it.busy })} busy, ${cpus.count { it.owners.isNotEmpty() }} pinned to work"
    androidx.compose.foundation.layout.BoxWithConstraints(Modifier.fillMaxWidth()) {
        val perRow = ((maxWidth + gap) / (cell + gap)).toInt().coerceAtLeast(1)
        val rows = ceil(cpus.size / perRow.toDouble()).toInt()
        Canvas(Modifier.fillMaxWidth().height(cell * rows + gap * (rows - 1).coerceAtLeast(0)).semantics { contentDescription = desc }) {
            val c = cell.toPx()
            val g = gap.toPx()
            cpus.forEachIndexed { i, cpu ->
                val x = (i % perRow) * (c + g)
                val y = (i / perRow) * (c + g)
                val level = if (cpu.busy < 0.05) 0 else (ceil(cpu.busy * 6)).toInt().coerceIn(1, 6)
                drawRoundRect(s.heat[level], Offset(x, y), Size(c, c), CornerRadius(3f * density))
                if (cpu.reserved) {
                    drawLine(s.faint, Offset(x + 2, y + c - 2), Offset(x + c - 2, y + 2), 1.5f * density)
                }
                if (cpu.owners.isNotEmpty()) {
                    drawRoundRect(ring, Offset(x + 1, y + 1), Size(c - 2, c - 2), CornerRadius(3f * density), style = Stroke(1.5f * density))
                }
            }
        }
    }
    Spacer(Modifier.height(8.dp))
    Row(horizontalArrangement = Arrangement.spacedBy(14.dp), verticalAlignment = Alignment.CenterVertically) {
        Row(verticalAlignment = Alignment.CenterVertically) {
            Text("Idle ", style = MaterialTheme.typography.labelSmall, color = MaterialTheme.colorScheme.onSurfaceVariant)
            s.heat.forEach { Box(Modifier.size(8.dp, 8.dp).background(it)) }
            Text(" Busy", style = MaterialTheme.typography.labelSmall, color = MaterialTheme.colorScheme.onSurfaceVariant)
        }
        Row(verticalAlignment = Alignment.CenterVertically) {
            Canvas(Modifier.size(9.dp)) { drawRect(ring, style = Stroke(1.5f * density)) }
            Text(" Pinned", style = MaterialTheme.typography.labelSmall, color = MaterialTheme.colorScheme.onSurfaceVariant)
        }
    }
}

@Composable
private fun UsageCharts(model: AppModel, fleet: Fleet, now: Long) {
    val current = fleet.runners.filter { it.current }
    val st = current.mapNotNull { it.status }
    val loading = model.seriesFor != model.range
    val data = if (loading) null else model.series
    val sig = LocalSignals.current
    Column(verticalArrangement = Arrangement.spacedBy(12.dp)) {
        ChartPanel(
            "CPU", data, loading, model.range,
            listOf(Triple("Whole host", sig.series[0]) { p: com.restartfu.offload.mobile.Point -> p.cpuBusy },
                Triple("offload work", sig.series[1]) { p -> p.cpuUsed },
                Triple("Guaranteed", sig.series[2]) { p -> p.cpuAlloc }),
            capacity = st.sumOf { it.capCpu }, max = maxOf(st.sumOf { it.hostCpus }.toDouble(), st.sumOf { it.capCpu }), fmt = ::cores, now = now,
        )
        ChartPanel(
            "Memory", data, loading, model.range,
            listOf(Triple("Whole host", sig.series[0]) { p: com.restartfu.offload.mobile.Point -> p.memUsed },
                Triple("offload work", sig.series[1]) { p -> p.memWork },
                Triple("Booked", sig.series[2]) { p -> p.memAlloc }),
            capacity = st.sumOf { it.capMem }.toDouble(), max = maxOf(current.sumOf { it.metrics?.memTotal ?: 0 }.toDouble(), st.sumOf { it.capMem }.toDouble()),
            fmt = { bytes(it) }, bytes = true, now = now,
        )
    }
}

private fun niceMax(v: Double): Double {
    if (v <= 0) return 1.0
    val e = 10.0.pow(floor(log10(v)))
    val f = v / e
    return (listOf(1.0, 1.2, 1.6, 2.0, 2.4, 3.0, 4.0, 5.0, 6.0, 8.0, 10.0).firstOrNull { it >= f - 1e-9 } ?: 10.0) * e
}

private fun niceBytes(v: Double): Double {
    val g = v / (1L shl 30)
    val steps = listOf(1, 2, 4, 8, 16, 24, 32, 48, 64, 96, 128, 192, 256, 384, 512, 768, 1024, 1536, 2048)
    return (steps.firstOrNull { it >= g }?.toDouble() ?: (ceil(g / 1024) * 1024)) * (1L shl 30)
}

@Composable
private fun ChartPanel(
    title: String,
    data: Series?,
    loading: Boolean,
    range: String,
    lines: List<Triple<String, Color, (com.restartfu.offload.mobile.Point) -> Double>>,
    capacity: Double,
    max: Double,
    fmt: (Double) -> String,
    bytes: Boolean = false,
    now: Long,
) {
    val measurer = rememberTextMeasurer()
    val muted = MaterialTheme.colorScheme.onSurfaceVariant
    val grid = MaterialTheme.colorScheme.outlineVariant
    val refColor = MaterialTheme.colorScheme.outline
    val tick = TextStyle(fontFamily = Inter, fontSize = 10.sp, color = muted)
    Panel {
        Row(verticalAlignment = Alignment.CenterVertically) {
            Text(title, style = MaterialTheme.typography.titleSmall, modifier = Modifier.weight(1f))
            val last = data?.points?.lastOrNull()
            if (last != null) {
                Text("${fmt(lines[0].third(last))} now", style = MaterialTheme.typography.labelMedium, color = muted)
            }
        }
        Spacer(Modifier.height(4.dp))
        Row(horizontalArrangement = Arrangement.spacedBy(12.dp)) {
            lines.forEach { (label, color, _) ->
                Row(verticalAlignment = Alignment.CenterVertically) {
                    Box(Modifier.size(10.dp, 3.dp).background(color, RoundedCornerShape(2.dp)))
                    Text(" $label", style = MaterialTheme.typography.labelSmall, color = muted)
                }
            }
        }
        Spacer(Modifier.height(10.dp))
        val pts = data?.points.orEmpty()
        if (pts.size < 2) {
            Box(Modifier.fillMaxWidth().height(150.dp), contentAlignment = Alignment.Center) {
                Text(if (loading) "Loading…" else if (data == null) "No usage data yet" else "Collecting samples…", style = MaterialTheme.typography.bodySmall, color = muted)
            }
            return@Panel
        }
        val rangeSec = RANGES.first { it.first == range }.second
        Canvas(Modifier.fillMaxWidth().height(150.dp).semantics { contentDescription = "$title over the last ${RANGE_LABELS[range]}" }) {
            val left = 54.dp.toPx()
            val bottom = 18.dp.toPx()
            val w = size.width - left
            val h = size.height - bottom
            var ymax = maxOf(max, pts.maxOf { p -> lines.maxOf { it.third(p) } })
            ymax = if (bytes) niceBytes(ymax) else niceMax(ymax)
            val nowS = now / 1000
            val x0 = minOf(pts.first().t, nowS - rangeSec).toDouble()
            val x1 = maxOf(pts.last().t, nowS - 5).toDouble()
            fun X(t: Long) = left + ((t - x0) / (x1 - x0)).toFloat() * w
            fun Y(v: Double) = (h * (1 - v / ymax)).toFloat()
            for (i in 0..2) {
                val v = ymax * i / 2
                val y = Y(v)
                drawLine(grid, Offset(left, y), Offset(size.width, y), 1f)
                val label = measurer.measure(fmt(v), tick)
                drawText(label, topLeft = Offset(left - label.size.width - 6.dp.toPx(), (y - label.size.height / 2).coerceIn(0f, h - label.size.height)))
            }
            if (capacity > 0) {
                val y = Y(capacity)
                drawLine(refColor, Offset(left, y), Offset(size.width, y), 1.5f, pathEffect = PathEffect.dashPathEffect(floatArrayOf(8f, 6f)))
            }
            val fmtX = DateTimeFormatter.ofPattern(if (rangeSec >= 86400 * 2) "EEE" else "HH:mm").withZone(ZoneId.systemDefault())
            for (k in 0..2) {
                val t = (x0 + (x1 - x0) * (k / 2.0)).toLong()
                val l = measurer.measure(fmtX.format(Instant.ofEpochSecond(t)), tick)
                val x = (X(t) - l.size.width / 2).coerceIn(left, size.width - l.size.width)
                drawText(l, topLeft = Offset(x, size.height - l.size.height))
            }
            val gap = data!!.step * 3
            for ((_, color, value) in lines.reversed()) {
                val path = Path()
                var prev: Long? = null
                for (p in pts) {
                    if (prev == null || p.t - prev > gap) path.moveTo(X(p.t), Y(value(p))) else path.lineTo(X(p.t), Y(value(p)))
                    prev = p.t
                }
                drawPath(path, color, style = Stroke(2f * density, cap = StrokeCap.Round, join = StrokeJoin.Round))
            }
            val last = pts.last()
            for ((_, color, value) in lines) drawCircle(color, 3.5f * density, Offset(X(last.t), Y(value(last))))
        }
        if (capacity > 0) {
            Spacer(Modifier.height(6.dp))
            Row(verticalAlignment = Alignment.CenterVertically) {
                Canvas(Modifier.size(14.dp, 2.dp)) { drawLine(refColor, Offset.Zero, Offset(size.width, 0f), 3f, pathEffect = PathEffect.dashPathEffect(floatArrayOf(6f, 4f))) }
                Text(" Capacity offload hands out: ${fmt(capacity)}", style = MaterialTheme.typography.labelSmall, color = muted)
            }
        }
    }
}
