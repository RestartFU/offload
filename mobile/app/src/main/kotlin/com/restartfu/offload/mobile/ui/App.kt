package com.restartfu.offload.mobile.ui

import androidx.activity.compose.BackHandler
import androidx.compose.animation.AnimatedContent
import androidx.compose.animation.core.tween
import androidx.compose.animation.fadeIn
import androidx.compose.animation.fadeOut
import androidx.compose.animation.slideInHorizontally
import androidx.compose.animation.slideOutHorizontally
import androidx.compose.animation.togetherWith
import androidx.compose.foundation.background
import androidx.compose.foundation.border
import androidx.compose.foundation.clickable
import androidx.compose.foundation.layout.Arrangement
import androidx.compose.foundation.layout.Box
import androidx.compose.foundation.layout.Column
import androidx.compose.foundation.layout.PaddingValues
import androidx.compose.foundation.layout.Row
import androidx.compose.foundation.layout.Spacer
import androidx.compose.foundation.layout.WindowInsets
import androidx.compose.foundation.layout.fillMaxSize
import androidx.compose.foundation.layout.fillMaxWidth
import androidx.compose.foundation.layout.height
import androidx.compose.foundation.layout.navigationBars
import androidx.compose.foundation.layout.padding
import androidx.compose.foundation.layout.size
import androidx.compose.foundation.layout.statusBars
import androidx.compose.foundation.layout.width
import androidx.compose.foundation.layout.windowInsetsPadding
import androidx.compose.foundation.shape.CircleShape
import androidx.compose.foundation.shape.RoundedCornerShape
import androidx.compose.material3.Icon
import androidx.compose.material3.MaterialTheme
import androidx.compose.material3.SnackbarHost
import androidx.compose.material3.SnackbarHostState
import androidx.compose.material3.Text
import androidx.compose.runtime.Composable
import androidx.compose.runtime.LaunchedEffect
import androidx.compose.runtime.getValue
import androidx.compose.runtime.mutableLongStateOf
import androidx.compose.runtime.remember
import androidx.compose.runtime.setValue
import androidx.compose.ui.Alignment
import androidx.compose.ui.Modifier
import androidx.compose.ui.draw.clip
import androidx.compose.ui.graphics.Color
import androidx.compose.ui.graphics.vector.ImageVector
import androidx.compose.ui.semantics.Role
import androidx.compose.ui.semantics.contentDescription
import androidx.compose.ui.semantics.selected
import androidx.compose.ui.semantics.semantics
import androidx.compose.ui.text.font.FontWeight
import androidx.compose.ui.text.style.TextOverflow
import androidx.compose.ui.unit.dp
import com.restartfu.offload.mobile.AppModel
import com.restartfu.offload.mobile.Link
import com.restartfu.offload.mobile.Page
import com.restartfu.offload.mobile.Tab
import kotlinx.coroutines.delay

/** Room the floating tab bar takes over the foot of a page, so lists can scroll clear of it. */
val BarClearance = 104.dp

fun Tab.icon(): ImageVector = when (this) {
    Tab.OVERVIEW -> Icons.Overview
    Tab.CONTAINERS -> Icons.Containers
    Tab.GITHUB -> Icons.GitHub
    Tab.ACTIVITY -> Icons.Activity
}

/** A clock for "for 3m 12s" and "ago": ticks every second while the app shows. */
@Composable
fun rememberNow(model: AppModel): Long {
    var now by remember { mutableLongStateOf(model.now()) }
    LaunchedEffect(Unit) {
        while (true) {
            delay(1_000)
            now = model.now()
        }
    }
    return now
}

@Composable
fun OffloadApp(model: AppModel, deviceName: String, animate: Boolean = true) {
    if (model.pairing == null) {
        PairScreen(model, deviceName)
        return
    }
    BackHandler(enabled = model.ask != null || model.pages.size > 1 || (model.page as? Page.Home)?.tab != Tab.OVERVIEW) { model.back() }
    val snack = remember { SnackbarHostState() }
    LaunchedEffect(model.notice) {
        val n = model.notice ?: return@LaunchedEffect
        snack.showSnackbar(n)
        model.notice = null
    }
    Box(Modifier.fillMaxSize()) {
        val page = model.page
        AnimatedContent(
            targetState = page,
            transitionSpec = {
                if (!animate) {
                    fadeIn(tween(0)) togetherWith fadeOut(tween(0))
                } else if (initialState is Page.Home && targetState is Page.Home) {
                    fadeIn(tween(180)) togetherWith fadeOut(tween(120))
                } else {
                    val forward = targetState !is Page.Home
                    (slideInHorizontally(tween(240)) { if (forward) it / 6 else -it / 6 } + fadeIn(tween(240))) togetherWith
                        (slideOutHorizontally(tween(200)) { if (forward) -it / 6 else it / 6 } + fadeOut(tween(160)))
                }
            },
            label = "page",
        ) { p ->
            when (p) {
                is Page.Home -> HomePage(model, p.tab)
                is Page.Detail -> WorkloadPage(model, p.runner, p.id)
                Page.AddGitHub -> AddGitHubPage(model)
                Page.Settings -> SettingsPage(model)
            }
        }
        if (page is Page.Home) {
            TabBar(page.tab, onSelect = { model.open(Page.Home(it)) }, modifier = Modifier.align(Alignment.BottomCenter))
        }
        model.ask?.let { ConfirmSheet(it) { model.ask = null } }
        SnackbarHost(
            snack,
            Modifier.align(Alignment.BottomCenter).windowInsetsPadding(WindowInsets.navigationBars)
                .padding(bottom = if (page is Page.Home) 84.dp else 16.dp, start = 12.dp, end = 12.dp),
        )
    }
}

@Composable
private fun HomePage(model: AppModel, tab: Tab) {
    Column(Modifier.fillMaxSize()) {
        Header(
            title = tab.title,
            leading = { Icon(tab.icon(), null, Modifier.padding(start = 16.dp, end = 12.dp).size(22.dp), tint = MaterialTheme.colorScheme.onSurface) },
            model = model,
            actions = { IconAction(Icons.Settings, "Settings", { model.open(Page.Settings) }) },
        )
        Box(Modifier.weight(1f)) {
            when (tab) {
                Tab.OVERVIEW -> OverviewPage(model)
                Tab.CONTAINERS -> ContainersPage(model)
                Tab.GITHUB -> GitHubPage(model)
                Tab.ACTIVITY -> ActivityPage(model)
            }
        }
    }
}

/** A page's one header row: back or the page's icon, its title, the link's dot, then actions. */
@Composable
fun Header(
    title: String,
    model: AppModel,
    leading: @Composable () -> Unit = {},
    onBack: (() -> Unit)? = null,
    subtitle: String? = null,
    actions: @Composable () -> Unit = {},
) {
    val glass = LocalGlass.current
    Column(Modifier.fillMaxWidth().background(glass.pane).windowInsetsPadding(WindowInsets.statusBars)) {
        Row(Modifier.fillMaxWidth().height(60.dp), verticalAlignment = Alignment.CenterVertically) {
            if (onBack != null) {
                Box(Modifier.padding(start = 4.dp)) { IconAction(Icons.Back, "Back", onBack, tint = MaterialTheme.colorScheme.onSurface) }
            } else {
                leading()
            }
            Column(Modifier.weight(1f)) {
                Text(title, style = MaterialTheme.typography.titleLarge.copy(fontWeight = FontWeight.Medium), maxLines = 1, overflow = TextOverflow.Ellipsis)
                if (subtitle != null) {
                    Text(subtitle, style = MaterialTheme.typography.bodySmall, color = MaterialTheme.colorScheme.onSurfaceVariant, maxLines = 1, overflow = TextOverflow.Ellipsis)
                }
            }
            LinkDot(model.link)
            Spacer(Modifier.width(4.dp))
            actions()
            Spacer(Modifier.width(4.dp))
        }
        Box(Modifier.fillMaxWidth().height(1.dp).background(glass.edge))
        ConnectionLine(model)
    }
}

@Composable
private fun LinkDot(link: Link) {
    val s = LocalSignals.current
    val (color, name) = when (link) {
        Link.Live -> s.working to "Connected"
        Link.Connecting -> s.waiting to "Connecting"
        is Link.Down -> s.danger to "Not connected"
    }
    Box(
        Modifier.padding(horizontal = 12.dp).size(10.dp).clip(CircleShape).background(color)
            .semantics { contentDescription = name },
    )
}

/** While the computer isn't answering, one slim line under the header says why and when it tries again. */
@Composable
private fun ConnectionLine(model: AppModel) {
    val link = model.link
    if (link == Link.Live) return
    val now = rememberNow(model)
    val (text, error) = when (link) {
        Link.Connecting -> "Connecting to ${model.pairing?.name ?: "your computer"}…" to false
        is Link.Down -> {
            val left = ((link.retryAt - now) / 1000).coerceAtLeast(0)
            (link.reason + if (link.retryAt == 0L) "" else if (left > 0) " · Trying again in ${left}s" else " · Trying again…") to true
        }
        Link.Live -> "" to false
    }
    val color = if (error) MaterialTheme.colorScheme.error else MaterialTheme.colorScheme.onSurfaceVariant
    Row(
        Modifier.fillMaxWidth()
            .background(if (error) MaterialTheme.colorScheme.errorContainer else Color.Transparent)
            .clickable(enabled = error, role = Role.Button) { model.retry() }
            .padding(horizontal = 16.dp, vertical = 8.dp),
        verticalAlignment = Alignment.CenterVertically,
    ) {
        Text(text, style = MaterialTheme.typography.bodySmall, color = color, modifier = Modifier.weight(1f))
        if (error && link is Link.Down && link.retryAt != 0L) {
            Text("Try again", style = MaterialTheme.typography.labelLarge, color = color, fontWeight = FontWeight.SemiBold, modifier = Modifier.padding(start = 12.dp))
        }
    }
}

/** xd's floating bar: a pill of icon tabs over the foot of the page, opaque so nothing shows through. */
@Composable
private fun TabBar(selected: Tab, onSelect: (Tab) -> Unit, modifier: Modifier = Modifier) {
    val glass = LocalGlass.current
    val shape = RoundedCornerShape(36.dp)
    Row(
        modifier
            .windowInsetsPadding(WindowInsets.navigationBars)
            .padding(bottom = 16.dp)
            .clip(shape)
            .background(glass.overlay)
            .border(1.dp, glass.edge, shape)
            .padding(6.dp),
        horizontalArrangement = Arrangement.spacedBy(4.dp),
    ) {
        for (t in Tab.entries) {
            val on = t == selected
            Box(
                Modifier
                    .size(width = 64.dp, height = 52.dp)
                    .clip(RoundedCornerShape(26.dp))
                    .background(if (on) MaterialTheme.colorScheme.surfaceContainerHighest else Color.Transparent)
                    .let { if (on) it.border(1.dp, MaterialTheme.colorScheme.outline.copy(alpha = 0.5f), RoundedCornerShape(26.dp)) else it }
                    .semantics { contentDescription = t.title; this.selected = on }
                    .clickable(role = Role.Tab) { onSelect(t) },
                contentAlignment = Alignment.Center,
            ) {
                Icon(
                    t.icon(), null, Modifier.size(22.dp),
                    tint = if (on) MaterialTheme.colorScheme.onSurface else MaterialTheme.colorScheme.onSurfaceVariant,
                )
            }
        }
    }
}

/** Padding for a page's scrolling content: the reading measure, and room for the tab bar. */
fun pagePadding(bar: Boolean = true) = PaddingValues(start = 16.dp, end = 16.dp, top = 12.dp, bottom = if (bar) BarClearance else 32.dp)
