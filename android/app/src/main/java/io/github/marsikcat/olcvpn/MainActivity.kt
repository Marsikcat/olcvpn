package io.github.marsikcat.olcvpn

import android.Manifest
import android.app.Activity
import android.content.ClipData
import android.content.ClipboardManager
import android.content.Intent
import android.content.pm.PackageManager
import android.graphics.Bitmap
import android.graphics.BitmapFactory
import android.graphics.Color
import android.net.Uri
import android.net.VpnService
import android.os.Build
import android.os.Bundle
import android.provider.MediaStore
import android.provider.Settings
import android.util.Base64
import android.view.View
import android.view.WindowInsets
import android.view.WindowInsetsController
import android.webkit.CookieManager
import android.webkit.JavascriptInterface
import android.webkit.ValueCallback
import android.webkit.WebChromeClient
import android.webkit.WebResourceError
import android.webkit.WebResourceRequest
import android.webkit.WebView
import android.webkit.WebViewClient
import android.widget.FrameLayout
import android.widget.Toast
import org.json.JSONObject
import java.io.ByteArrayOutputStream
import java.io.File
import kotlin.concurrent.thread

/**
 * Окно приложения — та же страница, что на ПК в окне WebView2: её отдаёт
 * бэкенд на 127.0.0.1, а сюда она приходит с токеном в cookie. Страница
 * зовёт оболочку через window.olcAndroid (см. Bridge) — разрешение на VPN,
 * буфер обмена, «Отправить», цвет системных полос.
 */
class MainActivity : Activity(), Backend.Listener {

    companion object {
        /** Окно на экране — туда идут капча и установка обновления. */
        @Volatile var shown: MainActivity? = null
            private set

        private const val REQ_VPN = 1
        private const val REQ_FILE = 2
        private const val REQ_NOTIFY = 3
        private val OUR_SCHEMES = setOf("olconnect", "olcrtc", "olcvpn")
    }

    private lateinit var root: FrameLayout
    private lateinit var web: WebView
    private var appUrl: String? = null
    @Volatile private var onApp = false
    private var pageReady = false
    private val pendingJs = ArrayList<String>()
    private var fileCallback: ValueCallback<Array<Uri>>? = null
    private var cameraFile: File? = null

    override fun onCreate(savedInstanceState: Bundle?) {
        super.onCreate(savedInstanceState)
        root = FrameLayout(this)
        web = WebView(this)
        root.addView(web, FrameLayout.LayoutParams(-1, -1))
        setContentView(root)
        edgeToEdge()
        setupWeb()

        Backend.listeners += this
        Backend.start()
        Backend.url?.let { load(it) }

        if (Build.VERSION.SDK_INT >= 33 &&
            checkSelfPermission(Manifest.permission.POST_NOTIFICATIONS) != PackageManager.PERMISSION_GRANTED
        ) {
            requestPermissions(arrayOf(Manifest.permission.POST_NOTIFICATIONS), REQ_NOTIFY)
        }
        handle(intent)
    }

    /** Страница рисуется под системными полосами, а отступы — по их размеру. */
    private fun edgeToEdge() {
        if (Build.VERSION.SDK_INT < 30) return
        window.setDecorFitsSystemWindows(false)
        window.statusBarColor = Color.TRANSPARENT
        window.navigationBarColor = Color.TRANSPARENT
        root.setOnApplyWindowInsetsListener { v, insets ->
            val bars = insets.getInsets(WindowInsets.Type.systemBars())
            val ime = insets.getInsets(WindowInsets.Type.ime())
            v.setPadding(bars.left, bars.top, bars.right, maxOf(bars.bottom, ime.bottom))
            WindowInsets.CONSUMED
        }
    }

    private fun setupWeb() {
        web.settings.apply {
            javaScriptEnabled = true
            domStorageEnabled = true
            allowFileAccess = false
            allowContentAccess = false
            setSupportZoom(false)
        }
        web.setBackgroundColor(Color.TRANSPARENT)
        web.addJavascriptInterface(Bridge(), "olcAndroid")
        web.webViewClient = object : WebViewClient() {
            override fun shouldOverrideUrlLoading(view: WebView, req: WebResourceRequest): Boolean {
                val uri = req.url
                return when (uri.scheme) {
                    "http", "https" -> false
                    in OUR_SCHEMES -> {
                        import(uri.toString())
                        true
                    }
                    else -> {
                        runCatching { startActivity(Intent(Intent.ACTION_VIEW, uri)) }
                        true
                    }
                }
            }

            override fun onPageStarted(view: WebView, url: String, favicon: Bitmap?) {
                onApp = appUrl != null && url.startsWith(appUrl!!)
                if (!onApp) pageReady = false
            }

            override fun onPageFinished(view: WebView, url: String) {
                if (onApp) {
                    pageReady = true
                    pendingJs.forEach { web.evaluateJavascript(it, null) }
                    pendingJs.clear()
                }
            }

            override fun onReceivedError(view: WebView, req: WebResourceRequest, err: WebResourceError) {
                // Бэкенд мог ещё не подняться или как раз перезапускается.
                if (req.isForMainFrame && onApp) {
                    view.postDelayed({ appUrl?.let { load(it) } }, 700)
                }
            }
        }
        web.webChromeClient = object : WebChromeClient() {
            override fun onShowFileChooser(
                view: WebView, callback: ValueCallback<Array<Uri>>, params: FileChooserParams
            ): Boolean = chooseFile(callback, params)
        }
    }

    private fun load(url: String) {
        appUrl = url
        val cm = CookieManager.getInstance()
        cm.setCookie(url, "olcvpn_token=${Backend.token}; Path=/; HttpOnly; SameSite=Strict")
        cm.flush()
        web.loadUrl(url)
    }

    // ── Backend.Listener ───────────────────────────────────────────────

    override fun onHello(url: String) {
        if (url != appUrl || !onApp) load(url)
    }

    fun showCaptcha(url: String) {
        web.loadUrl(url)
    }

    fun leaveCaptcha() {
        if (!onApp) appUrl?.let { load(it) }
    }

    fun install(path: String) {
        Alerts.pendingInstall = null
        if (!packageManager.canRequestPackageInstalls()) {
            Alerts.pendingInstall = path
            Toast.makeText(this, "Разрешите olcvpn устанавливать приложения и вернитесь назад", Toast.LENGTH_LONG).show()
            startActivity(Intent(Settings.ACTION_MANAGE_UNKNOWN_APP_SOURCES, Uri.parse("package:$packageName")))
            return
        }
        val uri = Files.uri(this, "update", File(path).name)
        startActivity(
            Intent(Intent.ACTION_VIEW)
                .setDataAndType(uri, "application/vnd.android.package-archive")
                .addFlags(Intent.FLAG_GRANT_READ_URI_PERMISSION or Intent.FLAG_ACTIVITY_NEW_TASK)
        )
    }

    // ── что пришло снаружи: ссылки, «Поделиться» ───────────────────────

    override fun onNewIntent(intent: Intent) {
        super.onNewIntent(intent)
        setIntent(intent)
        handle(intent)
    }

    private fun handle(i: Intent?) {
        i ?: return
        when (i.action) {
            Intent.ACTION_VIEW -> i.data?.takeIf { it.scheme in OUR_SCHEMES }?.let { import(it.toString()) }
            Intent.ACTION_SEND -> {
                val text = i.getStringExtra(Intent.EXTRA_TEXT)
                @Suppress("DEPRECATION")
                val stream = i.getParcelableExtra<Uri>(Intent.EXTRA_STREAM)
                when {
                    !text.isNullOrBlank() -> import(text)
                    stream != null -> importPicture(stream)
                }
            }
        }
        i.action = null // не импортировать второй раз после поворота экрана
    }

    private fun import(text: String) {
        js("window.olcImport && olcImport(${JSONObject.quote(text)})")
    }

    private fun importPicture(uri: Uri) {
        thread {
            val data = runCatching { pictureData(uri) }.getOrNull()
            runOnUiThread {
                if (data == null) {
                    Toast.makeText(this, "Картинка не открылась", Toast.LENGTH_SHORT).show()
                } else {
                    js("window.olcImport && olcImport(null, ${JSONObject.quote(data)})")
                }
            }
        }
    }

    /** A picture as a data: URL, shrunk to what a QR code needs. */
    private fun pictureData(uri: Uri): String {
        val bounds = BitmapFactory.Options().apply { inJustDecodeBounds = true }
        contentResolver.openInputStream(uri)!!.use { BitmapFactory.decodeStream(it, null, bounds) }
        var sample = 1
        while (maxOf(bounds.outWidth, bounds.outHeight) / (sample * 2) >= 1600) sample *= 2
        val bmp = contentResolver.openInputStream(uri)!!.use {
            BitmapFactory.decodeStream(it, null, BitmapFactory.Options().apply { inSampleSize = sample })
        } ?: error("not an image")
        val out = ByteArrayOutputStream()
        bmp.compress(Bitmap.CompressFormat.PNG, 100, out)
        return "data:image/png;base64," + Base64.encodeToString(out.toByteArray(), Base64.NO_WRAP)
    }

    private fun js(code: String) {
        if (pageReady) web.evaluateJavascript(code, null) else pendingJs += code
    }

    // ── выбор файла: конфиг WireGuard, QR-код (с камеры или из галереи) ──

    private fun chooseFile(callback: ValueCallback<Array<Uri>>, params: WebChromeClient.FileChooserParams): Boolean {
        fileCallback?.onReceiveValue(null)
        fileCallback = callback
        val types = params.acceptTypes.filter { it.isNotBlank() }
        val images = types.isNotEmpty() && types.all { it.startsWith("image/") }
        val pick = Intent(Intent.ACTION_GET_CONTENT)
            .addCategory(Intent.CATEGORY_OPENABLE)
            .setType(if (images) "image/*" else "*/*")
        val chooser = Intent.createChooser(pick, if (images) "QR-код" else "Файл")
        cameraFile = null
        if (images) camera()?.let { chooser.putExtra(Intent.EXTRA_INITIAL_INTENTS, arrayOf(it)) }
        return try {
            startActivityForResult(chooser, REQ_FILE)
            true
        } catch (_: Exception) {
            fileCallback = null
            false
        }
    }

    /** A camera intent that saves the full photo where Files can serve it. */
    private fun camera(): Intent? {
        val f = File(cacheDir, "camera/qr.jpg")
        f.parentFile?.mkdirs()
        f.delete()
        f.createNewFile()
        val uri = Files.uri(this, "camera", f.name)
        val cam = Intent(MediaStore.ACTION_IMAGE_CAPTURE)
            .putExtra(MediaStore.EXTRA_OUTPUT, uri)
            .addFlags(Intent.FLAG_GRANT_WRITE_URI_PERMISSION or Intent.FLAG_GRANT_READ_URI_PERMISSION)
        val apps = packageManager.queryIntentActivities(cam, 0)
        if (apps.isEmpty()) return null
        apps.forEach {
            grantUriPermission(
                it.activityInfo.packageName, uri,
                Intent.FLAG_GRANT_WRITE_URI_PERMISSION or Intent.FLAG_GRANT_READ_URI_PERMISSION
            )
        }
        cameraFile = f
        return cam
    }

    @Deprecated("Deprecated in Java")
    override fun onActivityResult(requestCode: Int, resultCode: Int, data: Intent?) {
        super.onActivityResult(requestCode, resultCode, data)
        when (requestCode) {
            REQ_VPN -> answerVpn(resultCode == RESULT_OK)
            REQ_FILE -> {
                val cb = fileCallback ?: return
                fileCallback = null
                val shot = cameraFile?.takeIf { it.length() > 0 }
                val uri = when {
                    resultCode != RESULT_OK -> null
                    data?.data != null -> data.data
                    shot != null -> Files.uri(this, "camera", shot.name)
                    else -> null
                }
                cb.onReceiveValue(uri?.let { arrayOf(it) })
            }
        }
    }

    private fun answerVpn(ok: Boolean) {
        web.evaluateJavascript("window.olcVpnAnswer && olcVpnAnswer($ok)", null)
    }

    // ── жизненный цикл ──────────────────────────────────────────────────

    override fun onResume() {
        super.onResume()
        shown = this
        Alerts.clear(this)
        Alerts.pendingCaptcha?.let {
            Alerts.pendingCaptcha = null
            showCaptcha(it)
        }
        Alerts.pendingInstall?.let { if (packageManager.canRequestPackageInstalls()) install(it) }
    }

    override fun onPause() {
        if (shown === this) shown = null
        super.onPause()
    }

    override fun onDestroy() {
        Backend.listeners -= this
        web.destroy()
        super.onDestroy()
    }

    @Deprecated("Deprecated in Java")
    override fun onBackPressed() {
        // Со страницы капчи — обратно в приложение; из приложения — в фон,
        // как кнопка «закрыть» на ПК прячет окно в трей. Туннель работает.
        if (!onApp && appUrl != null) {
            load(appUrl!!)
        } else {
            moveTaskToBack(true)
        }
    }

    /**
     * window.olcAndroid. Методы работают только для своей страницы: в этом же
     * WebView бывает и страница капчи VK.
     */
    inner class Bridge {
        @JavascriptInterface
        fun prepareVpn() {
            runOnUiThread {
                if (!onApp) return@runOnUiThread
                val ask = VpnService.prepare(this@MainActivity)
                if (ask == null) answerVpn(true) else startActivityForResult(ask, REQ_VPN)
            }
        }

        @JavascriptInterface
        fun copy(text: String) {
            if (!onApp) return
            runOnUiThread {
                getSystemService(ClipboardManager::class.java)
                    .setPrimaryClip(ClipData.newPlainText("olcvpn", text))
            }
        }

        @JavascriptInterface
        fun share(text: String) {
            if (!onApp || text.isBlank()) return
            runOnUiThread {
                val send = Intent(Intent.ACTION_SEND).setType("text/plain").putExtra(Intent.EXTRA_TEXT, text)
                startActivity(Intent.createChooser(send, "Поделиться серверами"))
            }
        }

        @JavascriptInterface
        fun setTheme(light: Boolean) {
            if (!onApp) return
            runOnUiThread { paintBars(light) }
        }
    }

    private var paintedLight: Boolean? = null

    /** Фон под системными полосами и цвет их значков — как у страницы. */
    private fun paintBars(light: Boolean) {
        if (paintedLight == light) return
        paintedLight = light
        root.setBackgroundColor(if (light) 0xFFF2F4F8.toInt() else 0xFF0E1116.toInt())
        if (Build.VERSION.SDK_INT >= 30) {
            val mask = WindowInsetsController.APPEARANCE_LIGHT_STATUS_BARS or
                WindowInsetsController.APPEARANCE_LIGHT_NAVIGATION_BARS
            window.insetsController?.setSystemBarsAppearance(if (light) mask else 0, mask)
        } else {
            @Suppress("DEPRECATION")
            window.decorView.systemUiVisibility = if (light) View.SYSTEM_UI_FLAG_LIGHT_STATUS_BAR else 0
        }
    }
}
