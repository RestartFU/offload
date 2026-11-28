package com.restartfu.offload.mobile

import java.io.IOException
import java.io.InputStream
import java.net.HttpURLConnection
import java.net.Inet4Address
import java.net.Inet6Address
import java.net.InetAddress
import java.net.Proxy
import java.net.URL
import java.net.URLEncoder
import kotlinx.coroutines.Dispatchers
import kotlinx.coroutines.awaitCancellation
import kotlinx.coroutines.delay
import kotlinx.coroutines.ensureActive
import kotlinx.coroutines.launch
import kotlinx.coroutines.withContext
import org.json.JSONArray
import org.json.JSONObject

/** What the app speaks: GET /api/info reports the dashboard's level, which must not be newer. */
const val API_LEVEL = 1
const val DEFAULT_PORT = 7421

class ApiException(val code: Int, message: String) : IOException(message)

/** The computer this phone is paired with. */
data class Pairing(val address: String, val token: String, val name: String)

/**
 * Talks to `offload dashboard` on the paired computer. Every request goes to an address on the
 * tailnet, through the VPN that routes to it and never a proxy, so WireGuard carries it; anything
 * else is refused before a byte is sent.
 */
interface Dashboard {
    suspend fun info(): Info
    suspend fun fleet(): Fleet
    suspend fun series(runner: String, range: String): Series
    suspend fun ledger(sinceRfc3339: String): Ledger
    suspend fun stop(runner: String, kind: String, id: String)
    suspend fun pause(runner: String, paused: Boolean)
    suspend fun githubRemove(owner: String, forget: Boolean): List<Pair<String, String>>
    suspend fun githubAdd(add: GitHubAdd): List<Pair<String, String>>
    suspend fun forgetThisDevice()
    /** Follows a job's or session's output; [onText] gets each piece as it arrives. Returns at its end. */
    suspend fun logs(runner: String, kind: String, id: String, stream: String, onText: (String) -> Unit)
}

data class GitHubAdd(
    val owner: String,
    val token: String,
    val labels: List<String>,
    val systemLabels: Boolean,
    val max: Int?,
    val shared: Boolean,
    val on: List<String>,
)

/** Splits "host", "host:port", "100.1.2.3", "[fd7a::1]:7421" into host and port. */
fun splitAddress(raw: String): Pair<String, Int>? {
    var s = raw.trim().removePrefix("http://").trimEnd('/')
    if (s.isEmpty() || s.contains('/') || s.contains('@')) return null
    if (s.startsWith("[")) {
        val end = s.indexOf(']').takeIf { it > 0 } ?: return null
        val host = s.substring(1, end)
        val rest = s.substring(end + 1)
        val port = if (rest.isEmpty()) DEFAULT_PORT else rest.removePrefix(":").toIntOrNull() ?: return null
        return host to port
    }
    if (s.count { it == ':' } > 1) return s to DEFAULT_PORT
    val port = s.substringAfter(':', "").let { if (it.isEmpty()) DEFAULT_PORT else it.toIntOrNull() ?: return null }
    s = s.substringBefore(':')
    if (!s.matches(Regex("^[A-Za-z0-9.-]+$"))) return null
    return s to port.takeIf { it in 1..65535 }.let { it ?: return null }
}

/** Tailscale's ranges: 100.64.0.0/10 and fd7a:115c:a1e0::/48. */
fun onTailnet(a: InetAddress): Boolean = when (a) {
    is Inet4Address -> a.address[0].toInt() and 0xff == 100 && (a.address[1].toInt() and 0xc0) == 64
    is Inet6Address -> a.address.take(6).map { it.toInt() and 0xff } == listOf(0xfd, 0x7a, 0x11, 0x5c, 0xa1, 0xe0)
    else -> false
}

class NotOnTailnet(host: String) : IOException(
    "$host isn't on your tailnet. Use the computer's Tailscale name or its 100.x address, and turn Tailscale on here.",
)

/** Resolves [host] and keeps only tailnet addresses, so a token never leaves the tailnet. */
fun tailnetAddress(host: String): InetAddress {
    val all = try {
        InetAddress.getAllByName(host).toList()
    } catch (e: IOException) {
        throw IOException("Can't find $host. Is Tailscale on, on this phone?")
    }
    return all.firstOrNull(::onTailnet) ?: throw NotOnTailnet(host)
}

/** Opens connections to the paired computer, and nowhere but the tailnet. */
interface Tailnet {
    /** A connection to [path] on [host], not yet connected. Throws when [host] isn't a tailnet address. */
    fun open(host: String, port: Int, path: String): HttpURLConnection
}

internal fun tailnetUrl(ip: InetAddress, port: Int, path: String): URL {
    val literal = if (ip is Inet6Address) "[${ip.hostAddress?.substringBefore('%')}]" else ip.hostAddress
    return URL("http://$literal:$port$path")
}

/**
 * Goes by the address alone, over whatever network is up. For the JVM, where there is no VPN to
 * ask; the app uses [VpnTailnet]. Never through a proxy, which would be handed the token.
 */
object DirectTailnet : Tailnet {
    override fun open(host: String, port: Int, path: String) =
        tailnetUrl(tailnetAddress(host), port, path).openConnection(Proxy.NO_PROXY) as HttpURLConnection
}

class HttpDashboard(private val tailnet: Tailnet, private val pairing: () -> Pairing?) : Dashboard {

    /** Builds the request without touching the network yet, so a caller can arrange to cancel it first. */
    private fun connect(method: String, path: String, token: String? = null, address: String? = null, readTimeout: Int = 15_000): Pair<HttpURLConnection, String> {
        val p = pairing()
        val addr = address ?: p?.address ?: throw ApiException(401, "Not paired")
        val (host, port) = splitAddress(addr) ?: throw IOException("\"$addr\" isn't an address")
        val c = tailnet.open(host, port, path)
        c.instanceFollowRedirects = false
        c.useCaches = false
        c.connectTimeout = 8_000
        c.readTimeout = readTimeout
        c.requestMethod = method
        c.setRequestProperty("Accept", "application/json")
        (token ?: p?.token)?.takeIf { it.isNotEmpty() }?.let { c.setRequestProperty("Authorization", "Bearer $it") }
        return c to addr
    }

    /** Sends the request and waits for the answer to start; anything but success is thrown. */
    private fun send(c: HttpURLConnection, addr: String, body: JSONObject? = null): HttpURLConnection {
        val code = try {
            if (body != null) {
                c.doOutput = true
                c.setRequestProperty("Content-Type", "application/json")
                c.outputStream.use { it.write(body.toString().toByteArray()) }
            }
            c.responseCode
        } catch (e: IOException) {
            c.disconnect()
            throw IOException(friendly(e, addr), e)
        }
        if (code !in 200..299) {
            val msg = runCatching { JSONObject(c.errorStream.bufferedReader().readText()).optString("error") }.getOrNull()
            c.disconnect()
            throw ApiException(code, msg?.ifEmpty { null } ?: "The dashboard answered $code")
        }
        return c
    }

    private fun friendly(e: IOException, address: String): String = when (e) {
        is java.net.ConnectException -> "Nothing answers at $address. Is offload dashboard running on the computer?"
        is java.net.SocketTimeoutException -> "$address didn't answer in time. Are both devices on the tailnet?"
        else -> e.message ?: "Can't reach $address"
    }

    private fun json(method: String, path: String, body: JSONObject? = null, readTimeout: Int = 15_000): String {
        val (c, addr) = connect(method, path, readTimeout = readTimeout)
        return send(c, addr, body).let { try { it.inputStream.bufferedReader().readText() } finally { it.disconnect() } }
    }

    private fun q(s: String) = URLEncoder.encode(s, "UTF-8")

    // Each call waits a little longer than the dashboard gives its own calls to runners and to
    // GitHub (up to 90 s for an add), so a slow change is reported, not cut off half done.

    override suspend fun info() = io { parseInfo(JSONObject(json("GET", "/api/info"))) }
    override suspend fun fleet() = io { parseFleet(JSONObject(json("GET", "/api/fleet"))) }
    override suspend fun series(runner: String, range: String) = io { parseSeries(JSONObject(json("GET", "/api/series?host=${q(runner)}&range=${q(range)}"))) }
    override suspend fun ledger(sinceRfc3339: String) = io { parseLedger(JSONObject(json("GET", "/api/ledger?since=${q(sinceRfc3339)}", readTimeout = 25_000))) }
    override suspend fun stop(runner: String, kind: String, id: String) = io {
        json("POST", "/api/stop", JSONObject().put("host", runner).put("kind", kind).put("id", id), readTimeout = 25_000); Unit
    }
    override suspend fun pause(runner: String, paused: Boolean) = io {
        json("POST", "/api/pause", JSONObject().put("host", runner).put("paused", paused), readTimeout = 25_000); Unit
    }

    private fun results(s: String): List<Pair<String, String>> {
        val a = JSONObject(s).optJSONArray("results") ?: JSONArray()
        return (0 until a.length()).map { a.getJSONObject(it).let { r -> r.optString("runner") to r.optString("error", "") } }
    }

    override suspend fun githubRemove(owner: String, forget: Boolean) = io {
        results(json("POST", "/api/github/remove", JSONObject().put("owner", owner).put("forget", forget), readTimeout = 45_000))
    }

    override suspend fun githubAdd(add: GitHubAdd) = io {
        val body = JSONObject().put("owner", add.owner).put("token", add.token).put("labels", JSONArray(add.labels))
            .put("system_labels", add.systemLabels).put("shared", add.shared).put("on", JSONArray(add.on))
        add.max?.let { body.put("max", it) }
        results(json("POST", "/api/github/add", body, readTimeout = 105_000))
    }

    override suspend fun forgetThisDevice() = io { json("DELETE", "/api/device"); Unit }

    override suspend fun logs(runner: String, kind: String, id: String, stream: String, onText: (String) -> Unit) = withContext(Dispatchers.IO) {
        val (c, addr) = connect("GET", "/api/logs?host=${q(runner)}&kind=$kind&id=${q(id)}&stream=$stream&tail=65536", readTimeout = 0)
        // A blocked connect or read can't be cancelled, so leaving the page closes the connection
        // under it, and so does an answer that takes too long to start.
        val closer = launch { try { awaitCancellation() } finally { c.disconnect() } }
        val slow = launch { delay(20_000); c.disconnect() }
        try {
            send(c, addr)
            slow.cancel()
            readLogs(c.inputStream, onText)
        } catch (e: IOException) {
            ensureActive()
            throw e
        } finally {
            slow.cancel()
            closer.cancel()
            c.disconnect()
        }
    }

    /** Pairs with the dashboard at [address] using the code it shows, returning the new pairing. */
    suspend fun pair(address: String, code: String, deviceName: String): Pairing = io {
        val (c, addr) = connect("POST", "/api/pair", token = "", address = address)
        send(c, addr, JSONObject().put("code", code).put("name", deviceName))
        val o = try { JSONObject(c.inputStream.bufferedReader().readText()) } finally { c.disconnect() }
        val info = parseInfo(o.optJSONObject("info") ?: JSONObject())
        Pairing(address.trim(), o.getString("token"), info.name.ifEmpty { address.trim() })
    }
}

/** The log stream starts with one JSON line, {"offset":N}, then the raw output. */
internal fun readLogs(input: InputStream, onText: (String) -> Unit) {
    while (true) {
        val b = input.read()
        if (b < 0) return
        if (b == '\n'.code) break
    }
    val reader = input.reader(Charsets.UTF_8)
    val buf = CharArray(8 shl 10)
    while (true) {
        val n = reader.read(buf)
        if (n < 0) return
        onText(String(buf, 0, n))
    }
}

private suspend fun <T> io(block: () -> T): T = withContext(Dispatchers.IO) { block() }
