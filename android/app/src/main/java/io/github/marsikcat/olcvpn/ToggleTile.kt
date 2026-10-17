package io.github.marsikcat.olcvpn

import android.app.PendingIntent
import android.content.Intent
import android.net.VpnService
import android.os.Build
import android.service.quicksettings.Tile
import android.service.quicksettings.TileService
import org.json.JSONObject

/**
 * Плитка в шторке — то же, что пункт «Подключить/Отключить» в меню трея на ПК.
 */
class ToggleTile : TileService(), Backend.Listener {

    override fun onStartListening() {
        Backend.listeners += this
        paint(Backend.state)
    }

    override fun onStopListening() {
        Backend.listeners -= this
    }

    override fun onState(st: JSONObject) = paint(st)

    override fun onClick() {
        if (Backend.running) {
            Backend.send(Backend.op("disconnect"))
            return
        }
        // Разрешение на VPN спрашивает только окно приложения.
        if (VpnService.prepare(this) != null) {
            openApp()
            return
        }
        OlcVpnService.start(this, OlcVpnService.ACTION_CONNECT)
    }

    private fun openApp() {
        val i = Intent(this, MainActivity::class.java).addFlags(Intent.FLAG_ACTIVITY_NEW_TASK)
        if (Build.VERSION.SDK_INT >= 34) {
            startActivityAndCollapse(PendingIntent.getActivity(this, 0, i, PendingIntent.FLAG_IMMUTABLE))
        } else {
            @Suppress("DEPRECATION")
            startActivityAndCollapse(i)
        }
    }

    private fun paint(st: JSONObject?) {
        val tile = qsTile ?: return
        tile.state = if (st?.optBoolean("running") == true) Tile.STATE_ACTIVE else Tile.STATE_INACTIVE
        tile.label = "olcvpn"
        if (Build.VERSION.SDK_INT >= 29) tile.subtitle = st?.optString("title")?.takeIf { it.isNotEmpty() }
        tile.updateTile()
    }
}
