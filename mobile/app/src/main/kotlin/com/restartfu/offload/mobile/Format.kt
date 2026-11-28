package com.restartfu.offload.mobile

import java.time.Instant
import java.time.ZoneId
import java.time.format.DateTimeFormatter
import java.util.Locale
import kotlin.math.abs
import kotlin.math.floor
import kotlin.math.roundToLong

private const val GiB = 1024.0 * 1024 * 1024
private const val MiB = 1024.0 * 1024

fun bytes(b: Number?): String {
    val v = b?.toDouble() ?: return "–"
    return when {
        v == 0.0 -> "0"
        abs(v) >= 1024 * GiB -> "%.1f TiB".format(Locale.ROOT, v / (1024 * GiB))
        abs(v) >= 10 * GiB -> "${(v / GiB).roundToLong()} GiB"
        abs(v) >= GiB -> "%.1f GiB".format(Locale.ROOT, v / GiB)
        abs(v) >= MiB -> "${(v / MiB).roundToLong()} MiB"
        else -> "${(v / 1024).roundToLong()} KiB"
    }
}

fun cores(c: Double?): String {
    if (c == null || c.isNaN()) return "–"
    if (abs(c) >= 100) return c.roundToLong().toString()
    val r = Math.round(c * 10) / 10.0
    return if (r == floor(r)) r.toLong().toString() else "%.1f".format(Locale.ROOT, r)
}

fun duration(sec: Double?): String {
    if (sec == null || sec.isNaN()) return "–"
    val s = sec.coerceAtLeast(0.0)
    return when {
        s < 1 -> "${(s * 1000).roundToLong()}ms"
        s < 10 -> "%.1fs".format(Locale.ROOT, s).replace(".0s", "s")
        s < 60 -> "${s.roundToLong()}s"
        s < 3600 -> "${(s / 60).toLong()}m ${"%02d".format((s % 60).toLong())}s"
        s < 86400 -> "${(s / 3600).toLong()}h ${"%02d".format(((s % 3600) / 60).toLong())}m"
        else -> "${(s / 86400).toLong()}d ${((s % 86400) / 3600).toLong()}h"
    }
}

fun secondsSince(t: Instant?, now: Long): Double? = t?.let { (now - it.toEpochMilli()) / 1000.0 }

fun ago(t: Instant?, now: Long): String {
    val s = secondsSince(t, now) ?: return "–"
    return when {
        s < 60 -> "just now"
        s < 3600 -> "${(s / 60).toLong()}m ago"
        else -> duration(s) + " ago"
    }
}

fun pct(x: Double?): String = if (x == null || x.isNaN()) "–" else "${Math.round(x * 100)}%"

fun clockTime(t: Instant?): String = t?.let {
    DateTimeFormatter.ofPattern("MMM d, HH:mm", Locale.getDefault()).withZone(ZoneId.systemDefault()).format(it)
} ?: "–"

fun quantile(xs: List<Double>, q: Double): Double? {
    if (xs.isEmpty()) return null
    val a = xs.sorted()
    val i = (a.size - 1) * q
    val lo = floor(i).toInt()
    val hi = kotlin.math.ceil(i).toInt()
    return a[lo] + (a[hi] - a[lo]) * (i - lo)
}

private val ansi = Regex("\u001b\\[[0-9;?]*[ -/]*[@-~]|\u001b\\][^\u0007]*\u0007|\r(?!\n)")
fun stripAnsi(t: String) = ansi.replace(t, "")

fun plural(n: Int, one: String, many: String = one + "s") = "$n ${if (n == 1) one else many}"
