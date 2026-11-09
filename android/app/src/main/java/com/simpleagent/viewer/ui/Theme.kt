package com.simpleagent.viewer.ui

import androidx.compose.material3.MaterialTheme
import androidx.compose.material3.darkColorScheme
import androidx.compose.runtime.Composable
import androidx.compose.ui.graphics.Color

// 暗色配色，取桌面版 dsh 主题的基调（背景 #101418、卡片略亮、文字偏灰）。
// 手机上没有 GPU 常驻的字体缓存，颜色对比度要够，别用太低饱和的灰。
private val Background = Color(0xFF101418)
private val Surface = Color(0xFF171C22)
private val SurfaceHigh = Color(0xFF1E242B)
private val TextPrimary = Color(0xFFE6E9EC)
private val TextMuted = Color(0xFF9AA4AF)
private val Accent = Color(0xFF6BA6FF)
private val Danger = Color(0xFFF2746E)
private val Warn = Color(0xFFE0B341)
private val Ok = Color(0xFF62C08B)

private val scheme = darkColorScheme(
    primary = Accent,
    onPrimary = Color(0xFF0B1220),
    background = Background,
    onBackground = TextPrimary,
    surface = Surface,
    onSurface = TextPrimary,
    surfaceVariant = SurfaceHigh,
    onSurfaceVariant = TextMuted,
    error = Danger,
)

/** 语义色（Compose 的 ColorScheme 没有这些名字，单独暴露）。 */
object ViewerColors {
    val muted: Color @Composable get() = TextMuted
    val danger: Color @Composable get() = Danger
    val warn: Color @Composable get() = Warn
    val ok: Color @Composable get() = Ok
    val surfaceHigh: Color @Composable get() = SurfaceHigh
    val bubble: Color @Composable get() = Color(0xFF23303F)
    val codeBg: Color @Composable get() = Color(0xFF0C1015)
    val accent: Color @Composable get() = Accent
}

@Composable
fun ViewerTheme(content: @Composable () -> Unit) {
    MaterialTheme(colorScheme = scheme, content = content)
}
