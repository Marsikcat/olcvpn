package io.github.marsikcat.olcvpn

import android.content.Context
import android.net.ConnectivityManager
import android.net.LinkProperties
import android.net.Network
import org.json.JSONArray

/**
 * DNS текущей сети. У Go-бэкенда в Android нет /etc/resolv.conf, поэтому
 * серверы сети ему сообщает оболочка — при запуске и при каждой смене сети.
 * Сеть смотрится «для нас»: приложение исключено из своего VPN, так что это
 * всегда настоящая сеть, а не туннель.
 */
object Net {
    @Volatile private var servers: List<String> = emptyList()

    fun dns(): List<String> = servers

    fun watch(ctx: Context) {
        val cm = ctx.getSystemService(ConnectivityManager::class.java) ?: return
        cm.activeNetwork?.let { n -> cm.getLinkProperties(n)?.let { update(it) } }
        cm.registerDefaultNetworkCallback(object : ConnectivityManager.NetworkCallback() {
            override fun onLinkPropertiesChanged(network: Network, lp: LinkProperties) {
                update(lp)
                report()
            }
        })
    }

    private fun update(lp: LinkProperties) {
        servers = lp.dnsServers.mapNotNull { it.hostAddress?.substringBefore('%') }
    }

    fun report() {
        Backend.send(Backend.op("net").put("dns", JSONArray(servers)))
    }
}
