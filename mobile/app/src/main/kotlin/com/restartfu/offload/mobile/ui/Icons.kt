package com.restartfu.offload.mobile.ui

import androidx.compose.ui.graphics.Color
import androidx.compose.ui.graphics.SolidColor
import androidx.compose.ui.graphics.StrokeCap
import androidx.compose.ui.graphics.StrokeJoin
import androidx.compose.ui.graphics.vector.ImageVector
import androidx.compose.ui.graphics.vector.addPathNodes
import androidx.compose.ui.unit.dp

/** Line icons on a 24-unit grid, stroked like xd's (lucide). Color comes from the Icon's tint. */
private fun line(name: String, vararg paths: String, fill: Boolean = false) = ImageVector.Builder(name, 24.dp, 24.dp, 24f, 24f).apply {
    for (d in paths) {
        addPath(
            pathData = addPathNodes(d),
            fill = if (fill) SolidColor(Color.Black) else null,
            stroke = if (fill) null else SolidColor(Color.Black),
            strokeLineWidth = 2f,
            strokeLineCap = StrokeCap.Round,
            strokeLineJoin = StrokeJoin.Round,
        )
    }
}.build()

object Icons {
    val Overview = line("overview", "M4 4h6v6H4z", "M14 4h6v6h-6z", "M14 14h6v6h-6z", "M4 14h6v6H4z")
    val Containers = line(
        "containers",
        "M21 8a2 2 0 0 0 -1 -1.73l-7 -4a2 2 0 0 0 -2 0l-7 4A2 2 0 0 0 3 8v8a2 2 0 0 0 1 1.73l7 4a2 2 0 0 0 2 0l7 -4A2 2 0 0 0 21 16Z",
        "M3.3 7 12 12l8.7 -5", "M12 22V12",
    )
    val GitHub = line(
        "github",
        "M15 22v-4a4.8 4.8 0 0 0 -1 -3.5c3 0 6 -2 6 -5.5c0.08 -1.25 -0.27 -2.48 -1 -3.5c0.28 -1.15 0.28 -2.35 0 -3.5c0 0 -1 0 -3 1.5c-2.64 -0.5 -5.36 -0.5 -8 0C6 2 5 2 5 2c-0.3 1.15 -0.3 2.35 0 3.5A5.4 5.4 0 0 0 4 9c0 3.5 3 5.5 6 5.5c-0.39 0.49 -0.68 1.05 -0.85 1.65c-0.17 0.6 -0.22 1.23 -0.15 1.85v4",
        "M9 18c-4.51 2 -5 -2 -7 -2",
    )
    val Activity = line("activity", "M3 3v18h18", "M7 15l4 -4 3 3 6 -7")
    val Settings = line(
        "settings",
        "M12.22 2h-0.44a2 2 0 0 0 -2 2v0.18a2 2 0 0 1 -1 1.73l-0.43 0.25a2 2 0 0 1 -2 0l-0.15 -0.08a2 2 0 0 0 -2.73 0.73l-0.22 0.38a2 2 0 0 0 0.73 2.73l0.15 0.1a2 2 0 0 1 1 1.72v0.51a2 2 0 0 1 -1 1.74l-0.15 0.09a2 2 0 0 0 -0.73 2.73l0.22 0.38a2 2 0 0 0 2.73 0.73l0.15 -0.08a2 2 0 0 1 2 0l0.43 0.25a2 2 0 0 1 1 1.73V20a2 2 0 0 0 2 2h0.44a2 2 0 0 0 2 -2v-0.18a2 2 0 0 1 1 -1.73l0.43 -0.25a2 2 0 0 1 2 0l0.15 0.08a2 2 0 0 0 2.73 -0.73l0.22 -0.39a2 2 0 0 0 -0.73 -2.73l-0.15 -0.08a2 2 0 0 1 -1 -1.74v-0.5a2 2 0 0 1 1 -1.74l0.15 -0.09a2 2 0 0 0 0.73 -2.73l-0.22 -0.38a2 2 0 0 0 -2.73 -0.73l-0.15 0.08a2 2 0 0 1 -2 0l-0.43 -0.25a2 2 0 0 1 -1 -1.73V4a2 2 0 0 0 -2 -2z",
        "M12 9a3 3 0 1 0 0 6a3 3 0 1 0 0 -6",
    )
    val Back = line("back", "M15 18l-6 -6 6 -6")
    val Chevron = line("chevron", "M9 18l6 -6 -6 -6")
    val Pause = line("pause", "M9 5v14", "M15 5v14")
    val Play = line("play", "M7 4.5v15l12 -7.5z")
    val Close = line("close", "M18 6 6 18", "M6 6l12 12")
    val Copy = line("copy", "M9 9h11v11H9z", "M5 15H4V4h11v1")
    val Check = line("check", "M20 6 9 17l-5 -5")
    val External = line("external", "M15 3h6v6", "M10 14 21 3", "M18 13v6a2 2 0 0 1 -2 2H5a2 2 0 0 1 -2 -2V8a2 2 0 0 1 2 -2h6")
    val Plus = line("plus", "M12 5v14", "M5 12h14")
    val Retry = line("retry", "M21 12a9 9 0 1 1 -3 -6.7L21 8", "M21 3v5h-5")
    val Terminal = line("terminal", "M4 17l6 -6 -6 -6", "M12 19h8")
    val Phone = line("phone", "M7 2h10a2 2 0 0 1 2 2v16a2 2 0 0 1 -2 2H7a2 2 0 0 1 -2 -2V4a2 2 0 0 1 2 -2z", "M11 18h2")
    val Laptop = line("laptop", "M4 5h16v11H4z", "M2 20h20")
    val Alert = line("alert", "M12 3 2 20h20z", "M12 10v4", "M12 17h0.01")
}
