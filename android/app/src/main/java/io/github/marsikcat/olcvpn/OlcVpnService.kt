package io.github.marsikcat.olcvpn

import android.app.Notification
import android.app.NotificationManager
import android.app.PendingIntent
import android.content.Context
import android.content.Intent
import android.content.pm.ServiceInfo
import android.net.TrafficStats
import android.net.VpnService
import android.os.Build
import android.os.Handler
import android.os.Looper
import android.os.ParcelFileDescriptor
import android.os.Process
import android.util.Log
import org.json.JSONObject

/**
 * Служба на всё время сессии. Она держит процесс приложения живым (иначе
 * Android выгрузит его вместе с туннелем) и показывает состояние в
 * уведомлении, а в режиме «Весь трафик» ещё и открывает VPN: бэкенд просит
 * tun.open, служба собирает интерфейс через VpnService.Builder и отдаёт
 * дескриптор бэкенду, sing-box внутри него читает пакеты.
 *
 * Само приложение из своего VPN исключено. Так соединения ядра, клиента VK
 * TURN и бэкенда идут мимо туннеля и не замыкают его на себя — на ПК для
 * этого служат правила sing-box по имени процесса.
 */
class OlcVpnService : VpnService() {

    companion object {
        private const val TAG = "olcvpn"
        const val ACTION_START = "io.github.marsikcat.olcvpn.START"
        const val ACTION_CONNECT = "io.github.marsikcat.olcvpn.CONNECT"
        const val ACTION_DISCONNECT = "io.github.marsikcat.olcvpn.DISCONNECT"
        private const val NOTE_ID = 1

        @Volatile var instance: OlcVpnService? = null
            private set

        private val main = Handler(Looper.getMainLooper())
        private val pending = ArrayList<JSONObject>()

        fun start(ctx: Context, action: String) {
            try {
                ctx.startForegroundService(Intent(ctx, OlcVpnService::class.java).setAction(action))
            } catch (e: Exception) {
                // Android 12+ не даёт поднять службу из фона; сессия всё
                // равно идёт, просто процесс могут выгрузить.
                Log.w(TAG, "foreground service: ${e.message}")
            }
        }

        /** Follows the backend's state: a session needs the service, none does not. */
        fun sync(ctx: Context, st: JSONObject) {
            val svc = instance
            when {
                st.optBoolean("running") && svc == null -> start(ctx, ACTION_START)
                st.optBoolean("running") -> svc!!.showState(st)
                svc != null -> svc.finish()
            }
        }

        fun openTun(ctx: Context, req: JSONObject) {
            main.post {
                val svc = instance
                if (svc != null) {
                    svc.establish(req)
                } else {
                    pending += req
                    start(ctx, ACTION_START)
                }
            }
        }

        fun closeTun() {
            main.post { instance?.dropTun() }
        }
    }

    private var tun: ParcelFileDescriptor? = null
    private var foreground = false
    private var lastState: JSONObject? = null
    private var speed: String? = null
    private var uidRx = -1L
    private var uidTx = -1L

    override fun onCreate() {
        super.onCreate()
        instance = this
    }

    override fun onStartCommand(intent: Intent?, flags: Int, startId: Int): Int {
        goForeground()
        when (intent?.action) {
            // SERVICE_INTERFACE: система сама поднимает VPN — «Постоянный VPN»
            // в настройках Android.
            ACTION_CONNECT, SERVICE_INTERFACE -> connect()
            ACTION_DISCONNECT -> Backend.send(Backend.op("disconnect"))
        }
        val reqs = pending.toList()
        pending.clear()
        reqs.forEach { establish(it) }
        return START_NOT_STICKY
    }

    private fun connect() {
        Backend.start()
        Backend.whenReady { Backend.send(Backend.op("connect")) }
        // Если подключиться не вышло сразу (скажем, сервер не выбран),
        // состояние так и останется «отключено» — служба тогда не нужна.
        main.postDelayed({ if (!Backend.running && tun == null) finish() }, 8000)
    }

    private fun goForeground() {
        val n = note(Backend.state)
        if (Build.VERSION.SDK_INT >= 34) {
            startForeground(NOTE_ID, n, ServiceInfo.FOREGROUND_SERVICE_TYPE_SPECIAL_USE)
        } else {
            startForeground(NOTE_ID, n)
        }
        foreground = true
    }

    fun showState(st: JSONObject) {
        lastState = st
        val on = st.optString("phase").let { it == "proxy" || it == "connected" }
        if (!on) {
            // Связи нет — вместо скорости снова подсказка.
            speed = null
            uidRx = -1
            uidTx = -1
        }
        if (!foreground) return
        getSystemService(NotificationManager::class.java).notify(NOTE_ID, note(st))
    }

    /**
     * Скорость через туннель — раз в секунду от бэкенда. Её считает sing-box;
     * где его нет (olcrtc в «Только прокси»), берётся трафик самого
     * приложения: через него и идёт всё, что несёт туннель.
     */
    fun showSpeed(m: JSONObject) {
        var down = m.optLong("down")
        var up = m.optLong("up")
        if (!m.optBoolean("counted")) {
            val rx = TrafficStats.getUidRxBytes(Process.myUid())
            val tx = TrafficStats.getUidTxBytes(Process.myUid())
            if (rx < 0 || tx < 0) return
            down = if (uidRx < 0) 0 else rx - uidRx
            up = if (uidTx < 0) 0 else tx - uidTx
            uidRx = rx
            uidTx = tx
        } else {
            uidRx = -1
            uidTx = -1
        }
        speed = "↓ ${rate(down)}   ↑ ${rate(up)}"
        val st = lastState ?: Backend.state ?: return
        if (foreground) getSystemService(NotificationManager::class.java).notify(NOTE_ID, note(st))
    }

    private fun rate(bps: Long): String {
        val b = bps.coerceAtLeast(0)
        return when {
            b < 1024 -> "$b Б/с"
            b < 1024 * 1024 -> "${(b + 512) / 1024} КБ/с"
            else -> String.format(java.util.Locale("ru"), "%.1f МБ/с", b / 1048576.0)
        }
    }

    private fun note(st: JSONObject?): Notification {
        val title = st?.optString("title")?.takeIf { it.isNotEmpty() } ?: "Подключение…"
        val server = st?.optString("server").orEmpty()
        val hint = speed ?: st?.optString("hint").orEmpty()
        val open = PendingIntent.getActivity(
            this, 0, Intent(this, MainActivity::class.java),
            PendingIntent.FLAG_IMMUTABLE or PendingIntent.FLAG_UPDATE_CURRENT
        )
        val stop = PendingIntent.getService(
            this, 1, Intent(this, OlcVpnService::class.java).setAction(ACTION_DISCONNECT),
            PendingIntent.FLAG_IMMUTABLE or PendingIntent.FLAG_UPDATE_CURRENT
        )
        return Notification.Builder(this, Alerts.STATUS)
            .setSmallIcon(R.drawable.ic_stat)
            .setContentTitle(if (server.isNotEmpty()) "$title · $server" else title)
            .setContentText(hint)
            .setContentIntent(open)
            .setOngoing(true)
            .setOnlyAlertOnce(true)
            .setShowWhen(false)
            .addAction(Notification.Action.Builder(null, "Отключить", stop).build())
            .build()
    }

    fun establish(req: JSONObject) {
        val id = req.optInt("id")
        try {
            if (prepare(this) != null) {
                Alerts.vpnPermission(this)
                throw IllegalStateException("нет разрешения на VPN — откройте olcvpn и подключитесь оттуда")
            }
            val b = Builder()
                .setSession("olcvpn")
                .setMtu(req.optInt("mtu", 1500))
                .setConfigureIntent(
                    PendingIntent.getActivity(
                        this, 0, Intent(this, MainActivity::class.java), PendingIntent.FLAG_IMMUTABLE
                    )
                )
                .addDisallowedApplication(packageName)
            req.optJSONArray("addrs")?.let { a ->
                for (i in 0 until a.length()) prefix(a.getString(i)) { ip, len -> b.addAddress(ip, len) }
            }
            req.optJSONArray("routes")?.let { a ->
                for (i in 0 until a.length()) prefix(a.getString(i)) { ip, len -> b.addRoute(ip, len) }
            }
            req.optJSONArray("dns")?.let { a ->
                for (i in 0 until a.length()) b.addDnsServer(a.getString(i))
            }
            if (Build.VERSION.SDK_INT >= 29) b.setMetered(false)
            val fd = b.establish() ?: throw IllegalStateException("Android не дал открыть VPN")
            tun?.close()
            tun = fd
            if (!Backend.send(Backend.op("tun.fd").put("id", id), fd.fileDescriptor)) {
                dropTun()
            }
        } catch (e: Exception) {
            Log.w(TAG, "establish: ${e.message}")
            Backend.send(Backend.op("tun.err").put("id", id).put("error", e.message ?: e.toString()))
        }
    }

    private inline fun prefix(cidr: String, add: (String, Int) -> Unit) {
        val ip = cidr.substringBefore('/')
        val len = cidr.substringAfter('/', "").toIntOrNull() ?: if (ip.contains(':')) 128 else 32
        add(ip, len)
    }

    fun dropTun() {
        runCatching { tun?.close() }
        tun = null
    }

    fun finish() {
        dropTun()
        foreground = false
        stopForeground(STOP_FOREGROUND_REMOVE)
        stopSelf()
    }

    override fun onRevoke() {
        // VPN забрало другое приложение или его выключили в настройках.
        dropTun()
        Backend.send(Backend.op("revoked"))
        super.onRevoke()
    }

    override fun onDestroy() {
        dropTun()
        if (instance === this) instance = null
        super.onDestroy()
    }
}
