package io.github.marsikcat.olcvpn

import android.content.BroadcastReceiver
import android.content.Context
import android.content.Intent
import org.json.JSONObject
import java.io.File

/**
 * «Запускать при включении телефона» — флаг-файл autostart, его ставит
 * бэкенд (autostart_android.go). С «Подключаться сразу» служба поднимается
 * прямо отсюда: после загрузки Android даёт это сделать, а позже из фона —
 * уже нет.
 */
class BootReceiver : BroadcastReceiver() {
    override fun onReceive(ctx: Context, intent: Intent) {
        if (intent.action != Intent.ACTION_BOOT_COMPLETED) return
        val dir = ctx.filesDir
        if (!File(dir, "autostart").exists()) return
        val autoconnect = runCatching {
            JSONObject(File(dir, "olcvpn.json").readText()).optBoolean("autoconnect")
        }.getOrDefault(false)
        if (autoconnect) {
            OlcVpnService.start(ctx, OlcVpnService.ACTION_CONNECT)
        } else {
            Backend.start()
        }
    }
}
