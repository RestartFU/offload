package com.restartfu.offload.mobile

import androidx.compose.runtime.getValue
import androidx.compose.runtime.mutableStateListOf
import androidx.compose.runtime.mutableStateMapOf
import androidx.compose.runtime.mutableStateOf
import androidx.compose.runtime.setValue
import java.time.Instant
import java.time.temporal.ChronoUnit
import kotlinx.coroutines.CancellationException
import kotlinx.coroutines.CoroutineScope
import kotlinx.coroutines.Job
import kotlinx.coroutines.async
import kotlinx.coroutines.awaitAll
import kotlinx.coroutines.delay
import kotlinx.coroutines.launch

enum class Tab(val title: String) { OVERVIEW("Overview"), CONTAINERS("Containers"), GITHUB("GitHub"), ACTIVITY("Activity") }

sealed interface Page {
    data class Home(val tab: Tab) : Page
    data class Detail(val runner: String, val id: String) : Page
    data object AddGitHub : Page
    data object Settings : Page
}

/** A question the app asks before it removes or stops something. */
data class Ask(val title: String, val body: String, val action: String, val keep: String = "Keep it", val run: () -> Unit)

/** How the link to the computer is doing, for the line under the header. */
sealed interface Link {
    data object Connecting : Link
    data object Live : Link
    data class Down(val reason: String, val retryAt: Long) : Link
}

val RANGES = listOf("1h" to 3600L, "6h" to 6 * 3600L, "24h" to 86400L, "7d" to 7 * 86400L)

/**
 * Everything the screens show and do. The pages read its state; it polls the dashboard while the
 * app is in front ([start]/[stop]) and holds nothing on disk but the pairing.
 */
class AppModel(
    private val store: PairingStore,
    private val prefs: Prefs,
    private val scope: CoroutineScope,
    private val makeDashboard: (() -> Pairing?) -> Dashboard = { HttpDashboard(it) },
    private val pairer: suspend (String, String, String) -> Pairing = { a, c, n -> HttpDashboard { null }.pair(a, c, n) },
    private val clock: () -> Long = System::currentTimeMillis,
) {
    /** The per-phone settings the model needs; [Preferences] in the app. */
    interface Prefs {
        var theme: ThemeChoice
        var range: String
        var lastAddress: String
    }

    var pairing by mutableStateOf(store.load())
        private set
    val dashboard: Dashboard = makeDashboard { pairing }

    var theme by mutableStateOf(prefs.theme)
        private set
    var info by mutableStateOf<Info?>(null)
        private set
    var fleet by mutableStateOf<Fleet?>(null)
        private set
    var link by mutableStateOf<Link>(Link.Connecting)
        private set
    /** Why the phone is on the pairing page again, when the computer forgot it. */
    var unpairedReason by mutableStateOf<String?>(null)
        private set

    var range by mutableStateOf(prefs.range)
        private set
    var series by mutableStateOf<Series?>(null)
        private set
    var seriesFor by mutableStateOf("")
        private set
    var ledger by mutableStateOf<Ledger?>(null)
        private set
    var ledgerFor by mutableStateOf("")
        private set

    val pages = mutableStateListOf<Page>(Page.Home(Tab.OVERVIEW))
    val page: Page get() = pages.last()
    /** Runner name -> null while pausing or resuming, or the error. */
    val pausing = mutableStateMapOf<String, String?>()
    /** "runner/id" -> null while stopping, or the error. */
    val stopping = mutableStateMapOf<String, String?>()
    var notice by mutableStateOf<String?>(null)
    var ask by mutableStateOf<Ask?>(null)

    private var poller: Job? = null

    fun chooseTheme(t: ThemeChoice) {
        theme = t
        prefs.theme = t
    }

    val lastAddress: String get() = prefs.lastAddress

    fun open(p: Page) {
        if (p is Page.Home) {
            pages.clear()
            pages += p
        } else {
            pages += p
        }
    }

    /** Back: true when something closed, false when the app should leave. */
    fun back(): Boolean {
        if (ask != null) {
            ask = null
            return true
        }
        if (pages.size > 1) {
            pages.removeAt(pages.lastIndex)
            return true
        }
        val home = page as? Page.Home
        if (home != null && home.tab != Tab.OVERVIEW) {
            pages[0] = Page.Home(Tab.OVERVIEW)
            return true
        }
        return false
    }

    suspend fun pair(address: String, code: String, deviceName: String) {
        val p = pairer(address.trim(), code, deviceName)
        store.save(p)
        prefs.lastAddress = address.trim()
        unpairedReason = null
        fleet = null
        info = null
        link = Link.Connecting
        pairing = p
        pages.clear()
        pages += Page.Home(Tab.OVERVIEW)
        poll()
    }

    /** Forgets the computer here, and asks it to forget this phone's key when it can. */
    suspend fun unpair() {
        runCatching { dashboard.forgetThisDevice() }
        dropPairing(null)
    }

    internal fun dropPairing(reason: String?) {
        poller?.cancel()
        poller = null
        store.clear()
        pairing = null
        fleet = null
        info = null
        series = null
        ledger = null
        unpairedReason = reason
    }

    fun start() {
        if (poller?.isActive == true) return
        poll()
    }

    fun stop() {
        poller?.cancel()
        poller = null
    }

    private fun poll() {
        poller?.cancel()
        if (pairing == null) return
        poller = scope.launch {
            var failures = 0
            while (true) {
                try {
                    if (!refresh()) return@launch
                    failures = 0
                } catch (e: CancellationException) {
                    throw e
                } catch (e: ApiException) {
                    if (e.code == 401) {
                        dropPairing("${pairing?.name ?: "The computer"} forgot this phone. Pair it again from Devices in the dashboard.")
                        return@launch
                    }
                    failures++
                    link = Link.Down(e.message ?: "The dashboard refused", clock() + backoff(failures))
                } catch (e: Exception) {
                    failures++
                    link = Link.Down(e.message ?: "Can't reach the computer", clock() + backoff(failures))
                }
                delay(if (failures == 0) 2_000 else backoff(failures))
            }
        }
    }

    /** Reads the computer's info (once) and the fleet. False when this app is too old for it. */
    suspend fun refresh(): Boolean {
        if (info == null) {
            val i = dashboard.info()
            info = i
            if (i.apiLevel > API_LEVEL) {
                link = Link.Down("${i.name} runs a newer offload than this app understands. Update the app.", 0)
                return false
            }
        }
        fleet = dashboard.fleet()
        link = Link.Live
        return true
    }

    private fun backoff(failures: Int) = (2_000L shl (failures - 1).coerceAtMost(3)).coerceAtMost(15_000)

    internal fun failed(reason: String) {
        link = Link.Down(reason, clock() + backoff(1))
    }

    /** Tries now instead of at the next retry. */
    fun retry() = poll()

    fun chooseRange(r: String) {
        range = r
        prefs.range = r
    }

    private fun rangeSeconds(r: String) = RANGES.firstOrNull { it.first == r }?.second ?: 3600L

    suspend fun loadSeries() {
        val r = range
        val runners = fleet?.runners.orEmpty().filter { it.current }
        val got = runners.map { runner ->
            scope.async { runCatching { dashboard.series(runner.name, r) }.getOrNull() }
        }.awaitAll().filterNotNull()
        if (r != range) return
        series = combine(got)
        seriesFor = r
    }

    suspend fun loadLedger() {
        val r = range
        val since = Instant.ofEpochMilli(clock()).minusSeconds(rangeSeconds(r)).truncatedTo(ChronoUnit.SECONDS).toString()
        val l = runCatching { dashboard.ledger(since) }.getOrElse { Ledger(emptyList(), mapOf("dashboard" to (it.message ?: "failed")), emptyList()) }
        if (r != range) return
        ledger = l
        ledgerFor = r
    }

    fun togglePause(runner: Runner) {
        val paused = runner.status?.paused == true
        if (pausing.containsKey(runner.name) && pausing[runner.name] == null) return
        pausing[runner.name] = null
        scope.launch {
            try {
                dashboard.pause(runner.name, !paused)
                fleet = runCatching { dashboard.fleet() }.getOrDefault(fleet)
                pausing.remove(runner.name)
                notice = if (paused) "${runner.name} takes new work again" else "${runner.name} is paused: running work finishes, nothing new starts"
            } catch (e: Exception) {
                pausing[runner.name] = "Couldn't ${if (paused) "resume" else "pause"} it: ${e.message}"
            }
        }
    }

    fun stopWorkload(w: Workload) {
        val key = "${w.runner.name}/${w.id}"
        stopping[key] = null
        scope.launch {
            try {
                dashboard.stop(w.runner.name, w.apiKind, w.id)
                notice = if (w.kind == Workload.Kind.SESSION) "Stopping the session" else "Cancelling ${w.id}"
            } catch (e: Exception) {
                stopping[key] = e.message ?: "failed"
            }
        }
    }

    suspend fun removeGitHub(owner: String, forget: Boolean): String {
        val res = dashboard.githubRemove(owner, forget)
        val bad = res.filter { it.second.isNotEmpty() }
        fleet = runCatching { dashboard.fleet() }.getOrDefault(fleet)
        return when {
            bad.isNotEmpty() -> "Couldn't remove $owner from " + bad.joinToString { "${it.first} (${it.second})" }
            forget -> "Removed $owner here. Delete its scale sets in GitHub's runner settings too."
            else -> "Removing $owner: its runners stop, then GitHub forgets them."
        }
    }

    suspend fun addGitHub(add: GitHubAdd): List<Pair<String, String>> {
        val res = dashboard.githubAdd(add)
        fleet = runCatching { dashboard.fleet() }.getOrDefault(fleet)
        return res
    }

    fun workload(runner: String, id: String): Workload? =
        fleet?.workloads()?.firstOrNull { it.runner.name == runner && it.id == id }

    fun now() = clock()
}
