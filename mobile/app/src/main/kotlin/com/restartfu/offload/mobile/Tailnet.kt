package com.restartfu.offload.mobile

import android.content.Context
import android.net.ConnectivityManager
import android.net.NetworkCapabilities
import java.io.IOException
import java.net.HttpURLConnection
import java.net.Proxy

/**
 * Connections that can only leave through the VPN that routes to the computer's tailnet address.
 * The address alone proves nothing: with Tailscale off, 100.64.0.0/10 is whatever the network the
 * phone is on says it is (carriers use that range), and the token would go there in the clear.
 * So the name is resolved by the VPN, the VPN must have a route of its own to the address (a
 * default route doesn't count), and the socket is bound to it.
 */
class VpnTailnet(context: Context) : Tailnet {
    private val cm = context.applicationContext.getSystemService(ConnectivityManager::class.java)

    override fun open(host: String, port: Int, path: String): HttpURLConnection {
        @Suppress("DEPRECATION") // the replacement callbacks need a registered listener for a one-off lookup
        val vpns = cm.allNetworks.filter { cm.getNetworkCapabilities(it)?.hasTransport(NetworkCapabilities.TRANSPORT_VPN) == true }
        if (vpns.isEmpty()) throw IOException("Tailscale is off on this phone. Turn it on and try again.")
        for (vpn in vpns) {
            val ip = runCatching { vpn.getAllByName(host).toList() }.getOrDefault(emptyList()).firstOrNull(::onTailnet) ?: continue
            if (cm.getLinkProperties(vpn)?.routes.orEmpty().none { !it.isDefaultRoute && it.matches(ip) }) continue
            return vpn.openConnection(tailnetUrl(ip, port, path), Proxy.NO_PROXY) as HttpURLConnection
        }
        throw NotOnTailnet(host)
    }
}
