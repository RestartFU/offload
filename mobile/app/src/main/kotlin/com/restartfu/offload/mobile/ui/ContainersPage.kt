package com.restartfu.offload.mobile.ui

import androidx.compose.foundation.clickable
import androidx.compose.foundation.horizontalScroll
import androidx.compose.foundation.layout.Arrangement
import androidx.compose.foundation.layout.Column
import androidx.compose.foundation.layout.Row
import androidx.compose.foundation.layout.Spacer
import androidx.compose.foundation.layout.fillMaxSize
import androidx.compose.foundation.layout.fillMaxWidth
import androidx.compose.foundation.layout.height
import androidx.compose.foundation.layout.padding
import androidx.compose.foundation.layout.size
import androidx.compose.foundation.lazy.LazyColumn
import androidx.compose.foundation.lazy.items
import androidx.compose.foundation.rememberScrollState
import androidx.compose.material3.Icon
import androidx.compose.material3.MaterialTheme
import androidx.compose.material3.Text
import androidx.compose.runtime.Composable
import androidx.compose.runtime.getValue
import androidx.compose.runtime.mutableStateOf
import androidx.compose.runtime.saveable.rememberSaveable
import androidx.compose.runtime.setValue
import androidx.compose.ui.Alignment
import androidx.compose.ui.Modifier
import androidx.compose.ui.semantics.Role
import androidx.compose.ui.text.style.TextOverflow
import androidx.compose.ui.unit.dp
import androidx.compose.ui.unit.sp
import com.restartfu.offload.mobile.AppModel
import com.restartfu.offload.mobile.Page
import com.restartfu.offload.mobile.Workload
import com.restartfu.offload.mobile.ago
import com.restartfu.offload.mobile.bytes
import com.restartfu.offload.mobile.cores
import com.restartfu.offload.mobile.duration
import com.restartfu.offload.mobile.secondsSince
import com.restartfu.offload.mobile.workloads

/** The state of a job or session as a mark and its word. */
fun stateOf(w: Workload): Pair<Mark, String> {
    if (w.stale) return Mark.STOPPED to "Unknown"
    val g = w.github
    if (w.kind == Workload.Kind.GITHUB && g != null) {
        val verdict = when (g.result) {
            "succeeded" -> Mark.DONE to "Succeeded"
            "failed" -> Mark.FAILED to "Failed"
            "canceled", "cancelled" -> Mark.STOPPED to "Cancelled"
            else -> null
        }
        if (w.active) return verdict ?: if (g.job.isNotEmpty()) Mark.WORKING to "Running" else Mark.WAITING to "Starting"
        if (w.reason.startsWith("out of memory")) return Mark.FAILED to "Out of memory"
        if (verdict != null) return verdict
        if (g.job.isEmpty()) return Mark.STOPPED to "Took no job"
    }
    return when (w.state) {
        "running" -> Mark.WORKING to "Running"
        "queued" -> Mark.WAITING to if ("building" in w.reason) "Building environment" else "Queued"
        "uploading" -> Mark.WAITING to "Uploading"
        "exited" -> if (w.exitCode != null && w.exitCode != 0) Mark.FAILED to "Exit ${w.exitCode}" else Mark.DONE to "Done"
        "failed" -> Mark.FAILED to if (w.reason.startsWith("out of memory")) "Out of memory" else "Failed"
        "cancelled" -> Mark.STOPPED to "Cancelled"
        else -> Mark.STOPPED to w.state.ifEmpty { "Unknown" }
    }
}

fun timeOf(w: Workload, now: Long): String = when {
    w.stale -> "as of ${ago(w.runner.lastSeen, now)}"
    w.finished != null && w.started != null && w.started != w.finished -> "took ${duration((w.finished.toEpochMilli() - w.started.toEpochMilli()) / 1000.0)}"
    w.finished != null -> "ended ${ago(w.finished, now)}"
    w.started != null && w.state == "running" -> "for ${duration(secondsSince(w.started, now))}"
    else -> "waiting ${duration(secondsSince(w.created, now))}"
}

@Composable
fun ContainersPage(model: AppModel) {
    val fleet = model.fleet ?: return Waiting(model)
    var state by rememberSaveable { mutableStateOf("active") }
    var kind by rememberSaveable { mutableStateOf("") }
    val now = rememberNow(model)
    val all = fleet.workloads()
    val list = all.filter {
        (state == "all" || (state == "active") == it.active) &&
            (kind.isEmpty() || (kind == "job" && it.kind == Workload.Kind.JOB) || (kind == "session" && it.kind == Workload.Kind.SESSION) || (kind == "github" && it.kind == Workload.Kind.GITHUB))
    }.sortedWith(compareByDescending<Workload> { it.active }.thenByDescending { it.created })
    LazyColumn(Modifier.fillMaxSize(), contentPadding = pagePadding()) {
        item {
            Segmented(listOf("active" to "Active", "finished" to "Finished", "all" to "All"), state, { state = it })
            Spacer(Modifier.height(10.dp))
            Row(Modifier.horizontalScroll(rememberScrollState())) {
                Segmented(listOf("" to "Everything", "job" to "Jobs", "session" to "Sessions", "github" to "GitHub"), kind, { kind = it })
            }
            Spacer(Modifier.height(8.dp))
        }
        if (list.isEmpty()) {
            item {
                if (state == "active") {
                    Empty("Nothing is running", "Jobs you start with offload run, MCP sessions and GitHub jobs show up here.")
                } else {
                    Empty("Nothing here", "Runners list finished work for an hour. Activity goes further back.")
                }
            }
        }
        items(list, key = { it.runner.name + "/" + it.id }) { w ->
            WorkloadRow(w, now, showRunner = fleet.runners.size > 1) { model.open(Page.Detail(w.runner.name, w.id)) }
        }
    }
}

@Composable
fun WorkloadRow(w: Workload, now: Long, showRunner: Boolean, onClick: () -> Unit) {
    val (mark, word) = stateOf(w)
    Column(
        Modifier.fillMaxWidth().clickable(role = Role.Button, onClick = onClick).padding(vertical = 12.dp),
    ) {
        Row(verticalAlignment = Alignment.CenterVertically) {
            Text(
                when (w.kind) { Workload.Kind.JOB -> "Job"; Workload.Kind.SESSION -> "Session"; Workload.Kind.GITHUB -> "GitHub" },
                style = MaterialTheme.typography.labelSmall, color = MaterialTheme.colorScheme.onSurfaceVariant,
            )
            Text(" · ${w.id}", style = MaterialTheme.typography.labelSmall.copy(fontFamily = Mono), color = LocalSignals.current.faint)
            Spacer(Modifier.weight(1f))
            StatusLabel(mark, word)
        }
        Spacer(Modifier.height(4.dp))
        Row(verticalAlignment = Alignment.CenterVertically) {
            Text(
                w.title, maxLines = 2, overflow = TextOverflow.Ellipsis, modifier = Modifier.weight(1f),
                style = if (w.kind == Workload.Kind.GITHUB) MaterialTheme.typography.bodyLarge else MaterialTheme.typography.bodyMedium.copy(fontFamily = Mono, fontSize = 13.5.sp),
            )
            Icon(Icons.Chevron, null, Modifier.padding(start = 8.dp).size(16.dp), tint = LocalSignals.current.faint)
        }
        Spacer(Modifier.height(4.dp))
        Text(
            listOfNotNull(w.repo.ifEmpty { null }, if (showRunner) w.runner.name else null, timeOf(w, now)).joinToString(" · "),
            style = MaterialTheme.typography.bodySmall, color = MaterialTheme.colorScheme.onSurfaceVariant, maxLines = 1, overflow = TextOverflow.Ellipsis,
        )
        if (w.live && w.state == "running") {
            Spacer(Modifier.height(10.dp))
            Row(horizontalArrangement = Arrangement.spacedBy(16.dp)) {
                UseMeter("CPU", w.use?.cpu, w.cpu, "${cores(w.use?.cpu)} / ${cores(w.cpu)}", Modifier.weight(1f))
                UseMeter("Memory", w.use?.mem?.toDouble(), w.mem.toDouble(), "${bytes(w.use?.mem)} / ${bytes(w.mem)}", Modifier.weight(1f), warn = true)
            }
        } else if (!w.active && (w.cpuSeconds > 0 || w.peakMem > 0)) {
            Spacer(Modifier.height(2.dp))
            Text(
                listOfNotNull(if (w.cpuSeconds > 0) "${duration(w.cpuSeconds)} of CPU" else null, if (w.peakMem > 0) "peak ${bytes(w.peakMem)}" else null).joinToString(" · "),
                style = MaterialTheme.typography.bodySmall, color = LocalSignals.current.faint,
            )
        }
    }
    Rule()
}

@Composable
fun UseMeter(label: String, used: Double?, limit: Double, text: String, modifier: Modifier = Modifier, warn: Boolean = false) {
    Column(modifier) {
        Row {
            Text(label, style = MaterialTheme.typography.labelSmall, color = MaterialTheme.colorScheme.onSurfaceVariant, modifier = Modifier.weight(1f))
            Text(text, style = MaterialTheme.typography.labelSmall)
        }
        Spacer(Modifier.height(4.dp))
        val f = if (used != null && limit > 0) used / limit else 0.0
        Meter(f, color = if (warn) meterColor(f) else MaterialTheme.colorScheme.primary)
    }
}
