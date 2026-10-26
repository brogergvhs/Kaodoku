plugins {
    kotlin("jvm") version "2.2.10"
    application
}

val parsersVersion = "dc223c3d8098415b29cf1acfedc401a88d01c9fe"

repositories {
    mavenCentral()
    google()
    maven("https://jitpack.io")
}

dependencies {
    implementation("com.github.Kotatsu-Redo:kotatsu-parsers-redo:$parsersVersion")
    implementation("com.squareup.okhttp3:okhttp:4.12.0")
    implementation("io.javalin:javalin:6.3.0")
    implementation("com.fasterxml.jackson.module:jackson-module-kotlin:2.17.2")
    implementation("org.slf4j:slf4j-simple:2.0.16")
    implementation("org.jetbrains.kotlinx:kotlinx-coroutines-core:1.9.0")
    runtimeOnly("com.twelvemonkeys.imageio:imageio-webp:3.12.0")
    testImplementation(kotlin("test"))
}

tasks.test { useJUnitPlatform() }

application {
    mainClass = "worker.MainKt"
    applicationDefaultJvmArgs = listOf("-Dworker.lib=$parsersVersion")
}
