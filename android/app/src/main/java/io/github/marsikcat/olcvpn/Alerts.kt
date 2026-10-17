package io.github.marsikcat.olcvpn

import android.app.Notification
import android.app.NotificationChannel
import android.app.NotificationManager
import android.app.PendingIntent
import android.content.Context
import android.content.Intent

/**
 * Уведомления: постоянное — о состоянии туннеля (его ведёт OlcVpnService), и
 * разовые — когда без человека дальше не пойдёт: капча VK, скачанное
 * обновление, разрешение на VPN. Если окно открыто, капча и установка идут
 * прямо в него, без уведомления.
 */
object Alerts {
    const val STATUS = "status"
    private const val ACTION = "action"
    private const val NOTE_CAPTCHA = 2
    private const val NOTE_INSTALL = 3
    private const val NOTE_VPN = 4

    /** Страница капчи, которую окно должно показать, когда его откроют. */
    @Volatile var pendingCaptcha: String? = null
    @Volatile var pendingInstall: String? = null

    fun channels(ctx: Context) {
        val nm = ctx.getSystemService(NotificationManager::class.java)
        nm.createNotificationChannel(
            NotificationChannel(STATUS, "Состояние подключения", NotificationManager.IMPORTANCE_LOW)
                .apply { setShowBadge(false) }
        )
        nm.createNotificationChannel(
            NotificationChannel(ACTION, "Нужны ваши действия", NotificationManager.IMPORTANCE_HIGH)
        )
    }

    fun captcha(ctx: Context, url: String) {
        val nm = ctx.getSystemService(NotificationManager::class.java)
        if (url.isEmpty()) {
            pendingCaptcha = null
            nm.cancel(NOTE_CAPTCHA)
            MainActivity.shown?.leaveCaptcha()
            return
        }
        val w = MainActivity.shown
        if (w != null) {
            w.showCaptcha(url)
            return
        }
        pendingCaptcha = url
        notify(ctx, NOTE_CAPTCHA, "Проверка VK", "VK просит подтвердить, что вы не робот — нажмите")
    }

    fun install(ctx: Context, path: String) {
        val w = MainActivity.shown
        if (w != null) {
            w.install(path)
            return
        }
        pendingInstall = path
        notify(ctx, NOTE_INSTALL, "Обновление olcvpn скачано", "Нажмите, чтобы установить")
    }

    fun vpnPermission(ctx: Context) {
        notify(ctx, NOTE_VPN, "Нужно разрешение на VPN", "Откройте olcvpn и подключитесь — Android спросит разрешение")
    }

    fun clear(ctx: Context) {
        val nm = ctx.getSystemService(NotificationManager::class.java)
        nm.cancel(NOTE_CAPTCHA)
        nm.cancel(NOTE_INSTALL)
        nm.cancel(NOTE_VPN)
    }

    private fun notify(ctx: Context, id: Int, title: String, text: String) {
        val open = PendingIntent.getActivity(
            ctx, id, Intent(ctx, MainActivity::class.java),
            PendingIntent.FLAG_IMMUTABLE or PendingIntent.FLAG_UPDATE_CURRENT
        )
        val n = Notification.Builder(ctx, ACTION)
            .setSmallIcon(R.drawable.ic_stat)
            .setContentTitle(title)
            .setContentText(text)
            .setContentIntent(open)
            .setAutoCancel(true)
            .build()
        ctx.getSystemService(NotificationManager::class.java).notify(id, n)
    }
}
