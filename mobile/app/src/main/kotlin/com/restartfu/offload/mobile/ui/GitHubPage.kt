package com.restartfu.offload.mobile.ui

import androidx.compose.foundation.background
import androidx.compose.foundation.clickable
import androidx.compose.foundation.layout.Arrangement
import androidx.compose.foundation.layout.Box
import androidx.compose.foundation.layout.Column
import androidx.compose.foundation.layout.Row
import androidx.compose.foundation.layout.Spacer
import androidx.compose.foundation.layout.WindowInsets
import androidx.compose.foundation.layout.fillMaxSize
import androidx.compose.foundation.layout.fillMaxWidth
import androidx.compose.foundation.layout.height
import androidx.compose.foundation.layout.heightIn
import androidx.compose.foundation.layout.imePadding
import androidx.compose.foundation.layout.navigationBars
import androidx.compose.foundation.layout.padding
import androidx.compose.foundation.layout.size
import androidx.compose.foundation.layout.width
import androidx.compose.foundation.layout.windowInsetsPadding
import androidx.compose.foundation.lazy.LazyColumn
import androidx.compose.foundation.lazy.items
import androidx.compose.foundation.rememberScrollState
import androidx.compose.foundation.shape.RoundedCornerShape
import androidx.compose.foundation.text.KeyboardOptions
import androidx.compose.foundation.verticalScroll
import androidx.compose.material3.Button
import androidx.compose.material3.Checkbox
import androidx.compose.material3.CircularProgressIndicator
import androidx.compose.material3.Icon
import androidx.compose.material3.MaterialTheme
import androidx.compose.material3.OutlinedButton
import androidx.compose.material3.Switch
import androidx.compose.material3.Text
import androidx.compose.runtime.Composable
import androidx.compose.runtime.LaunchedEffect
import androidx.compose.runtime.getValue
import androidx.compose.runtime.mutableStateListOf
import androidx.compose.runtime.mutableStateOf
import androidx.compose.runtime.remember
import androidx.compose.runtime.rememberCoroutineScope
import androidx.compose.runtime.setValue
import androidx.compose.ui.Alignment
import androidx.compose.ui.Modifier
import androidx.compose.ui.platform.LocalClipboardManager
import androidx.compose.ui.platform.LocalUriHandler
import androidx.compose.ui.semantics.Role
import androidx.compose.ui.text.AnnotatedString
import androidx.compose.ui.text.font.FontWeight
import androidx.compose.ui.text.input.KeyboardCapitalization
import androidx.compose.ui.text.input.KeyboardType
import androidx.compose.ui.text.input.PasswordVisualTransformation
import androidx.compose.ui.text.style.TextOverflow
import androidx.compose.ui.unit.dp
import com.restartfu.offload.mobile.AppModel
import com.restartfu.offload.mobile.Ask
import com.restartfu.offload.mobile.GitHubAdd
import com.restartfu.offload.mobile.Page
import com.restartfu.offload.mobile.Pool
import com.restartfu.offload.mobile.Runner
import com.restartfu.offload.mobile.Workload
import com.restartfu.offload.mobile.ago
import com.restartfu.offload.mobile.plural
import com.restartfu.offload.mobile.workloads
import kotlinx.coroutines.delay
import kotlinx.coroutines.launch

private fun labelsOf(p: Pool): List<String> {
    val seen = mutableSetOf<String>()
    return (p.systemLabels + p.labels).filter { seen.add(it.lowercase()) }
}

fun runsOn(labels: List<String>) = when (labels.size) {
    0 -> "offload"
    1 -> labels[0]
    else -> "[" + labels.joinToString(", ") + "]"
}

@Composable
fun GitHubPage(model: AppModel) {
    val fleet = model.fleet ?: return Waiting(model)
    val now = rememberNow(model)
    val owners = linkedMapOf<String, MutableList<Pair<Runner, Pool>>>()
    for (r in fleet.runners) for (p in r.github) {
        val key = owners.keys.firstOrNull { it.equals(p.owner, ignoreCase = true) } ?: p.owner
        owners.getOrPut(key) { mutableListOf() } += r to p
    }
    val jobs = fleet.workloads().filter { it.kind == Workload.Kind.GITHUB }
        .sortedWith(compareByDescending<Workload> { it.active }.thenByDescending { it.created })
    val active = jobs.filter { it.active }
    val recent = jobs.filter { !it.active }.take(10)
    var removing by remember { mutableStateOf<String?>(null) }
    var notice by remember { mutableStateOf<Pair<Mark, String>?>(null) }
    val scope = rememberCoroutineScope()
    LazyColumn(Modifier.fillMaxSize(), contentPadding = pagePadding()) {
        notice?.let { (m, t) -> item { Notice(t, m) } }
        if (owners.isEmpty()) {
            item {
                Empty(
                    "Run GitHub Actions here",
                    "Connect an organization or a repository, and its workflows with runs-on: offload run on your runners, each job in its own container.",
                ) {
                    Button(onClick = { model.open(Page.AddGitHub) }, shape = RoundedCornerShape(12.dp), modifier = Modifier.heightIn(min = 48.dp)) {
                        Icon(Icons.Plus, null, Modifier.size(18.dp))
                        Spacer(Modifier.width(8.dp))
                        Text("Add organization", fontWeight = FontWeight.SemiBold)
                    }
                }
            }
        } else {
            var room = 0
            for (r in fleet.runners) if (r.online) for (p in r.github) if (p.state == "listening") room += (p.capacity - p.running - p.starting).coerceAtLeast(0)
            val running = active.count { it.github?.job?.isNotEmpty() == true }
            item {
                Row(horizontalArrangement = Arrangement.spacedBy(12.dp)) {
                    Stat("Running", "$running", if (active.size > running) "${active.size - running} starting" else "GitHub jobs right now", Modifier.weight(1f))
                    Stat("Room for", "$room", "more, across ${plural(fleet.runners.count { it.github.isNotEmpty() }, "runner")}", Modifier.weight(1f))
                }
            }
            item { SectionTitle("Jobs", if (active.isEmpty()) "None right now" else null, Modifier.padding(top = 8.dp)) }
            if (active.isEmpty() && recent.isEmpty()) {
                item {
                    Text(
                        "They show up here the moment a workflow with runs-on: ${runsOn(owners.values.first().first().second.labels)} starts.",
                        style = MaterialTheme.typography.bodySmall, color = MaterialTheme.colorScheme.onSurfaceVariant,
                    )
                }
            }
            items(active + recent, key = { "j:" + it.runner.name + it.id }) { w ->
                WorkloadRow(w, now, showRunner = fleet.runners.size > 1) { model.open(Page.Detail(w.runner.name, w.id)) }
            }
            item {
                SectionTitle("Organizations and repositories", null, Modifier.padding(top = 16.dp)) {
                    IconAction(Icons.Plus, "Add organization", { model.open(Page.AddGitHub) }, tint = MaterialTheme.colorScheme.primary)
                }
            }
            items(owners.entries.toList(), key = { "o:" + it.key }) { (owner, list) ->
                OwnerCard(model, owner, list, now, busy = removing == owner) {
                    removing = owner
                    scope.launch {
                        notice = try {
                            val msg = model.removeGitHub(owner, forget = false)
                            (if (msg.startsWith("Couldn't")) Mark.FAILED else Mark.DONE) to msg
                        } catch (e: Exception) {
                            Mark.FAILED to "Couldn't remove $owner: ${e.message}"
                        }
                        removing = null
                    }
                }
                Spacer(Modifier.height(12.dp))
            }
        }
    }
}

@Composable
private fun OwnerCard(model: AppModel, owner: String, list: List<Pair<Runner, Pool>>, now: Long, busy: Boolean, onRemove: () -> Unit) {
    val first = list.first().second
    val labels = labelsOf(first)
    val line = "runs-on: ${runsOn(labels)}"
    var copied by remember { mutableStateOf(false) }
    val clip = LocalClipboardManager.current
    val uri = LocalUriHandler.current
    LaunchedEffect(copied) { if (copied) { delay(1600); copied = false } }
    Panel {
        Row(verticalAlignment = Alignment.CenterVertically) {
            Box(
                Modifier.size(36.dp).background(MaterialTheme.colorScheme.primary.copy(alpha = 0.16f), RoundedCornerShape(8.dp)),
                contentAlignment = Alignment.Center,
            ) {
                Text(owner.take(1).uppercase(), style = MaterialTheme.typography.titleMedium, color = MaterialTheme.colorScheme.primary)
            }
            Spacer(Modifier.width(12.dp))
            Column(Modifier.weight(1f)) {
                Text(owner, style = MaterialTheme.typography.titleMedium, maxLines = 1, overflow = TextOverflow.Ellipsis)
                Text(
                    listOfNotNull(first.name.ifEmpty { null }, if (owner.contains('/')) "Repository" else "Organization", if (first.shared) "warm caches" else "clean caches").joinToString(" · "),
                    style = MaterialTheme.typography.bodySmall, color = MaterialTheme.colorScheme.onSurfaceVariant,
                )
            }
            if (first.url.isNotEmpty()) IconAction(Icons.External, "Open $owner on GitHub", { runCatching { uri.openUri(first.url) } })
        }
        Spacer(Modifier.height(12.dp))
        Row(
            Modifier.fillMaxWidth().background(MaterialTheme.colorScheme.surfaceContainerLowest, RoundedCornerShape(8.dp))
                .clickable(role = Role.Button) { clip.setText(AnnotatedString(line)); copied = true }
                .padding(start = 12.dp, end = 4.dp, top = 4.dp, bottom = 4.dp),
            verticalAlignment = Alignment.CenterVertically,
        ) {
            Text(line, style = MaterialTheme.typography.bodySmall.copy(fontFamily = Mono), modifier = Modifier.weight(1f), maxLines = 2)
            IconAction(if (copied) Icons.Check else Icons.Copy, if (copied) "Copied" else "Copy the runs-on line", { clip.setText(AnnotatedString(line)); copied = true },
                tint = if (copied) LocalSignals.current.working else MaterialTheme.colorScheme.onSurfaceVariant)
        }
        Spacer(Modifier.height(12.dp))
        for ((r, p) in list) {
            Row(Modifier.fillMaxWidth().padding(vertical = 6.dp), verticalAlignment = Alignment.CenterVertically) {
                Column(Modifier.weight(1f)) {
                    Text(r.name, style = MaterialTheme.typography.bodyMedium)
                    Text(roomText(p, r, now), style = MaterialTheme.typography.bodySmall, color = MaterialTheme.colorScheme.onSurfaceVariant)
                }
                val (m, word) = when {
                    !r.online -> Mark.STOPPED to "Unknown"
                    p.state == "listening" -> Mark.WORKING to "Ready"
                    p.state == "preparing" -> Mark.WAITING to "Preparing"
                    p.state == "failed" -> Mark.FAILED to "Not taking jobs"
                    p.state == "removing" -> Mark.STOPPED to "Removing"
                    else -> Mark.STOPPED to p.state
                }
                StatusLabel(m, word)
            }
        }
        Spacer(Modifier.height(8.dp))
        DangerButton(
            if (busy) "Removing…" else "Remove $owner",
            {
                model.ask = Ask(
                    "Remove $owner?",
                    "Its jobs stop on every runner and its runner scale sets are deleted on GitHub. Workflows with this runs-on line wait until you add it again.",
                    "Remove $owner", run = onRemove,
                )
            },
            Modifier.fillMaxWidth(), enabled = !busy,
        )
    }
}

private fun roomText(p: Pool, r: Runner, now: Long): String {
    if (!r.online) return "Not answering"
    if (p.state != "listening") return p.detail
    val busy = p.running + p.starting
    val room = (p.capacity - busy).coerceAtLeast(0)
    if (busy == 0 && room == 0) return "Full: GitHub holds its jobs until there's room"
    val parts = listOfNotNull(
        if (p.running > 0) "${p.running} running" else null,
        if (p.starting > 0) "${p.starting} starting" else null,
        if (room > 0) "room for $room more" else "no room for more",
    )
    val last = p.lastRun?.let { " · last job ${ago(it, now)}" } ?: ""
    return parts.joinToString(" · ") + last
}

@Composable
fun AddGitHubPage(model: AppModel) {
    val fleet = model.fleet
    val eligible = fleet?.runners.orEmpty().filter { it.online && it.current && it.status?.runtime == "container" && it.url != "http://local" }
    var owner by remember { mutableStateOf("") }
    var useLogin by remember { mutableStateOf(true) }
    var token by remember { mutableStateOf("") }
    val chosen = remember { mutableStateListOf<String>().apply { addAll(eligible.mapNotNull { it.status?.id }) } }
    val arch = eligible.mapNotNull { r -> mapOf("amd64" to "x64", "arm64" to "arm64", "arm" to "arm")[r.status?.arch] }.distinct()
    var labels by remember { mutableStateOf((listOf("self-hosted", "linux") + (if (arch.size == 1) arch else emptyList()) + "offload").joinToString(", ")) }
    var shared by remember { mutableStateOf(false) }
    var busy by remember { mutableStateOf(false) }
    var error by remember { mutableStateOf<String?>(null) }
    val scope = rememberCoroutineScope()
    val ownerOk = Regex("^[A-Za-z0-9][A-Za-z0-9._-]*(/[A-Za-z0-9._-]+)?$").matches(owner.trim().removePrefix("https://github.com/").trimEnd('/'))
    val ok = ownerOk && chosen.isNotEmpty() && (useLogin || token.isNotBlank()) && !busy
    Column(Modifier.fillMaxSize()) {
        Header("Add organization", model, onBack = { model.back() })
        Column(
            Modifier.weight(1f).verticalScroll(rememberScrollState()).imePadding().padding(16.dp)
                .windowInsetsPadding(WindowInsets.navigationBars),
        ) {
            Text(
                "Every runner you pick takes this owner's GitHub Actions jobs while it has room, each in a fresh container.",
                style = MaterialTheme.typography.bodyMedium, color = MaterialTheme.colorScheme.onSurfaceVariant,
            )
            Spacer(Modifier.height(20.dp))
            Field("Organization or repository", owner, { owner = it; error = null }, "acme or acme/api",
                keyboard = KeyboardOptions(keyboardType = KeyboardType.Uri, capitalization = KeyboardCapitalization.None, autoCorrectEnabled = false))
            Spacer(Modifier.height(20.dp))
            ToggleRow("Use the computer's gh login", "GitHub needs the admin:org scope for an organization, repo for a repository.", useLogin) { useLogin = it }
            if (!useLogin) {
                Spacer(Modifier.height(12.dp))
                Column {
                    Text("Token", style = MaterialTheme.typography.labelLarge, fontWeight = FontWeight.Medium)
                    Spacer(Modifier.height(6.dp))
                    androidx.compose.material3.OutlinedTextField(
                        token, { token = it }, singleLine = true, modifier = Modifier.fillMaxWidth(), shape = RoundedCornerShape(10.dp),
                        placeholder = { Text("github_pat_…", color = LocalSignals.current.faint) },
                        visualTransformation = PasswordVisualTransformation(),
                        keyboardOptions = KeyboardOptions(keyboardType = KeyboardType.Password, autoCorrectEnabled = false),
                    )
                    Spacer(Modifier.height(6.dp))
                    Text("A fine-grained token with only Self-hosted runners: write. It stays on the runners.", style = MaterialTheme.typography.bodySmall, color = MaterialTheme.colorScheme.onSurfaceVariant)
                }
            }
            Spacer(Modifier.height(20.dp))
            Text("Runners", style = MaterialTheme.typography.labelLarge, fontWeight = FontWeight.Medium)
            Spacer(Modifier.height(4.dp))
            if (eligible.isEmpty()) {
                Notice("No runner can take GitHub jobs right now: they need podman, to be answering and to run this computer's version.", Mark.WAITING)
            }
            for (r in eligible) {
                val id = r.status?.id ?: continue
                Row(
                    Modifier.fillMaxWidth().heightIn(min = 48.dp).clickable(role = Role.Checkbox) { if (id in chosen) chosen.remove(id) else chosen.add(id) },
                    verticalAlignment = Alignment.CenterVertically,
                ) {
                    Checkbox(id in chosen, null)
                    Spacer(Modifier.width(8.dp))
                    Column {
                        Text(r.name, style = MaterialTheme.typography.bodyLarge)
                        Text("${r.status?.os}/${r.status?.arch} · ${com.restartfu.offload.mobile.cores(r.status?.capCpu)} cores", style = MaterialTheme.typography.bodySmall, color = MaterialTheme.colorScheme.onSurfaceVariant)
                    }
                }
            }
            Spacer(Modifier.height(16.dp))
            Field("Labels", labels, { labels = it }, "self-hosted, linux, offload", hint = "Workflows use runs-on: ${runsOn(labels.split(',').map { it.trim() }.filter { it.isNotEmpty() })}",
                keyboard = KeyboardOptions(autoCorrectEnabled = false))
            if (!owner.contains('/')) {
                Spacer(Modifier.height(16.dp))
                ToggleRow("Share warm caches", "Its repositories trust each other, so jobs reuse checkouts and caches. Off, every job starts clean.", shared) { shared = it }
            }
            error?.let {
                Spacer(Modifier.height(16.dp))
                Notice(it, Mark.FAILED)
            }
            Spacer(Modifier.height(24.dp))
            Row(horizontalArrangement = Arrangement.spacedBy(12.dp)) {
                OutlinedButton({ model.back() }, Modifier.weight(1f).heightIn(min = 52.dp), shape = RoundedCornerShape(12.dp)) { Text("Cancel") }
                Button(
                    onClick = {
                        busy = true
                        error = null
                        scope.launch {
                            try {
                                val res = model.addGitHub(
                                    GitHubAdd(
                                        owner = owner.trim(), token = if (useLogin) "" else token.trim(),
                                        labels = labels.split(',').map { it.trim() }.filter { it.isNotEmpty() }, systemLabels = false,
                                        max = null, shared = shared, on = chosen.toList(),
                                    ),
                                )
                                val bad = res.filter { it.second.isNotEmpty() }
                                if (bad.isEmpty()) {
                                    model.notice = "Added ${owner.trim()} on ${plural(res.size, "runner")}"
                                    model.back()
                                } else {
                                    error = bad.joinToString("\n") { "${it.first}: ${it.second}" }
                                }
                            } catch (e: Exception) {
                                error = e.message
                            } finally {
                                busy = false
                            }
                        }
                    },
                    enabled = ok, modifier = Modifier.weight(1f).heightIn(min = 52.dp), shape = RoundedCornerShape(12.dp),
                ) {
                    if (busy) CircularProgressIndicator(Modifier.size(18.dp), strokeWidth = 2.dp, color = MaterialTheme.colorScheme.onPrimary)
                    else Text("Add", fontWeight = FontWeight.SemiBold)
                }
            }
        }
    }
}

@Composable
fun ToggleRow(title: String, sub: String?, checked: Boolean, onChange: (Boolean) -> Unit) {
    Row(
        Modifier.fillMaxWidth().clickable(role = Role.Switch) { onChange(!checked) }.padding(vertical = 6.dp),
        verticalAlignment = Alignment.CenterVertically,
    ) {
        Column(Modifier.weight(1f).padding(end = 16.dp)) {
            Text(title, style = MaterialTheme.typography.bodyLarge)
            if (sub != null) Text(sub, style = MaterialTheme.typography.bodySmall, color = MaterialTheme.colorScheme.onSurfaceVariant)
        }
        Switch(checked, null)
    }
}
