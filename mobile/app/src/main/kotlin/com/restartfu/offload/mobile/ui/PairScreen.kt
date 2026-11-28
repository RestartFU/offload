package com.restartfu.offload.mobile.ui

import androidx.compose.foundation.Canvas
import androidx.compose.foundation.background
import androidx.compose.foundation.shape.CircleShape
import androidx.compose.foundation.layout.Arrangement
import androidx.compose.foundation.layout.Box
import androidx.compose.foundation.layout.Column
import androidx.compose.foundation.layout.Row
import androidx.compose.foundation.layout.Spacer
import androidx.compose.foundation.layout.WindowInsets
import androidx.compose.foundation.layout.fillMaxSize
import androidx.compose.foundation.layout.fillMaxWidth
import androidx.compose.foundation.layout.height
import androidx.compose.foundation.layout.heightIn
import androidx.compose.foundation.layout.imePadding
import androidx.compose.foundation.layout.padding
import androidx.compose.foundation.layout.safeDrawing
import androidx.compose.foundation.layout.size
import androidx.compose.foundation.layout.width
import androidx.compose.foundation.layout.windowInsetsPadding
import androidx.compose.foundation.rememberScrollState
import androidx.compose.foundation.shape.RoundedCornerShape
import androidx.compose.foundation.text.KeyboardActions
import androidx.compose.foundation.text.KeyboardOptions
import androidx.compose.foundation.verticalScroll
import androidx.compose.material3.Button
import androidx.compose.material3.CircularProgressIndicator
import androidx.compose.material3.MaterialTheme
import androidx.compose.material3.OutlinedTextField
import androidx.compose.material3.OutlinedTextFieldDefaults
import androidx.compose.material3.Text
import androidx.compose.runtime.Composable
import androidx.compose.runtime.getValue
import androidx.compose.runtime.mutableStateOf
import androidx.compose.runtime.remember
import androidx.compose.runtime.rememberCoroutineScope
import androidx.compose.runtime.saveable.rememberSaveable
import androidx.compose.runtime.setValue
import androidx.compose.ui.Alignment
import androidx.compose.ui.Modifier
import androidx.compose.ui.geometry.CornerRadius
import androidx.compose.ui.geometry.Offset
import androidx.compose.ui.geometry.Size
import androidx.compose.ui.text.TextStyle
import androidx.compose.ui.text.font.FontWeight
import androidx.compose.ui.text.input.ImeAction
import androidx.compose.ui.text.input.KeyboardCapitalization
import androidx.compose.ui.text.input.KeyboardType
import androidx.compose.ui.unit.dp
import androidx.compose.ui.unit.sp
import com.restartfu.offload.mobile.AppModel
import com.restartfu.offload.mobile.splitAddress
import kotlinx.coroutines.launch

/** offload's mark: four cores, one busy, two half, one idle. */
@Composable
fun Mark(size: androidx.compose.ui.unit.Dp = 44.dp) {
    val c = MaterialTheme.colorScheme.primary
    Canvas(Modifier.size(size)) {
        val g = this.size.width * 0.12f
        val s = (this.size.width - g) / 2
        val r = CornerRadius(s * 0.22f)
        drawRoundRect(c, Offset(0f, 0f), Size(s, s), r)
        drawRoundRect(c.copy(alpha = 0.55f), Offset(s + g, 0f), Size(s, s), r)
        drawRoundRect(c.copy(alpha = 0.55f), Offset(0f, s + g), Size(s, s), r)
        drawRoundRect(c.copy(alpha = 0.25f), Offset(s + g, s + g), Size(s, s), r)
    }
}

@Composable
fun PairScreen(model: AppModel, deviceName: String, initialCode: String = "", initialError: String? = null) {
    var address by rememberSaveable { mutableStateOf(model.lastAddress) }
    var code by rememberSaveable { mutableStateOf(initialCode) }
    var error by remember { mutableStateOf(initialError) }
    var busy by remember { mutableStateOf(false) }
    val scope = rememberCoroutineScope()
    val addressOk = splitAddress(address) != null
    val codeOk = code.count { it.isLetterOrDigit() } == 8
    fun submit() {
        if (!addressOk || !codeOk || busy) return
        busy = true
        error = null
        scope.launch {
            try {
                model.pair(address, code, deviceName)
            } catch (e: Exception) {
                error = (e.message ?: "Pairing failed").replaceFirstChar { it.uppercase() }
            } finally {
                busy = false
            }
        }
    }
    Box(Modifier.fillMaxSize().windowInsetsPadding(WindowInsets.safeDrawing).imePadding(), contentAlignment = Alignment.TopCenter) {
        Column(
            Modifier.measure().fillMaxWidth().verticalScroll(rememberScrollState()).padding(horizontal = 24.dp, vertical = 40.dp),
        ) {
            Mark()
            Spacer(Modifier.height(28.dp))
            Text("Connect to your computer", style = MaterialTheme.typography.headlineMedium)
            Spacer(Modifier.height(10.dp))
            Text(
                "This phone shows your offload runners through the dashboard on your computer.",
                style = MaterialTheme.typography.bodyLarge, color = MaterialTheme.colorScheme.onSurfaceVariant,
            )
            model.unpairedReason?.let {
                Spacer(Modifier.height(20.dp))
                Notice(it, Mark.FAILED)
            }
            Spacer(Modifier.height(28.dp))
            Steps()
            Spacer(Modifier.height(28.dp))
            Field("Computer", address, { address = it; error = null }, "laptop or 100.101.102.103",
                hint = "Its Tailscale name or address, as the dashboard shows it",
                keyboard = KeyboardOptions(keyboardType = KeyboardType.Uri, imeAction = ImeAction.Next, autoCorrectEnabled = false))
            Spacer(Modifier.height(16.dp))
            Field(
                "Pairing code", code, { v -> code = v.uppercase().filter { it.isLetterOrDigit() || it == '-' }.take(9); error = null }, "ABCD-EFGH",
                mono = true,
                keyboard = KeyboardOptions(capitalization = KeyboardCapitalization.Characters, keyboardType = KeyboardType.Ascii, imeAction = ImeAction.Done, autoCorrectEnabled = false),
                onDone = ::submit,
            )
            if (error != null) {
                Spacer(Modifier.height(16.dp))
                Notice(error!!, Mark.FAILED)
            }
            Spacer(Modifier.height(24.dp))
            Button(
                onClick = ::submit, enabled = addressOk && codeOk && !busy,
                modifier = Modifier.fillMaxWidth().heightIn(min = 52.dp), shape = RoundedCornerShape(12.dp),
            ) {
                if (busy) {
                    CircularProgressIndicator(Modifier.size(18.dp), strokeWidth = 2.dp, color = MaterialTheme.colorScheme.onPrimary)
                    Spacer(Modifier.width(10.dp))
                    Text("Pairing…", fontWeight = FontWeight.SemiBold)
                } else {
                    Text("Pair", fontWeight = FontWeight.SemiBold)
                }
            }
            Spacer(Modifier.height(20.dp))
            Text(
                "Both devices need Tailscale on. The phone keeps its own key, so it stays paired across restarts until you forget it on either side.",
                style = MaterialTheme.typography.bodySmall, color = LocalSignals.current.faint,
            )
        }
    }
}

@Composable
private fun Steps() {
    val steps = listOf(
        "On your computer, run" to "offload dashboard",
        "Open Devices and press" to "Pair a phone",
        "Type the address and code it shows" to null,
    )
    Column(verticalArrangement = Arrangement.spacedBy(14.dp)) {
        steps.forEachIndexed { i, (text, code) ->
            Row(verticalAlignment = Alignment.CenterVertically) {
                Box(
                    Modifier.size(26.dp).background(MaterialTheme.colorScheme.primary.copy(alpha = 0.14f), CircleShape),
                    contentAlignment = Alignment.Center,
                ) {
                    Text("${i + 1}", style = MaterialTheme.typography.labelLarge, color = MaterialTheme.colorScheme.primary, fontWeight = FontWeight.SemiBold)
                }
                Spacer(Modifier.width(10.dp))
                Text(text, style = MaterialTheme.typography.bodyMedium)
                if (code != null) {
                    Spacer(Modifier.width(8.dp))
                    Chip(code, mono = code.startsWith("offload"))
                }
            }
        }
    }
}

@Composable
fun Field(
    label: String,
    value: String,
    onChange: (String) -> Unit,
    placeholder: String,
    hint: String? = null,
    mono: Boolean = false,
    keyboard: KeyboardOptions = KeyboardOptions.Default,
    onDone: (() -> Unit)? = null,
    modifier: Modifier = Modifier,
) {
    Column(modifier) {
        Text(label, style = MaterialTheme.typography.labelLarge, fontWeight = FontWeight.Medium)
        Spacer(Modifier.height(6.dp))
        val style = if (mono) TextStyle(fontFamily = Mono, fontSize = 18.sp, letterSpacing = 1.5.sp) else MaterialTheme.typography.bodyLarge
        OutlinedTextField(
            value = value, onValueChange = onChange, singleLine = true, textStyle = style,
            placeholder = { Text(placeholder, style = style, color = LocalSignals.current.faint) },
            keyboardOptions = keyboard,
            keyboardActions = KeyboardActions(onDone = { onDone?.invoke() }),
            shape = RoundedCornerShape(10.dp),
            colors = OutlinedTextFieldDefaults.colors(unfocusedBorderColor = LocalSignals.current.controlBorder),
            modifier = Modifier.fillMaxWidth(),
        )
        if (hint != null) {
            Spacer(Modifier.height(6.dp))
            Text(hint, style = MaterialTheme.typography.bodySmall, color = MaterialTheme.colorScheme.onSurfaceVariant)
        }
    }
}
