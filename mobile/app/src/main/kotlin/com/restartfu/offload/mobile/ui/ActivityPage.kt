package com.restartfu.offload.mobile.ui

import androidx.compose.foundation.layout.Arrangement
import androidx.compose.foundation.layout.Column
import androidx.compose.foundation.layout.Row
import androidx.compose.foundation.layout.Spacer
import androidx.compose.foundation.layout.fillMaxSize
import androidx.compose.foundation.layout.fillMaxWidth
import androidx.compose.foundation.layout.height
import androidx.compose.foundation.layout.padding
import androidx.compose.foundation.lazy.LazyColumn
import androidx.compose.foundation.lazy.items
import androidx.compose.material3.MaterialTheme
import androidx.compose.material3.Text
import androidx.compose.runtime.Composable
import androidx.compose.runtime.LaunchedEffect
import androidx.compose.ui.Modifier
import androidx.compose.ui.text.style.TextOverflow
import androidx.compose.ui.unit.dp
import androidx.compose.ui.unit.sp
import com.restartfu.offload.mobile.AppModel
import com.restartfu.offload.mobile.LedgerRow
import com.restartfu.offload.mobile.RANGES
import com.restartfu.offload.mobile.bytes
import com.restartfu.offload.mobile.cores
import com.restartfu.offload.mobile.duration
import com.restartfu.offload.mobile.pct
import com.restartfu.offload.mobile.plural
import com.restartfu.offload.mobile.quantile
import kotlinx.coroutines.delay

private data class CommandStats(
    val cmd: String, val runs: Int, val ok: Int, val oom: Int, val p50: Double?, val p95: Double?, val cpu: Double,
    val eff: Double?, val avgCores: Double?, val asks: Double?, val peakFrac: Double?, val mem: Double?,
)

private fun byCommand(jobs: List<LedgerRow>): List<CommandStats> = jobs.groupBy { it.command.ifEmpty { "?" } }.map { (cmd, es) ->
    val ran = es.filter { it.started && it.duration > 0 }
    val guaranteed = ran.sumOf { it.cpu * it.duration }
    val used = ran.sumOf { it.cpuSeconds }
    CommandStats(
        cmd = cmd, runs = es.size, ok = es.count { it.state == "exited" && it.exitCode == 0 }, oom = es.count { it.oom },
        p50 = quantile(ran.map { it.duration }, 0.5), p95 = quantile(ran.map { it.duration }, 0.95), cpu = used,
        eff = if (guaranteed > 0 && used > 0) used / guaranteed else null,
        avgCores = if (ran.isNotEmpty()) used / ran.sumOf { it.duration }.coerceAtLeast(1.0) else null,
        asks = quantile(ran.map { it.cpu }, 0.5),
        peakFrac = es.filter { it.peakMem > 0 && it.mem > 0 }.maxOfOrNull { it.peakMem.toDouble() / it.mem },
        mem = quantile(es.map { it.mem.toDouble() }, 0.5),
    )
}.sortedWith(compareByDescending<CommandStats> { it.cpu }.thenByDescending { it.runs }).take(12)

private fun hint(c: CommandStats): String? = when {
    c.oom > 0 -> "${plural(c.oom, "out-of-memory kill")}; its limit grows after each one"
    c.eff != null && c.eff < 0.35 && c.runs >= 2 && (c.asks ?: 0.0) >= 2 -> "Asks for ${cores(c.asks)} cores, uses ${cores(c.avgCores)} on average"
    c.peakFrac != null && c.peakFrac > 0.9 -> "Peaks at ${pct(c.peakFrac)} of its memory limit"
    else -> null
}

@Composable
fun ActivityPage(model: AppModel) {
    LaunchedEffect(model.range, model.fleet != null) {
        if (model.fleet == null) return@LaunchedEffect
        while (true) {
            model.loadLedger()
            delay(30_000)
        }
    }
    if (model.fleet == null) return Waiting(model)
    val ledger = model.ledger
    LazyColumn(Modifier.fillMaxSize(), contentPadding = pagePadding(), verticalArrangement = Arrangement.spacedBy(12.dp)) {
        item {
            Segmented(RANGES.map { it.first to it.first }, model.range, model::chooseRange)
            Spacer(Modifier.height(4.dp))
            Text("Finished work, last ${RANGE_LABELS[model.range]}", style = MaterialTheme.typography.bodySmall, color = MaterialTheme.colorScheme.onSurfaceVariant)
        }
        if (ledger == null || model.ledgerFor != model.range) {
            item { Loading() }
            return@LazyColumn
        }
        val jobs = ledger.rows.filter { it.kind == "job" }
        val notes = ledger.errors.map { (k, v) -> "$k: $v" } +
            (if (ledger.truncated.isNotEmpty()) listOf("Partial: ${ledger.truncated.joinToString()} sent only its newest entries, so older work is left out.") else emptyList())
        if (jobs.isEmpty()) {
            item { Empty(if (ledger.rows.isEmpty()) "No jobs finished" else "Only sessions finished", "Try a longer range.") }
        } else {
            val ok = jobs.count { it.state == "exited" && it.exitCode == 0 }
            val oom = jobs.count { it.oom }
            val started = jobs.filter { it.started }
            val waits = started.map { it.queueWait }
            val cpuH = jobs.sumOf { it.cpuSeconds } / 3600
            val warm = jobs.filter { it.syncFiles > 0 && it.uploaded != null }
            val warmRate = if (warm.isNotEmpty()) 1 - warm.sumOf { it.uploaded!! }.toDouble() / warm.sumOf { it.syncFiles }.coerceAtLeast(1) else null
            item {
                Row(horizontalArrangement = Arrangement.spacedBy(12.dp)) {
                    Stat("Jobs finished", "${jobs.size}", "${pct(ok.toDouble() / jobs.size)} succeeded", Modifier.weight(1f))
                    Stat("Out of memory", "$oom", if (oom > 0) "Retried with double memory" else "None hit its limit", Modifier.weight(1f))
                }
            }
            item {
                Row(horizontalArrangement = Arrangement.spacedBy(12.dp)) {
                    Stat("Wait to start", duration(quantile(waits, 0.5)), "median · 95th ${duration(quantile(waits, 0.95))}", Modifier.weight(1f))
                    Stat("CPU time", if (cpuH < 10) "%.1f".format(cpuH) else "${cpuH.toLong()}", "across ${plural(started.size, "job")}", Modifier.weight(1f), unit = "h")
                }
            }
            if (warmRate != null) {
                item { Stat("Files already warm", pct(warmRate), "of synced files didn't need uploading") }
            }
            item { SectionTitle("By command", "Most CPU time first", Modifier.padding(top = 8.dp)) }
            items(byCommand(jobs), key = { it.cmd }) { c ->
                Column(Modifier.fillMaxWidth().padding(vertical = 12.dp)) {
                    Text(c.cmd, style = MaterialTheme.typography.bodyMedium.copy(fontFamily = Mono, fontSize = 13.5.sp), maxLines = 2, overflow = TextOverflow.Ellipsis)
                    Spacer(Modifier.height(4.dp))
                    Text(
                        "${plural(c.runs, "run")} · ${pct(c.ok.toDouble() / c.runs)} succeeded · median ${duration(c.p50)} · 95th ${duration(c.p95)}",
                        style = MaterialTheme.typography.bodySmall, color = MaterialTheme.colorScheme.onSurfaceVariant,
                    )
                    Spacer(Modifier.height(8.dp))
                    Row(horizontalArrangement = Arrangement.spacedBy(16.dp)) {
                        UseMeter("CPU used", c.eff, 1.0, if (c.eff == null) "–" else "${pct(c.eff)} of guaranteed", Modifier.weight(1f))
                        UseMeter("Peak memory", c.peakFrac, 1.0, if (c.peakFrac == null) "–" else "${pct(c.peakFrac)} of ${bytes(c.mem)}", Modifier.weight(1f), warn = true)
                    }
                    hint(c)?.let {
                        Spacer(Modifier.height(8.dp))
                        Notice(it, if (c.oom > 0) Mark.FAILED else Mark.WAITING)
                    }
                }
                Rule()
            }
        }
        if (notes.isNotEmpty()) item { Column { notes.forEach { Notice(it, Mark.STOPPED) } } }
    }
}
