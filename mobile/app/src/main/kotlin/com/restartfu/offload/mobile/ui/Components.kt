package com.restartfu.offload.mobile.ui

import androidx.compose.foundation.BorderStroke
import androidx.compose.foundation.Canvas
import androidx.compose.foundation.background
import androidx.compose.foundation.border
import androidx.compose.foundation.clickable
import androidx.compose.foundation.layout.Arrangement
import androidx.compose.foundation.layout.Box
import androidx.compose.foundation.layout.Column
import androidx.compose.foundation.layout.ColumnScope
import androidx.compose.foundation.layout.Row
import androidx.compose.foundation.layout.RowScope
import androidx.compose.foundation.interaction.MutableInteractionSource
import androidx.compose.foundation.layout.Spacer
import androidx.compose.foundation.layout.WindowInsets
import androidx.compose.foundation.layout.fillMaxSize
import androidx.compose.foundation.layout.navigationBars
import androidx.compose.foundation.layout.windowInsetsPadding
import androidx.compose.foundation.layout.fillMaxWidth
import androidx.compose.foundation.layout.height
import androidx.compose.foundation.layout.heightIn
import androidx.compose.foundation.layout.padding
import androidx.compose.foundation.layout.size
import androidx.compose.foundation.layout.width
import androidx.compose.foundation.layout.widthIn
import androidx.compose.foundation.shape.CircleShape
import androidx.compose.foundation.shape.RoundedCornerShape
import androidx.compose.material3.Button
import androidx.compose.material3.ButtonDefaults
import androidx.compose.material3.Icon
import androidx.compose.material3.IconButton
import androidx.compose.material3.MaterialTheme
import androidx.compose.material3.OutlinedButton
import androidx.compose.material3.Surface
import androidx.compose.material3.Text
import androidx.compose.material3.TextButton
import androidx.compose.runtime.Composable
import androidx.compose.runtime.remember
import androidx.compose.ui.Alignment
import androidx.compose.ui.Modifier
import androidx.compose.ui.draw.clip
import androidx.compose.ui.geometry.CornerRadius
import androidx.compose.ui.geometry.Offset
import androidx.compose.ui.geometry.Size
import androidx.compose.ui.graphics.Color
import androidx.compose.ui.graphics.Path
import androidx.compose.ui.graphics.StrokeCap
import androidx.compose.ui.graphics.drawscope.Stroke
import androidx.compose.ui.graphics.vector.ImageVector
import androidx.compose.ui.semantics.Role
import androidx.compose.ui.semantics.contentDescription
import androidx.compose.ui.semantics.paneTitle
import androidx.compose.ui.semantics.semantics
import androidx.compose.ui.semantics.selected
import androidx.compose.ui.text.font.FontWeight
import androidx.compose.ui.text.style.TextOverflow
import androidx.compose.ui.unit.Dp
import androidx.compose.ui.unit.dp
import androidx.compose.ui.unit.sp
import com.restartfu.offload.mobile.Ask

/** The marks xd uses for state, each always next to its word: working, waiting, done, paused, plus failed and stopped. */
enum class Mark { WORKING, WAITING, DONE, PAUSED, FAILED, STOPPED }

@Composable
fun markColor(m: Mark): Color {
    val s = LocalSignals.current
    return when (m) {
        Mark.WORKING -> s.working
        Mark.WAITING -> s.waiting
        Mark.DONE -> s.done
        Mark.FAILED -> s.danger
        Mark.PAUSED, Mark.STOPPED -> MaterialTheme.colorScheme.onSurfaceVariant
    }
}

@Composable
fun StatusMark(mark: Mark, size: Dp = 10.dp) {
    val c = markColor(mark)
    Canvas(Modifier.size(size)) {
        val w = this.size.width
        val stroke = w * 0.18f
        when (mark) {
            Mark.WORKING -> drawCircle(c, radius = w * 0.42f)
            Mark.WAITING -> drawCircle(c, radius = w * 0.38f, style = Stroke(stroke))
            Mark.DONE -> drawPath(
                Path().apply { moveTo(w * 0.12f, w * 0.52f); lineTo(w * 0.4f, w * 0.8f); lineTo(w * 0.9f, w * 0.22f) },
                c, style = Stroke(stroke * 1.15f, cap = StrokeCap.Round),
            )
            Mark.PAUSED -> {
                drawLine(c, Offset(w * 0.3f, w * 0.15f), Offset(w * 0.3f, w * 0.85f), stroke * 1.2f, StrokeCap.Round)
                drawLine(c, Offset(w * 0.7f, w * 0.15f), Offset(w * 0.7f, w * 0.85f), stroke * 1.2f, StrokeCap.Round)
            }
            Mark.FAILED -> {
                drawLine(c, Offset(w * 0.18f, w * 0.18f), Offset(w * 0.82f, w * 0.82f), stroke * 1.15f, StrokeCap.Round)
                drawLine(c, Offset(w * 0.82f, w * 0.18f), Offset(w * 0.18f, w * 0.82f), stroke * 1.15f, StrokeCap.Round)
            }
            Mark.STOPPED -> drawLine(c, Offset(w * 0.15f, w * 0.5f), Offset(w * 0.85f, w * 0.5f), stroke * 1.2f, StrokeCap.Round)
        }
    }
}

/** A mark and its word, in the mark's color. */
@Composable
fun StatusLabel(mark: Mark, text: String, modifier: Modifier = Modifier) {
    Row(modifier, verticalAlignment = Alignment.CenterVertically, horizontalArrangement = Arrangement.spacedBy(6.dp)) {
        StatusMark(mark)
        Text(text, style = MaterialTheme.typography.labelMedium, color = markColor(mark), maxLines = 1)
    }
}

@Composable
fun Panel(modifier: Modifier = Modifier, padding: Dp = 16.dp, onClick: (() -> Unit)? = null, content: @Composable ColumnScope.() -> Unit) {
    val shape = MaterialTheme.shapes.medium
    Column(
        modifier
            .fillMaxWidth()
            .clip(shape)
            .background(MaterialTheme.colorScheme.surfaceContainer)
            .border(1.dp, MaterialTheme.colorScheme.outlineVariant, shape)
            .let { if (onClick != null) it.clickable(role = Role.Button, onClick = onClick) else it }
            .padding(padding),
        content = content,
    )
}

@Composable
fun SectionTitle(title: String, sub: String? = null, modifier: Modifier = Modifier, trailing: @Composable RowScope.() -> Unit = {}) {
    Row(modifier.fillMaxWidth().padding(top = 8.dp, bottom = 8.dp), verticalAlignment = Alignment.CenterVertically) {
        Column(Modifier.weight(1f)) {
            Text(title, style = MaterialTheme.typography.titleSmall.copy(fontWeight = FontWeight.SemiBold))
            if (sub != null) Text(sub, style = MaterialTheme.typography.bodySmall, color = MaterialTheme.colorScheme.onSurfaceVariant)
        }
        trailing()
    }
}

/** A thin bar: how full something is. [marker] draws a tick, such as a guarantee. */
@Composable
fun Meter(frac: Double, modifier: Modifier = Modifier, color: Color = MaterialTheme.colorScheme.primary, marker: Double? = null) {
    val track = LocalSignals.current.track
    val tick = MaterialTheme.colorScheme.onSurface
    Canvas(modifier.fillMaxWidth().height(6.dp)) {
        val r = CornerRadius(size.height / 2)
        drawRoundRect(track, cornerRadius = r)
        val f = frac.coerceIn(0.0, 1.0).toFloat()
        if (f > 0) drawRoundRect(color, size = Size(size.width * f.coerceAtLeast(0.02f), size.height), cornerRadius = r)
        if (marker != null) {
            val x = size.width * marker.coerceIn(0.0, 1.0).toFloat()
            drawLine(tick, Offset(x, -2f), Offset(x, size.height + 2f), 2f)
        }
    }
}

@Composable
fun meterColor(frac: Double, warnAt: Double = 0.9): Color =
    if (frac >= warnAt) LocalSignals.current.waiting else MaterialTheme.colorScheme.primary

/** A label, a large value and a line under it. */
@Composable
fun Stat(label: String, value: String, sub: String?, modifier: Modifier = Modifier, unit: String? = null, big: Boolean = false) {
    Panel(modifier) {
        Text(label, style = MaterialTheme.typography.labelMedium, color = MaterialTheme.colorScheme.onSurfaceVariant)
        Row(verticalAlignment = Alignment.Bottom) {
            Text(
                value,
                style = if (big) MaterialTheme.typography.displaySmall else MaterialTheme.typography.headlineSmall,
                maxLines = 1,
            )
            if (unit != null) {
                Text(
                    " $unit", style = MaterialTheme.typography.titleSmall, color = MaterialTheme.colorScheme.onSurfaceVariant,
                    modifier = Modifier.padding(bottom = if (big) 6.dp else 3.dp),
                )
            }
        }
        if (sub != null) {
            Spacer(Modifier.height(2.dp))
            Text(sub, style = MaterialTheme.typography.bodySmall, color = MaterialTheme.colorScheme.onSurfaceVariant)
        }
    }
}

/** Choices as xd's filter chips: outlined, the chosen one filled and without its outline. */
@Composable
fun Segmented(options: List<Pair<String, String>>, selected: String, onSelect: (String) -> Unit, modifier: Modifier = Modifier) {
    Row(modifier, horizontalArrangement = Arrangement.spacedBy(8.dp)) {
        for ((key, label) in options) {
            val on = key == selected
            val shape = RoundedCornerShape(8.dp)
            Box(
                Modifier
                    .heightIn(min = 40.dp)
                    .clip(shape)
                    .background(if (on) MaterialTheme.colorScheme.surfaceBright else Color.Transparent)
                    .border(1.dp, if (on) MaterialTheme.colorScheme.outlineVariant else LocalSignals.current.controlBorder, shape)
                    .semantics { this.selected = on }
                    .clickable(role = Role.Tab) { onSelect(key) }
                    .padding(horizontal = 14.dp),
                contentAlignment = Alignment.Center,
            ) {
                Text(
                    label, style = MaterialTheme.typography.labelLarge, maxLines = 1,
                    color = MaterialTheme.colorScheme.onSurface,
                    fontWeight = if (on) FontWeight.SemiBold else FontWeight.Normal,
                )
            }
        }
    }
}

/** An icon alone, always named for screen readers. */
@Composable
fun IconAction(icon: ImageVector, name: String, onClick: () -> Unit, enabled: Boolean = true, tint: Color = MaterialTheme.colorScheme.onSurfaceVariant) {
    IconButton(onClick = onClick, enabled = enabled, modifier = Modifier.semantics { contentDescription = name }) {
        Icon(icon, contentDescription = null, tint = tint, modifier = Modifier.size(20.dp))
    }
}

@Composable
fun Empty(title: String, body: String? = null, modifier: Modifier = Modifier, action: (@Composable () -> Unit)? = null) {
    Column(modifier.fillMaxWidth().padding(vertical = 40.dp, horizontal = 24.dp), horizontalAlignment = Alignment.CenterHorizontally) {
        Text(title, style = MaterialTheme.typography.titleMedium)
        if (body != null) {
            Spacer(Modifier.height(6.dp))
            Text(
                body, style = MaterialTheme.typography.bodyMedium, color = MaterialTheme.colorScheme.onSurfaceVariant,
                textAlign = androidx.compose.ui.text.style.TextAlign.Center,
            )
        }
        if (action != null) {
            Spacer(Modifier.height(16.dp))
            action()
        }
    }
}

/** A warning in the page: an icon and words, no box, as xd's host notices. */
@Composable
fun Notice(text: String, mark: Mark = Mark.WAITING, modifier: Modifier = Modifier) {
    Row(modifier.fillMaxWidth().padding(vertical = 4.dp), horizontalArrangement = Arrangement.spacedBy(10.dp)) {
        Box(Modifier.padding(top = 4.dp)) { StatusMark(mark) }
        Text(text, style = MaterialTheme.typography.bodySmall, color = MaterialTheme.colorScheme.onSurfaceVariant)
    }
}

@Composable
fun KeyValue(key: String, value: String, mono: Boolean = false) {
    Row(Modifier.fillMaxWidth().padding(vertical = 6.dp), horizontalArrangement = Arrangement.spacedBy(16.dp)) {
        Text(key, style = MaterialTheme.typography.bodySmall, color = MaterialTheme.colorScheme.onSurfaceVariant, modifier = Modifier.width(96.dp))
        Text(
            value,
            style = if (mono) MaterialTheme.typography.bodySmall.copy(fontFamily = Mono, fontSize = 12.5.sp) else MaterialTheme.typography.bodyMedium,
            modifier = Modifier.weight(1f),
        )
    }
}

@Composable
fun Rule() {
    Box(Modifier.fillMaxWidth().height(1.dp).background(MaterialTheme.colorScheme.outlineVariant))
}

/** A question before something is removed or stopped: a sheet at the foot of the screen, the way out last. */
@Composable
fun ConfirmSheet(ask: Ask, onDismiss: () -> Unit) {
    Box(
        Modifier.fillMaxSize().background(Color.Black.copy(alpha = 0.6f))
            .clickable(interactionSource = remember { MutableInteractionSource() }, indication = null, onClick = onDismiss),
        contentAlignment = Alignment.BottomCenter,
    ) {
        val shape = RoundedCornerShape(topStart = 20.dp, topEnd = 20.dp)
        Column(
            Modifier.measure().fillMaxWidth().clip(shape).background(MaterialTheme.colorScheme.surfaceContainerHigh)
                .border(1.dp, LocalGlass.current.edge, shape)
                .clickable(interactionSource = remember { MutableInteractionSource() }, indication = null) {}
                .semantics { paneTitle = ask.title }
                .windowInsetsPadding(WindowInsets.navigationBars).padding(24.dp),
        ) {
            Text(ask.title, style = MaterialTheme.typography.titleLarge)
            Spacer(Modifier.height(8.dp))
            Text(ask.body, style = MaterialTheme.typography.bodyMedium, color = MaterialTheme.colorScheme.onSurfaceVariant)
            Spacer(Modifier.height(24.dp))
            Button(
                onClick = { onDismiss(); ask.run() },
                modifier = Modifier.fillMaxWidth().heightIn(min = 52.dp), shape = RoundedCornerShape(12.dp),
                colors = ButtonDefaults.buttonColors(containerColor = MaterialTheme.colorScheme.error, contentColor = MaterialTheme.colorScheme.onError),
            ) { Text(ask.action, fontWeight = FontWeight.SemiBold) }
            Spacer(Modifier.height(8.dp))
            TextButton(onClick = onDismiss, modifier = Modifier.fillMaxWidth().heightIn(min = 48.dp)) {
                Text(ask.keep, color = MaterialTheme.colorScheme.onSurface)
            }
        }
    }
}

@Composable
fun DangerButton(text: String, onClick: () -> Unit, modifier: Modifier = Modifier, enabled: Boolean = true) {
    OutlinedButton(
        onClick = onClick, enabled = enabled, modifier = modifier.heightIn(min = 48.dp),
        border = BorderStroke(1.dp, if (enabled) MaterialTheme.colorScheme.error.copy(alpha = 0.6f) else MaterialTheme.colorScheme.outlineVariant),
        shape = RoundedCornerShape(10.dp),
    ) {
        Text(text, color = MaterialTheme.colorScheme.error, fontWeight = FontWeight.Medium, maxLines = 1, overflow = TextOverflow.Ellipsis)
    }
}

@Composable
fun Chip(text: String, modifier: Modifier = Modifier, mono: Boolean = false) {
    Surface(
        modifier, shape = RoundedCornerShape(6.dp), color = MaterialTheme.colorScheme.surfaceBright,
        contentColor = MaterialTheme.colorScheme.onSurface, border = BorderStroke(1.dp, MaterialTheme.colorScheme.outlineVariant),
    ) {
        Text(
            text, modifier = Modifier.padding(horizontal = 7.dp, vertical = 3.dp),
            style = if (mono) MaterialTheme.typography.labelMedium.copy(fontFamily = Mono) else MaterialTheme.typography.labelMedium,
        )
    }
}

/** Keeps reading surfaces to a comfortable measure on wide screens. */
fun Modifier.measure(): Modifier = this.widthIn(max = 720.dp)
