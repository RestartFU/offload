package com.restartfu.offload.mobile.ui

import androidx.compose.foundation.background
import androidx.compose.foundation.border
import androidx.compose.foundation.horizontalScroll
import androidx.compose.foundation.layout.Arrangement
import androidx.compose.foundation.layout.Box
import androidx.compose.foundation.layout.Column
import androidx.compose.foundation.layout.Row
import androidx.compose.foundation.layout.Spacer
import androidx.compose.foundation.layout.WindowInsets
import androidx.compose.foundation.layout.fillMaxSize
import androidx.compose.foundation.layout.fillMaxWidth
import androidx.compose.foundation.layout.height
import androidx.compose.foundation.layout.navigationBars
import androidx.compose.foundation.layout.padding
import androidx.compose.foundation.layout.size
import androidx.compose.foundation.layout.windowInsetsPadding
import androidx.compose.foundation.rememberScrollState
import androidx.compose.foundation.shape.RoundedCornerShape
import androidx.compose.foundation.text.selection.SelectionContainer
import androidx.compose.foundation.verticalScroll
import androidx.compose.material3.Icon
import androidx.compose.material3.MaterialTheme
import androidx.compose.material3.SmallFloatingActionButton
import androidx.compose.material3.Text
import androidx.compose.material3.TextButton
import androidx.compose.runtime.Composable
import androidx.compose.runtime.LaunchedEffect
import androidx.compose.runtime.getValue
import androidx.compose.runtime.mutableStateOf
import androidx.compose.runtime.remember
import androidx.compose.runtime.rememberCoroutineScope
import androidx.compose.runtime.rememberUpdatedState
import androidx.compose.runtime.saveable.rememberSaveable
import androidx.compose.runtime.setValue
import androidx.compose.runtime.snapshotFlow
import androidx.compose.ui.Alignment
import androidx.compose.ui.Modifier
import androidx.compose.ui.draw.clip
import androidx.compose.ui.draw.rotate
import androidx.compose.ui.semantics.contentDescription
import androidx.compose.ui.semantics.semantics
import androidx.compose.ui.text.TextStyle
import androidx.compose.ui.text.font.FontWeight
import androidx.compose.ui.unit.dp
import androidx.compose.ui.unit.sp
import com.restartfu.offload.mobile.AppModel
import com.restartfu.offload.mobile.Ask
import com.restartfu.offload.mobile.Workload
import com.restartfu.offload.mobile.bytes
import com.restartfu.offload.mobile.clockTime
import com.restartfu.offload.mobile.cores
import com.restartfu.offload.mobile.duration
import com.restartfu.offload.mobile.stripAnsi
import kotlinx.coroutines.CancellationException
import kotlinx.coroutines.delay
import kotlinx.coroutines.launch

private const val LOG_KEEP = 200_000

@Composable
fun WorkloadPage(model: AppModel, runner: String, id: String, initialView: String = "logs") {
    val w = model.workload(runner, id)
    val now = rememberNow(model)
    val kindWord = when (w?.kind) { Workload.Kind.SESSION -> "session"; Workload.Kind.GITHUB -> "GitHub job"; else -> "job" }
    Column(Modifier.fillMaxSize()) {
        Header(
            title = w?.let { if (it.kind == Workload.Kind.GITHUB && it.github?.job?.isNotEmpty() == true) it.github.job else id } ?: id,
            subtitle = "${kindWord.replaceFirstChar { it.uppercase() }} on $runner",
            model = model,
            onBack = { model.back() },
            actions = {
                if (w != null && w.live) {
                    val session = w.kind == Workload.Kind.SESSION
                    val stopping = model.stopping.containsKey("$runner/$id") && model.stopping["$runner/$id"] == null
                    TextButton(
                        enabled = !stopping,
                        onClick = {
                            model.ask = Ask(
                                title = if (session) "Stop this session?" else "Cancel this job?",
                                body = if (session) "The MCP server stops. Its client starts a new one on the next request." else "It stops now on $runner. Its output so far stays here for an hour.",
                                action = if (session) "Stop session" else "Cancel job",
                                keep = "Keep it running",
                            ) { model.stopWorkload(w) }
                        },
                    ) {
                        Text(if (stopping) "Stopping…" else if (session) "Stop" else "Cancel", color = MaterialTheme.colorScheme.error, fontWeight = FontWeight.SemiBold)
                    }
                }
            },
        )
        if (w == null) {
            Empty("It's gone", "Runners list finished work for an hour. Activity keeps the statistics for longer.")
            return
        }
        Column(Modifier.padding(horizontal = 16.dp).padding(top = 14.dp)) {
            val (mark, word) = stateOf(w)
            Row(verticalAlignment = Alignment.CenterVertically) {
                StatusLabel(mark, word)
                Text("  ·  ${timeOf(w, now)}", style = MaterialTheme.typography.labelMedium, color = MaterialTheme.colorScheme.onSurfaceVariant)
            }
            model.stopping["$runner/$id"]?.let { Notice("Couldn't stop it: $it", Mark.FAILED) }
            if (w.kind != Workload.Kind.GITHUB || w.github?.job.isNullOrEmpty()) {
                Spacer(Modifier.height(10.dp))
                Box(
                    Modifier.fillMaxWidth().clip(RoundedCornerShape(10.dp)).background(MaterialTheme.colorScheme.surfaceContainerLowest)
                        .border(1.dp, MaterialTheme.colorScheme.outlineVariant, RoundedCornerShape(10.dp)).padding(12.dp),
                ) {
                    SelectionContainer {
                        Text(w.title, style = TextStyle(fontFamily = Mono, fontSize = 13.sp, lineHeight = 19.sp), maxLines = 4)
                    }
                }
            } else {
                Spacer(Modifier.height(6.dp))
                Text(listOf(w.github.repo, w.github.workflow).filter { it.isNotEmpty() }.joinToString(" · "), style = MaterialTheme.typography.bodyMedium, color = MaterialTheme.colorScheme.onSurfaceVariant)
            }
            if (w.live && w.state == "running") {
                Spacer(Modifier.height(14.dp))
                Row(horizontalArrangement = Arrangement.spacedBy(16.dp)) {
                    UseMeter("CPU", w.use?.cpu, w.cpu, "${cores(w.use?.cpu)} of ${cores(w.cpu)} cores", Modifier.weight(1f))
                    UseMeter("Memory", w.use?.mem?.toDouble(), w.mem.toDouble(), "${bytes(w.use?.mem)} of ${bytes(w.mem)}", Modifier.weight(1f), warn = true)
                }
            }
            Spacer(Modifier.height(16.dp))
        }
        var view by rememberSaveable { mutableStateOf(initialView) }
        val streams = if (w.kind == Workload.Kind.SESSION) listOf("stderr" to "stderr", "stdout" to "stdout") else listOf("stdout" to "stdout", "stderr" to "stderr")
        var stream by rememberSaveable { mutableStateOf(streams[0].first) }
        Row(Modifier.padding(horizontal = 16.dp), verticalAlignment = Alignment.CenterVertically) {
            Segmented(listOf("logs" to "Logs", "details" to "Details"), view, { view = it })
            Spacer(Modifier.weight(1f))
        }
        Spacer(Modifier.height(10.dp))
        Box(Modifier.weight(1f)) {
            if (view == "logs") {
                Column {
                    Row(Modifier.padding(horizontal = 16.dp).horizontalScroll(rememberScrollState())) {
                        Segmented(streams, stream, { stream = it })
                    }
                    Spacer(Modifier.height(10.dp))
                    Logs(model, w, stream)
                }
            } else {
                Details(w)
            }
        }
    }
}

@Composable
private fun Logs(model: AppModel, w: Workload, stream: String) {
    var text by remember(w.id, stream) { mutableStateOf("") }
    var ended by remember(w.id, stream) { mutableStateOf(false) }
    var error by remember(w.id, stream) { mutableStateOf<String?>(null) }
    val active by rememberUpdatedState(w.active)
    LaunchedEffect(w.runner.name, w.id, stream) {
        while (true) {
            try {
                text = ""
                model.dashboard.logs(w.runner.name, w.apiKind, w.id, stream) { piece ->
                    val t = text + stripAnsi(piece)
                    text = if (t.length > LOG_KEEP) t.substring(t.length - LOG_KEEP) else t
                }
                error = null
                if (!active) {
                    ended = true
                    return@LaunchedEffect
                }
            } catch (e: CancellationException) {
                throw e
            } catch (e: Exception) {
                error = e.message
            }
            delay(3_000)
        }
    }
    val scroll = rememberScrollState()
    val scope = rememberCoroutineScope()
    var follow by remember { mutableStateOf(true) }
    LaunchedEffect(scroll) {
        snapshotFlow { scroll.value to scroll.maxValue }.collect { (v, max) -> follow = v >= max - 24 }
    }
    LaunchedEffect(text) { if (follow) scroll.scrollTo(scroll.maxValue) }
    Box(
        Modifier.fillMaxSize().background(MaterialTheme.colorScheme.surfaceContainerLowest)
            .semantics { contentDescription = "Output" },
    ) {
        SelectionContainer {
            Text(
                when {
                    text.isNotEmpty() -> text
                    error != null -> ""
                    ended -> "No output."
                    else -> "Waiting for output…"
                },
                style = TextStyle(fontFamily = Mono, fontSize = 12.sp, lineHeight = 18.sp, color = if (text.isEmpty()) LocalSignals.current.faint else MaterialTheme.colorScheme.onSurface),
                modifier = Modifier.fillMaxWidth().verticalScroll(scroll).padding(16.dp).padding(bottom = 48.dp)
                    .windowInsetsPadding(WindowInsets.navigationBars),
            )
        }
        error?.let {
            Box(Modifier.align(Alignment.TopStart).padding(12.dp)) { Notice("Can't read the output: $it · trying again", Mark.FAILED) }
        }
        if (!follow) {
            SmallFloatingActionButton(
                onClick = { scope.launch { scroll.animateScrollTo(scroll.maxValue) } },
                modifier = Modifier.align(Alignment.BottomEnd).windowInsetsPadding(WindowInsets.navigationBars).padding(16.dp)
                    .semantics { contentDescription = "Jump to the end" },
                containerColor = MaterialTheme.colorScheme.surfaceContainerHighest,
            ) {
                Icon(Icons.Chevron, null, Modifier.size(18.dp).rotate(90f), tint = MaterialTheme.colorScheme.onSurface)
            }
        }
    }
}

@Composable
private fun Details(w: Workload) {
    Column(Modifier.fillMaxSize().verticalScroll(rememberScrollState()).padding(horizontal = 16.dp).padding(bottom = 32.dp).windowInsetsPadding(WindowInsets.navigationBars)) {
        KeyValue("Runner", w.runner.name)
        KeyValue("ID", w.id, mono = true)
        if (w.repo.isNotEmpty()) KeyValue("Repository", w.repo)
        w.github?.let { g ->
            if (g.workflow.isNotEmpty()) KeyValue("Workflow", g.workflow)
            if (g.runner.isNotEmpty()) KeyValue("GitHub runner", g.runner, mono = true)
            if (g.url.isNotEmpty()) KeyValue("Run", g.url, mono = true)
        }
        if (w.caller.isNotEmpty()) {
            KeyValue("Started by", when (w.caller) { "token" -> "A paired computer"; "local" -> "The runner's own socket"; "github" -> "GitHub Actions"; else -> w.caller })
        }
        KeyValue("Guaranteed", "${cores(w.cpu)} cores · ${bytes(w.mem)} memory limit")
        if (w.cpus.isNotEmpty()) KeyValue("Pinned CPUs", w.cpus, mono = true)
        if (w.runtime.isNotEmpty()) KeyValue("Runs in", if (w.runtime == "container") "a container" else "the runner's host")
        if (w.image.isNotEmpty()) KeyValue("Image", w.image, mono = true)
        KeyValue("Created", clockTime(w.created))
        w.started?.let { KeyValue("Started", clockTime(it)) }
        w.finished?.let { KeyValue("Finished", clockTime(it)) }
        if (w.cpuSeconds > 0) KeyValue("CPU time", duration(w.cpuSeconds))
        if (w.peakMem > 0) KeyValue("Peak memory", bytes(w.peakMem))
        if (w.reason.isNotEmpty()) KeyValue("Why", w.reason)
    }
}
