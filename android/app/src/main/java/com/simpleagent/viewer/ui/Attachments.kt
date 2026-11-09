package com.simpleagent.viewer.ui

import android.content.Context
import android.graphics.BitmapFactory
import android.net.Uri
import android.provider.OpenableColumns
import java.io.ByteArrayOutputStream

/**
 * 读取用户选中的图片（相册 / 拍照 / 文件）。
 *
 * 统一转成 PNG：执行端只需要"能解码的图片字节"，而原格式五花八门（HEIC 在
 * 某些机型上执行端解不了）。体积也要压一下——手机拍的照片动辄 5–10MB，
 * 跨网络传到执行端很慢。
 */
object Attachments {

    /** 单张图片上传后的上限（压缩目标）。 */
    private const val MAX_DIMENSION = 1600
    private const val JPEG_QUALITY = 85

    /** 读出来的图片数据。 */
    data class Loaded(val bytes: ByteArray, val name: String, val mediaType: String)

    /**
     * 从一个 content:// 或 file:// URI 读出图片，必要时压缩。
     *
     * **会做 IO**，调用方放后台线程。
     */
    fun load(context: Context, uri: Uri): Loaded? {
        val name = displayName(context, uri) ?: "image"
        val raw = context.contentResolver.openInputStream(uri)?.use { it.readBytes() } ?: return null
        if (raw.isEmpty()) return null

        // 先看原始尺寸：小图（截图、图标）原样上传，避免无谓的二次编码
        val bounds = BitmapFactory.Options().apply { inJustDecodeBounds = true }
        BitmapFactory.decodeByteArray(raw, 0, raw.size, bounds)
        val longest = maxOf(bounds.outWidth, bounds.outHeight)
        if (longest <= MAX_DIMENSION) {
            return Loaded(raw, name, sniffMediaType(raw))
        }

        // 大图：按比例缩到 MAX_DIMENSION 再编成 PNG
        val sample = BitmapFactory.Options().apply {
            inSampleSize = sampleSize(bounds.outWidth, bounds.outHeight, MAX_DIMENSION)
        }
        val bitmap = BitmapFactory.decodeByteArray(raw, 0, raw.size, sample) ?: return Loaded(raw, name, sniffMediaType(raw))
        val scaled = scale(bitmap, MAX_DIMENSION)
        val out = ByteArrayOutputStream()
        scaled.compress(android.graphics.Bitmap.CompressFormat.PNG, 100, out)
        if (!scaled.isRecycled) scaled.recycle()
        return Loaded(out.toByteArray(), name, "image/png")
    }

    private fun sampleSize(width: Int, height: Int, target: Int): Int {
        var size = 1
        var w = width
        var h = height
        while (w / 2 >= target && h / 2 >= target) {
            w /= 2
            h /= 2
            size *= 2
        }
        return size.coerceAtLeast(1)
    }

    private fun scale(bitmap: android.graphics.Bitmap, target: Int): android.graphics.Bitmap {
        val longest = maxOf(bitmap.width, bitmap.height)
        if (longest <= target) return bitmap
        val ratio = target.toFloat() / longest
        return android.graphics.Bitmap.createScaledBitmap(
            bitmap,
            (bitmap.width * ratio).toInt().coerceAtLeast(1),
            (bitmap.height * ratio).toInt().coerceAtLeast(1),
            true,
        )
    }

    /** 从文件头判断媒体类型（执行端按 media_type 决定怎么展示）。 */
    fun sniffMediaType(data: ByteArray): String = when {
        data.size >= 8 && data[0] == 0x89.toByte() && data[1] == 'P'.code.toByte() -> "image/png"
        data.size >= 3 && data[0] == 0xFF.toByte() && data[1] == 0xD8.toByte() -> "image/jpeg"
        data.size >= 6 && data[0] == 'G'.code.toByte() && data[1] == 'I'.code.toByte() -> "image/gif"
        data.size >= 12 && data[8] == 'W'.code.toByte() && data[9] == 'E'.code.toByte() -> "image/webp"
        else -> "image/png"
    }

    /** 取文件的显示名（相册里通常是 IMG_1234.jpg）。 */
    private fun displayName(context: Context, uri: Uri): String? {
        if (uri.scheme == "file") return uri.lastPathSegment
        return runCatching {
            context.contentResolver.query(uri, arrayOf(OpenableColumns.DISPLAY_NAME), null, null, null)
                ?.use { cursor ->
                    if (cursor.moveToFirst()) cursor.getString(0) else null
                }
        }.getOrNull()
    }
}
