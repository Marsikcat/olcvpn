package io.github.marsikcat.olcvpn

import android.content.Context
import android.net.LocalSocket
import android.net.LocalSocketAddress
import android.os.Handler
import android.os.Looper
import android.os.SystemClock
import android.util.Log
import org.json.JSONObject
import java.io.File
import java.io.FileDescriptor
import java.io.OutputStream
import java.security.SecureRandom
import java.util.concurrent.CopyOnWriteArraySet
import kotlin.concurrent.thread

/**
 * Бэкенд olcvpn — тот же Go-код, что и на ПК (каталог app/ репозитория),
 * собранный под Android и уложенный в APK как libolcvpn.so. Оболочка
 * запускает его отдельным процессом, а дальше:
 *
 *  - интерфейс — его страница на 127.0.0.1, её показывает MainActivity;
 *  - по unix-сокету olcvpn.sock он присылает состояние, просит открыть VPN
 *    и показать капчу VK, а оболочка отдаёт ему дескриптор TUN.
 *
 * Протокол сокета описан в shell_android.go.
 */
object Backend {
    private const val TAG = "olcvpn"

    interface Listener {
        fun onHello(url: String) {}
        fun onState(st: JSONObject) {}
        fun onCaptcha(url: String) {}
        fun onInstall(path: String) {}
    }

    lateinit var ctx: Context
        private set

    /** Секрет интерфейса: без него бэкенд не отвечает (см. localOnly). */
    val token: String by lazy {
        val b = ByteArray(24)
        SecureRandom().nextBytes(b)
        b.joinToString("") { "%02x".format(it) }
    }

    @Volatile var url: String? = null
        private set
    @Volatile var state: JSONObject? = null
        private set

    val listeners = CopyOnWriteArraySet<Listener>()

    private val main = Handler(Looper.getMainLooper())
    private val sendLock = Any()
    private var proc: Process? = null
    private var stdin: OutputStream? = null
    @Volatile private var sock: LocalSocket? = null
    private val whenConnected = mutableListOf<() -> Unit>()
    private val crashes = ArrayDeque<Long>()

    fun init(c: Context) {
        ctx = c.applicationContext
    }

    val running: Boolean get() = state?.optBoolean("running") == true

    /** Starts the backend unless it is already up. */
    @Synchronized
    fun start() {
        if (proc?.isAlive == true) return
        val lib = ctx.applicationInfo.nativeLibraryDir
        val pb = ProcessBuilder(File(lib, "libolcvpn.so").absolutePath)
            .directory(ctx.filesDir)
            .redirectErrorStream(true)
        pb.environment().apply {
            put("OLCVPN_DIR", ctx.filesDir.absolutePath)
            put("OLCVPN_LIBDIR", lib)
            put("OLCVPN_TOKEN", token)
            put("OLCVPN_DNS", Net.dns().joinToString(","))
            put("HOME", ctx.filesDir.absolutePath)
            put("TMPDIR", ctx.cacheDir.absolutePath)
            // Go в Android берёт часовой пояс только из TZ, иначе время в
            // журнале — по UTC.
            put("TZ", java.util.TimeZone.getDefault().id)
        }
        val p = try {
            pb.start()
        } catch (e: Exception) {
            Log.e(TAG, "backend did not start", e)
            return
        }
        proc = p
        // Пока открыт наш конец stdin, бэкенд живёт; умрёт приложение —
        // закроется и он.
        stdin = p.outputStream
        thread(name = "olcvpn-out", isDaemon = true) {
            try {
                p.inputStream.bufferedReader().forEachLine { Log.i(TAG, it) }
            } catch (_: Exception) {
            }
            exited(p)
        }
        thread(name = "olcvpn-ctl", isDaemon = true) { link(p) }
    }

    /** Runs [action] once the control socket is up — now, if it already is. */
    fun whenReady(action: () -> Unit) {
        synchronized(whenConnected) {
            if (sock == null) {
                whenConnected += action
                return
            }
        }
        action()
    }

    fun send(msg: JSONObject, fd: FileDescriptor? = null): Boolean = synchronized(sendLock) {
        val s = sock ?: return false
        try {
            if (fd != null) s.setFileDescriptorsForSend(arrayOf(fd))
            s.outputStream.write((msg.toString() + "\n").toByteArray())
            s.outputStream.flush()
            true
        } catch (e: Exception) {
            Log.w(TAG, "control socket: ${e.message}")
            false
        } finally {
            if (fd != null) s.setFileDescriptorsForSend(null)
        }
    }

    fun op(name: String) = JSONObject().put("op", name)

    private fun link(p: Process) {
        val path = File(ctx.filesDir, "olcvpn.sock").absolutePath
        val deadline = SystemClock.elapsedRealtime() + 20_000
        var s: LocalSocket? = null
        while (p.isAlive && SystemClock.elapsedRealtime() < deadline) {
            try {
                s = LocalSocket().apply {
                    connect(LocalSocketAddress(path, LocalSocketAddress.Namespace.FILESYSTEM))
                }
                break
            } catch (_: Exception) {
                s = null
                Thread.sleep(100)
            }
        }
        if (s == null) {
            Log.e(TAG, "control socket never came up")
            return
        }
        val queued = synchronized(whenConnected) {
            sock = s
            whenConnected.toList().also { whenConnected.clear() }
        }
        Net.report()
        queued.forEach { it() }
        try {
            s.inputStream.bufferedReader().forEachLine { line ->
                runCatching { handle(JSONObject(line)) }
                    .onFailure { Log.w(TAG, "control message: ${it.message}") }
            }
        } catch (_: Exception) {
        }
        synchronized(whenConnected) { if (sock === s) sock = null }
        runCatching { s.close() }
    }

    private fun handle(m: JSONObject) {
        when (m.optString("op")) {
            "hello" -> {
                val u = m.optString("url")
                if (u.isNotEmpty()) {
                    url = u
                    main.post { listeners.forEach { it.onHello(u) } }
                }
            }
            "state" -> {
                state = m
                main.post {
                    OlcVpnService.sync(ctx, m)
                    listeners.forEach { it.onState(m) }
                }
            }
            "speed" -> main.post { OlcVpnService.instance?.showSpeed(m) }
            "tun.open" -> OlcVpnService.openTun(ctx, m)
            "tun.close" -> OlcVpnService.closeTun()
            "captcha" -> main.post { Alerts.captcha(ctx, m.optString("url")) }
            "install" -> main.post { Alerts.install(ctx, m.optString("path")) }
        }
    }

    private fun exited(p: Process) {
        val code = runCatching { p.waitFor() }.getOrDefault(-1)
        Log.w(TAG, "backend exited with $code")
        synchronized(this) {
            if (proc !== p) return
            proc = null
            stdin = null
        }
        url = null
        val stopped = op("state").put("phase", "stopped").put("running", false)
        state = stopped
        main.post {
            OlcVpnService.sync(ctx, stopped)
            listeners.forEach { it.onState(stopped) }
        }
        // Упавший бэкенд поднимаем снова, но не в цикле: три падения за
        // минуту — значит, что-то сломано всерьёз, и крутить его незачем.
        val now = SystemClock.elapsedRealtime()
        crashes.addLast(now)
        while (crashes.isNotEmpty() && now - crashes.first() > 60_000) crashes.removeFirst()
        if (crashes.size < 3) main.postDelayed({ start() }, 1000)
    }
}
