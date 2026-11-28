// Runners share one address, which Maven Central rate-limits (HTTP 429), so Google's mirror of
// it is asked first.
val centralMirror = "https://maven-central.storage-download.googleapis.com/maven2"

pluginManagement {
    repositories {
        google()
        maven("https://maven-central.storage-download.googleapis.com/maven2")
        mavenCentral()
        gradlePluginPortal()
    }
}

dependencyResolutionManagement {
    repositoriesMode.set(RepositoriesMode.FAIL_ON_PROJECT_REPOS)
    repositories {
        google()
        maven(centralMirror)
        mavenCentral()
    }
}

rootProject.name = "offload-mobile"

include(":app")
