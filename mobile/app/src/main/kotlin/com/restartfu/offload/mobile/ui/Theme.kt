package com.restartfu.offload.mobile.ui

import androidx.compose.foundation.background
import androidx.compose.foundation.layout.fillMaxSize
import androidx.compose.foundation.shape.RoundedCornerShape
import androidx.compose.material3.ColorScheme
import androidx.compose.material3.MaterialTheme
import androidx.compose.material3.Shapes
import androidx.compose.material3.Surface
import androidx.compose.material3.Typography
import androidx.compose.material3.darkColorScheme
import androidx.compose.material3.lightColorScheme
import androidx.compose.runtime.Composable
import androidx.compose.runtime.CompositionLocalProvider
import androidx.compose.runtime.Immutable
import androidx.compose.runtime.staticCompositionLocalOf
import androidx.compose.ui.Modifier
import androidx.compose.ui.graphics.Color
import androidx.compose.ui.graphics.compositeOver
import androidx.compose.ui.text.ExperimentalTextApi
import androidx.compose.ui.text.font.Font
import androidx.compose.ui.text.font.FontFamily
import androidx.compose.ui.text.font.FontVariation
import androidx.compose.ui.text.font.FontWeight
import androidx.compose.ui.unit.dp
import com.restartfu.offload.mobile.R

/**
 * xd's look: black glass (Dark) or frosted cool grey (Light), an indigo accent for actions,
 * focus and selection, and color otherwise kept for status.
 */
@Immutable
data class Signals(
    val working: Color,
    val waiting: Color,
    val done: Color,
    val danger: Color,
    val faint: Color,
    val rule: Color,
    val controlBorder: Color,
    val track: Color,
    val heat: List<Color>,
    val series: List<Color>,
)

@Immutable
data class Glass(val backdrop: Color, val pane: Color, val edge: Color) {
    /** The pane laid over the backdrop, opaque, for what floats over scrolling content. */
    val overlay: Color get() = pane.compositeOver(backdrop)
}

fun signals(light: Boolean) = if (!light) Signals(
    working = Color(0xFF63D8A8), waiting = Color(0xFFF0C06A), done = Color(0xFFA9ADFF), danger = Color(0xFFFF94A2),
    faint = Color(0xFF929292), rule = Color(0xFF262626), controlBorder = Color(0xFF6E6E6E), track = Color(0xFF222222),
    heat = listOf(Color(0xFF1A1A22), Color(0xFF262A55), Color(0xFF353B80), Color(0xFF4A50AE), Color(0xFF6267D6), Color(0xFF8B8FF8), Color(0xFFB9BCFF)),
    series = listOf(Color(0xFF8B8FF8), Color(0xFF63D8A8), Color(0xFFF0C06A)),
) else Signals(
    working = Color(0xFF17603F), waiting = Color(0xFF6C490A), done = Color(0xFF4045BB), danger = Color(0xFF97203A),
    faint = Color(0xFF5A6074), rule = Color(0xFFDADDE9), controlBorder = Color(0xFF7A7F92), track = Color(0xFFDFE2EE),
    heat = listOf(Color(0xFFE6E8F3), Color(0xFFCDD0F6), Color(0xFFAEB2F1), Color(0xFF8D92EA), Color(0xFF6B70E0), Color(0xFF5257D9), Color(0xFF3438A8)),
    series = listOf(Color(0xFF5257D9), Color(0xFF1F8A5B), Color(0xFFA0700F)),
)

fun glass(light: Boolean) = if (!light) {
    Glass(backdrop = Color(0xFF0B0B0B), pane = Color(0x0AFFFFFF), edge = Color(0x1FFFFFFF))
} else {
    Glass(backdrop = Color(0xFFE4E7F4), pane = Color(0x8CFFFFFF), edge = Color(0x1F1B1E2C))
}

val LocalSignals = staticCompositionLocalOf { signals(false) }
val LocalGlass = staticCompositionLocalOf { glass(false) }

fun colors(light: Boolean): ColorScheme = if (!light) darkColorScheme(
    primary = Color(0xFF8B8FF8), onPrimary = Color(0xFF0E1016),
    primaryContainer = Color(0xFF2A2A2A), onPrimaryContainer = Color(0xFFF2F2F2),
    secondary = Color(0xFFADADAD), onSecondary = Color(0xFF0B0B0B),
    secondaryContainer = Color(0xFF2A2A2A), onSecondaryContainer = Color(0xFFF2F2F2),
    tertiary = Color(0xFFA9ADFF), onTertiary = Color(0xFF0E1016),
    background = Color(0xFF0B0B0B), onBackground = Color(0xFFF2F2F2),
    surface = Color(0xFF0B0B0B), onSurface = Color(0xFFF2F2F2),
    surfaceVariant = Color(0xFF161616), onSurfaceVariant = Color(0xFFADADAD),
    surfaceContainerLowest = Color(0xFF050505), surfaceContainerLow = Color(0xFF0F0F0F),
    surfaceContainer = Color(0xFF111111), surfaceContainerHigh = Color(0xFF1B1B1B),
    surfaceContainerHighest = Color(0xFF222222), surfaceBright = Color(0xFF222222),
    inverseSurface = Color(0xFFF2F2F2), inverseOnSurface = Color(0xFF0F0F0F), inversePrimary = Color(0xFF4B4FB8),
    outline = Color(0xFF858585), outlineVariant = Color(0xFF2A2A2A),
    error = Color(0xFFFF94A2), onError = Color(0xFF351A20), errorContainer = Color(0xFF2A1416), onErrorContainer = Color(0xFFFFD9DE),
) else lightColorScheme(
    primary = Color(0xFF5257D9), onPrimary = Color(0xFFFFFFFF),
    primaryContainer = Color(0xFFDFE2F4), onPrimaryContainer = Color(0xFF1B1E2C),
    secondary = Color(0xFF4B5165), onSecondary = Color(0xFFFFFFFF),
    secondaryContainer = Color(0xFFDFE2F4), onSecondaryContainer = Color(0xFF1B1E2C),
    tertiary = Color(0xFF4045BB), onTertiary = Color(0xFFFFFFFF),
    background = Color(0xFFEEF0F9), onBackground = Color(0xFF1B1E2C),
    surface = Color(0xFFF3F4FB), onSurface = Color(0xFF1B1E2C),
    surfaceVariant = Color(0xFFECEEF7), onSurfaceVariant = Color(0xFF4B5165),
    surfaceContainerLowest = Color(0xFFFFFFFF), surfaceContainerLow = Color(0xFFF3F4FB),
    surfaceContainer = Color(0xFFF8F9FD), surfaceContainerHigh = Color(0xFFECEEF7),
    surfaceContainerHighest = Color(0xFFE4E7F2), surfaceBright = Color(0xFFFFFFFF),
    inverseSurface = Color(0xFF1B1E2C), inverseOnSurface = Color(0xFFF3F4FB), inversePrimary = Color(0xFFA3A7FF),
    outline = Color(0xFF6E7488), outlineVariant = Color(0xFFD3D7E6),
    error = Color(0xFF97203A), onError = Color(0xFFFFFFFF), errorContainer = Color(0xFFFBE9ED), onErrorContainer = Color(0xFF6E1426),
)

@OptIn(ExperimentalTextApi::class)
private fun inter(weight: FontWeight) = Font(R.font.inter, weight = weight, variationSettings = FontVariation.Settings(FontVariation.weight(weight.weight)))

val Inter = FontFamily(listOf(FontWeight.Normal, FontWeight.Medium, FontWeight.SemiBold, FontWeight.Bold).map(::inter))

@OptIn(ExperimentalTextApi::class)
val Mono = FontFamily(
    listOf(FontWeight.Normal, FontWeight.Medium).map {
        Font(R.font.jetbrains_mono, weight = it, variationSettings = FontVariation.Settings(FontVariation.weight(it.weight)))
    },
)

private val Type = Typography().let { b ->
    Typography(
        displayLarge = b.displayLarge.copy(fontFamily = Inter),
        displayMedium = b.displayMedium.copy(fontFamily = Inter),
        displaySmall = b.displaySmall.copy(fontFamily = Inter, fontWeight = FontWeight.SemiBold),
        headlineLarge = b.headlineLarge.copy(fontFamily = Inter),
        headlineMedium = b.headlineMedium.copy(fontFamily = Inter, fontWeight = FontWeight.SemiBold),
        headlineSmall = b.headlineSmall.copy(fontFamily = Inter, fontWeight = FontWeight.SemiBold),
        titleLarge = b.titleLarge.copy(fontFamily = Inter, fontWeight = FontWeight.SemiBold),
        titleMedium = b.titleMedium.copy(fontFamily = Inter, fontWeight = FontWeight.SemiBold),
        titleSmall = b.titleSmall.copy(fontFamily = Inter, fontWeight = FontWeight.Medium),
        bodyLarge = b.bodyLarge.copy(fontFamily = Inter),
        bodyMedium = b.bodyMedium.copy(fontFamily = Inter),
        bodySmall = b.bodySmall.copy(fontFamily = Inter),
        labelLarge = b.labelLarge.copy(fontFamily = Inter),
        labelMedium = b.labelMedium.copy(fontFamily = Inter),
        labelSmall = b.labelSmall.copy(fontFamily = Inter),
    )
}

val AppShapes = Shapes(
    extraSmall = RoundedCornerShape(4.dp),
    small = RoundedCornerShape(8.dp),
    medium = RoundedCornerShape(12.dp),
    large = RoundedCornerShape(16.dp),
    extraLarge = RoundedCornerShape(28.dp),
)

@Composable
fun OffloadTheme(light: Boolean, content: @Composable () -> Unit) {
    CompositionLocalProvider(LocalSignals provides signals(light), LocalGlass provides glass(light)) {
        MaterialTheme(colorScheme = colors(light), typography = Type, shapes = AppShapes) {
            Surface(
                Modifier.fillMaxSize().background(glass(light).backdrop),
                color = Color.Transparent,
                contentColor = MaterialTheme.colorScheme.onSurface,
                content = content,
            )
        }
    }
}
