package com.restartfu.offload.mobile

import android.content.Context
import android.net.ConnectivityManager
import android.net.NetworkCapabilities
import java.io.IOException
import java.net.HttpURLConnection
import java.net.Inet4Address
import java.net.InetAddress
import java.net.Proxy

/**
 * Connections that can only leave through Tailscale's VPN on this phone. The address alone proves
 * nothing: with Tailscale off, 100.64.0.0/10 is whatever the phone's network says it is (carriers
 * use that range), and another VPN's catch-all routes cover it too. The token would go there in
 * the clear. So the name is resolved by the VPN, the VPN must look like Tailscale ([carriedBy]),
 * and the socket is bound to it.
 */
class VpnTailnet(context: Context) : Tailnet {
    private val cm = context.applicationContext.getSystemService(ConnectivityManager::class.java)

    override fun open(host: String, port: Int, path: String): HttpURLConnection {
        @Suppress("DEPRECATION") // the replacement callbacks need a registered listener for a one-off lookup
        val vpns = cm.allNetworks.filter { cm.getNetworkCapabilities(it)?.hasTransport(NetworkCapabilities.TRANSPORT_VPN) == true }
        if (vpns.isEmpty()) throw IOException("Tailscale is off on this phone. Turn it on and try again.")
        for (vpn in vpns) {
            val link = cm.getLinkProperties(vpn) ?: continue
            val ip = runCatching { vpn.getAllByName(host).toList() }.getOrDefault(emptyList()).firstOrNull(::onTailnet) ?: continue
            val routes = link.routes.map { it.destination.address to it.destination.prefixLength }
            if (!carriedBy(ip, link.linkAddresses.map { it.address }, routes)) continue
            return vpn.openConnection(tailnetUrl(ip, port, path), Proxy.NO_PROXY) as HttpURLConnection
        }
        throw IOException("$host isn't reachable through Tailscale on this phone. Turn Tailscale on (another VPN doesn't count) and use the computer's Tailscale name or 100.x address.")
    }
}

/**
 * Whether a VPN with these interface [addresses] and [routes] (destination and prefix length) is
 * Tailscale carrying [ip]: it has a tailnet address of its own, and a route to [ip] that lies
 * inside Tailscale's ranges. Routes that merely contain the range don't count: 0.0.0.0/1 or
 * 64.0.0.0/2 are how other VPNs take all traffic without a default route.
 */
internal fun carriedBy(ip: InetAddress, addresses: List<InetAddress>, routes: List<Pair<InetAddress, Int>>): Boolean =
    addresses.any(::onTailnet) && routes.any { (dest, bits) ->
        onTailnet(dest) && bits >= (if (dest is Inet4Address) 10 else 48) && prefixHolds(dest, bits, ip)
    }

internal fun prefixHolds(dest: InetAddress, bits: Int, ip: InetAddress): Boolean {
    val a = dest.address
    val b = ip.address
    if (a.size != b.size || bits > a.size * 8) return false
    for (i in 0 until bits) {
        val mask = 0x80 shr (i % 8)
        if ((a[i / 8].toInt() and mask) != (b[i / 8].toInt() and mask)) return false
    }
    return true
}
