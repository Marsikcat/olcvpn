package io.github.marsikcat.olcvpn

import android.content.ContentProvider
import android.content.ContentValues
import android.content.Context
import android.database.Cursor
import android.database.MatrixCursor
import android.net.Uri
import android.os.ParcelFileDescriptor
import android.provider.OpenableColumns
import java.io.File
import java.io.FileNotFoundException

/**
 * Отдаёт другим приложениям два наших файла: скачанный APK — установщику
 * Android, и место для снимка — камере, когда QR-код фотографируют. Это то,
 * что делает FileProvider из AndroidX, без самой библиотеки.
 */
class Files : ContentProvider() {

    companion object {
        fun uri(ctx: Context, kind: String, name: String): Uri =
            Uri.parse("content://${ctx.packageName}.files/$kind/${Uri.encode(name)}")
    }

    override fun onCreate() = true

    private fun file(uri: Uri): File {
        val ctx = context ?: throw FileNotFoundException()
        val seg = uri.pathSegments
        if (seg.size != 2) throw FileNotFoundException(uri.toString())
        val root = when (seg[0]) {
            "update" -> File(ctx.filesDir, "update")
            "camera" -> File(ctx.cacheDir, "camera")
            else -> throw FileNotFoundException(uri.toString())
        }
        val f = File(root, seg[1])
        if (!f.canonicalPath.startsWith(root.canonicalPath + File.separator)) {
            throw FileNotFoundException(uri.toString())
        }
        return f
    }

    override fun openFile(uri: Uri, mode: String): ParcelFileDescriptor =
        ParcelFileDescriptor.open(file(uri), ParcelFileDescriptor.parseMode(mode))

    override fun getType(uri: Uri): String =
        if (uri.pathSegments.firstOrNull() == "update") "application/vnd.android.package-archive" else "image/jpeg"

    override fun query(
        uri: Uri, projection: Array<out String>?, selection: String?,
        selectionArgs: Array<out String>?, sortOrder: String?
    ): Cursor {
        val f = file(uri)
        return MatrixCursor(arrayOf(OpenableColumns.DISPLAY_NAME, OpenableColumns.SIZE)).apply {
            addRow(arrayOf<Any>(f.name, f.length()))
        }
    }

    override fun insert(uri: Uri, values: ContentValues?): Uri? = null
    override fun delete(uri: Uri, selection: String?, selectionArgs: Array<out String>?) = 0
    override fun update(uri: Uri, values: ContentValues?, selection: String?, selectionArgs: Array<out String>?) = 0
}
