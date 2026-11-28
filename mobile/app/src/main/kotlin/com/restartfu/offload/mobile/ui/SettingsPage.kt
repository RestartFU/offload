package com.restartfu.offload.mobile.ui

import androidx.compose.foundation.background
import androidx.compose.foundation.border
import androidx.compose.foundation.clickable
import androidx.compose.foundation.layout.Arrangement
import androidx.compose.foundation.layout.Box
import androidx.compose.foundation.layout.Column
import androidx.compose.foundation.layout.ColumnScope
import androidx.compose.foundation.layout.Row
import androidx.compose.foundation.layout.Spacer
import androidx.compose.foundation.layout.WindowInsets
import androidx.compose.foundation.layout.fillMaxSize
import androidx.compose.foundation.layout.fillMaxWidth
import androidx.compose.foundation.layout.height
import androidx.compose.foundation.layout.heightIn
import androidx.compose.foundation.layout.navigationBars
import androidx.compose.foundation.layout.padding
import androidx.compose.foundation.layout.size
import androidx.compose.foundation.layout.width
import androidx.compose.foundation.layout.windowInsetsPadding
import androidx.compose.foundation.rememberScrollState
import androidx.compose.foundation.shape.RoundedCornerShape
import androidx.compose.foundation.verticalScroll
import androidx.compose.material3.Icon
import androidx.compose.material3.MaterialTheme
import androidx.compose.material3.Text
import androidx.compose.runtime.Composable
import androidx.compose.runtime.getValue
import androidx.compose.runtime.mutableStateOf
import androidx.compose.runtime.remember
import androidx.compose.runtime.rememberCoroutineScope
import androidx.compose.runtime.setValue
import androidx.compose.ui.Alignment
import androidx.compose.ui.Modifier
import androidx.compose.ui.draw.clip
import androidx.compose.ui.graphics.Color
import androidx.compose.ui.graphics.vector.ImageVector
import androidx.compose.ui.semantics.Role
import androidx.compose.ui.semantics.selected
import androidx.compose.ui.semantics.semantics
import androidx.compose.ui.text.font.FontWeight
import androidx.compose.ui.unit.dp
import com.restartfu.offload.mobile.AppModel
import com.restartfu.offload.mobile.Ask
import com.restartfu.offload.mobile.Link
import com.restartfu.offload.mobile.ThemeChoice
import kotlinx.coroutines.launch

@Composable
fun SettingsPage(model: AppModel, appVersion: String = "0.1.0") {
    val scope = rememberCoroutineScope()
    val p = model.pairing
    Column(Modifier.fillMaxSize()) {
        Header("Settings", model, onBack = { model.back() })
        Column(
            Modifier.weight(1f).verticalScroll(rememberScrollState()).padding(16.dp).windowInsetsPadding(WindowInsets.navigationBars),
            verticalArrangement = Arrangement.spacedBy(8.dp),
        ) {
            GroupTitle("Appearance")
            Group {
                Row(Modifier.padding(12.dp), horizontalArrangement = Arrangement.spacedBy(10.dp)) {
                    ThemeChoice.entries.forEach { t -> ThemeSwatch(t, model.theme == t, Modifier.weight(1f)) { model.chooseTheme(t) } }
                }
            }
            Spacer(Modifier.height(12.dp))
            GroupTitle("Computer")
            Group {
                SettingRow(Icons.Laptop, p?.name ?: "–", when (model.link) {
                    Link.Live -> "Connected · offload ${model.info?.version ?: ""}".trimEnd()
                    Link.Connecting -> "Connecting…"
                    is Link.Down -> "Not connected"
                })
                Rule()
                SettingRow(Icons.Terminal, "Address", p?.address ?: "–")
                Rule()
                SettingRow(Icons.Phone, "This phone's key", "Kept in Android Keystore. It stays valid across restarts until you forget it here or under Devices in the dashboard.")
            }
            Spacer(Modifier.height(4.dp))
            DangerButton(
                "Forget this computer",
                {
                    val name = p?.name ?: "the computer"
                    model.ask = Ask(
                        "Forget $name?",
                        "This phone will need a new pairing code to connect again. The computer forgets this phone's key too.",
                        "Forget", keep = "Stay paired",
                    ) {
                        scope.launch {
                            try {
                                model.unpair()
                            } catch (e: Exception) {
                                model.ask = Ask(
                                    "Couldn't tell $name",
                                    "It didn't answer, so it still accepts this phone's key. You can forget it on this phone only, then remove this phone under Devices in the dashboard.",
                                    "Forget on this phone only", keep = "Stay paired",
                                ) { model.unpairHere() }
                            }
                        }
                    }
                },
                Modifier.fillMaxWidth(),
            )
            Spacer(Modifier.height(16.dp))
            GroupTitle("About")
            Group {
                SettingRow(null, "offload for Android", "Version $appVersion · talks to offload dashboard on your computer over Tailscale")
            }
        }
    }
}

@Composable
private fun GroupTitle(text: String) {
    Text(text, style = MaterialTheme.typography.labelLarge, color = MaterialTheme.colorScheme.onSurfaceVariant, modifier = Modifier.padding(start = 4.dp, top = 4.dp))
}

@Composable
private fun Group(content: @Composable ColumnScope.() -> Unit) {
    val shape = RoundedCornerShape(12.dp)
    Column(
        Modifier.fillMaxWidth().clip(shape).background(MaterialTheme.colorScheme.surfaceContainer).border(1.dp, MaterialTheme.colorScheme.outlineVariant, shape),
        content = content,
    )
}

/** A row like xd's Settings: an icon on a tinted tile, a title and a summary under it. */
@Composable
private fun SettingRow(icon: ImageVector?, title: String, sub: String) {
    Row(Modifier.fillMaxWidth().heightIn(min = 64.dp).padding(horizontal = 14.dp, vertical = 12.dp), verticalAlignment = Alignment.CenterVertically) {
        if (icon != null) {
            Box(
                Modifier.size(36.dp).background(MaterialTheme.colorScheme.primary.copy(alpha = 0.14f), RoundedCornerShape(8.dp)),
                contentAlignment = Alignment.Center,
            ) { Icon(icon, null, Modifier.size(18.dp), tint = MaterialTheme.colorScheme.primary) }
            Spacer(Modifier.width(14.dp))
        }
        Column(Modifier.weight(1f)) {
            Text(title, style = MaterialTheme.typography.bodyLarge)
            Text(sub, style = MaterialTheme.typography.bodySmall, color = MaterialTheme.colorScheme.onSurfaceVariant)
        }
    }
}

@Composable
private fun ThemeSwatch(t: ThemeChoice, on: Boolean, modifier: Modifier, onClick: () -> Unit) {
    val shape = RoundedCornerShape(10.dp)
    Row(
        modifier
            .heightIn(min = 52.dp)
            .clip(shape)
            .background(if (on) MaterialTheme.colorScheme.surfaceContainerHighest else Color.Transparent)
            .border(if (on) 2.dp else 1.dp, if (on) MaterialTheme.colorScheme.primary else MaterialTheme.colorScheme.outlineVariant, shape)
            .semantics { selected = on }
            .clickable(role = Role.RadioButton, onClick = onClick)
            .padding(horizontal = 10.dp),
        verticalAlignment = Alignment.CenterVertically,
        horizontalArrangement = Arrangement.Center,
    ) {
        val sw = RoundedCornerShape(4.dp)
        Box(Modifier.size(18.dp, 14.dp).clip(sw).border(1.dp, MaterialTheme.colorScheme.outline, sw)) {
            when (t) {
                ThemeChoice.DARK -> Box(Modifier.fillMaxSize().background(Color(0xFF0B0B0B)))
                ThemeChoice.LIGHT -> Box(Modifier.fillMaxSize().background(Color(0xFFF3F4FB)))
                ThemeChoice.SYSTEM -> Row(Modifier.fillMaxSize()) {
                    Box(Modifier.weight(1f).fillMaxSize().background(Color(0xFF0B0B0B)))
                    Box(Modifier.weight(1f).fillMaxSize().background(Color(0xFFF3F4FB)))
                }
            }
        }
        Spacer(Modifier.width(8.dp))
        Text(t.label, style = MaterialTheme.typography.labelLarge, fontWeight = if (on) FontWeight.SemiBold else FontWeight.Normal)
    }
}
